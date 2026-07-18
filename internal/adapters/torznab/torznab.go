// Package torznab implements ports.Indexer for Newznab AND Torznab — the
// protocols are near-identical XML APIs (blueprint §5), so one adapter
// reaches every indexer Prowlarr or Jackett can proxy.
package torznab

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// Client speaks the Newznab/Torznab API for one configured indexer.
type Client struct {
	cfg  ports.IndexerConfig
	http *http.Client
}

var _ ports.Indexer = (*Client)(nil)

// New returns a Client for the given config.
func New(cfg ports.IndexerConfig) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

// rss is the subset of the feed we consume.
type rss struct {
	Channel struct {
		Items []item `xml:"item"`
	} `xml:"channel"`
}

type item struct {
	Title     string `xml:"title"`
	Link      string `xml:"link"`
	GUID      string `xml:"guid"`
	PubDate   string `xml:"pubDate"`
	Size      int64  `xml:"size"`
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Length int64  `xml:"length,attr"`
	} `xml:"enclosure"`
	// torznab:attr / newznab:attr name-value pairs.
	Attrs []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
}

func (c *Client) call(ctx context.Context, params url.Values) ([]byte, error) {
	params.Set("apikey", c.cfg.APIKey)
	full := strings.TrimRight(c.cfg.URL, "/") + "/api?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("indexer %s: %w", c.cfg.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("indexer %s: status %d", c.cfg.Name, resp.StatusCode)
	}
	// Newznab reports errors as XML with code/description attributes.
	if strings.Contains(string(body[:min(len(body), 200)]), "<error") {
		var e struct {
			Code string `xml:"code,attr"`
			Desc string `xml:"description,attr"`
		}
		if xml.Unmarshal(body, &e) == nil && e.Code != "" {
			return nil, fmt.Errorf("indexer %s: error %s: %s", c.cfg.Name, e.Code, e.Desc)
		}
	}
	return body, nil
}

// FetchRSS implements ports.Indexer: an empty-query t=search returns the
// indexer's most recent releases (the standard Torznab RSS mode).
func (c *Client) FetchRSS(ctx context.Context) ([]ports.Release, error) {
	return c.Search(ctx, domain.SearchQuery{})
}

// Search implements ports.Indexer. TV queries use t=tvsearch with
// season/ep parameters; everything else is a plain t=search. Book queries
// (ADR 0006) pin the Newznab book categories — 7000s for ebooks plus 3030
// for audiobooks — unless the indexer config narrows them.
func (c *Client) Search(ctx context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	params := url.Values{}
	if q.Season > 0 {
		params.Set("t", "tvsearch")
		params.Set("season", strconv.Itoa(q.Season))
		if q.Episode > 0 {
			params.Set("ep", strconv.Itoa(q.Episode))
		}
	} else {
		params.Set("t", "search")
	}
	params.Set("q", q.Q)
	cats := c.cfg.Categories
	if len(cats) == 0 && q.Kind == domain.KindBook {
		cats = []int{7000, 7020, 3030} // Books, Ebook, Audio/Audiobook
	}
	if len(cats) > 0 {
		strs := make([]string, len(cats))
		for i, cat := range cats {
			strs[i] = strconv.Itoa(cat)
		}
		params.Set("cat", strings.Join(strs, ","))
	}

	body, err := c.call(ctx, params)
	if err != nil {
		return nil, err
	}
	var feed rss
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("indexer %s: bad XML: %w", c.cfg.Name, err)
	}

	out := make([]ports.Release, 0, len(feed.Channel.Items))
	for _, it := range feed.Channel.Items {
		r := ports.Release{
			Title:     strings.TrimSpace(it.Title),
			Size:      it.Size,
			Indexer:   c.cfg.Name,
			IndexerID: c.cfg.ID,
			Protocol:  c.cfg.Protocol,
			InfoURL:   it.GUID,
		}
		if r.DownloadURL = it.Enclosure.URL; r.DownloadURL == "" {
			r.DownloadURL = it.Link
		}
		if r.Size == 0 {
			r.Size = it.Enclosure.Length
		}
		if t, err := time.Parse(time.RFC1123Z, it.PubDate); err == nil {
			r.PublishDate = t
		} else if t, err := time.Parse(time.RFC1123, it.PubDate); err == nil {
			r.PublishDate = t
		}
		for _, a := range it.Attrs {
			v, _ := strconv.Atoi(a.Value)
			switch a.Name {
			case "seeders":
				r.Seeders = v
			case "peers", "leechers":
				r.Peers = v
			case "size":
				if r.Size == 0 {
					r.Size, _ = strconv.ParseInt(a.Value, 10, 64)
				}
			}
		}
		if r.Title != "" && r.DownloadURL != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// Test implements ports.Indexer via t=caps, which every implementation
// answers without burning API hits.
func (c *Client) Test(ctx context.Context) error {
	body, err := c.call(ctx, url.Values{"t": {"caps"}})
	if err != nil {
		return err
	}
	if !strings.Contains(string(body), "<caps") {
		return fmt.Errorf("indexer %s: caps response not recognized", c.cfg.Name)
	}
	return nil
}
