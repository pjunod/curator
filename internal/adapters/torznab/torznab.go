// Package torznab implements ports.Indexer for Newznab AND Torznab — the
// protocols are near-identical XML APIs (blueprint §5), so one adapter
// reaches every indexer Prowlarr or Jackett can proxy.
package torznab

import (
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/adapters/httpx"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// Client speaks the Newznab/Torznab API for one configured indexer.
type Client struct {
	cfg  ports.IndexerConfig
	http *http.Client
}

var _ ports.Indexer = (*Client)(nil)
var _ ports.IndexerCapabilitiesProvider = (*Client)(nil)

const capabilitiesTTL = 24 * time.Hour

type capabilityCacheEntry struct {
	caps       ports.IndexerCapabilities
	err        error
	retryAfter time.Time
	ready      chan struct{}
}

var capabilityCache = struct {
	sync.Mutex
	entries map[string]*capabilityCacheEntry
}{entries: map[string]*capabilityCacheEntry{}}

// New returns a Client for the given config.
func New(cfg ports.IndexerConfig) *Client {
	cfg.URL = ports.NormalizeURL(cfg.URL)
	return &Client{cfg: cfg, http: httpx.NewClient(30 * time.Second)}
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
	Comments  string `xml:"comments"`
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

type capsDocument struct {
	XMLName   xml.Name `xml:"caps"`
	Searching struct {
		Search struct {
			Available       string `xml:"available,attr"`
			SupportedParams string `xml:"supportedParams,attr"`
		} `xml:"search"`
		TV struct {
			Available       string `xml:"available,attr"`
			SupportedParams string `xml:"supportedParams,attr"`
		} `xml:"tv-search"`
		Movie struct {
			Available       string `xml:"available,attr"`
			SupportedParams string `xml:"supportedParams,attr"`
		} `xml:"movie-search"`
	} `xml:"searching"`
}

func cacheKey(cfg ports.IndexerConfig) string {
	// Including a one-way credential digest prevents stale capabilities from
	// crossing a config edit without retaining or exposing the credential.
	sum := sha256.Sum256([]byte(cfg.APIKey))
	return fmt.Sprintf("%d|%s|%x|%v", cfg.ID, cfg.URL, sum[:8], cfg.Categories)
}

func capability(rawAvailable, rawParams string) ports.IndexerSearchCapability {
	available := strings.ToLower(strings.TrimSpace(rawAvailable))
	known := available == "yes" || available == "no"
	out := ports.IndexerSearchCapability{Known: known, Available: available == "yes", Parameters: map[string]bool{}}
	for _, value := range strings.Split(rawParams, ",") {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			out.Parameters[value] = true
		}
	}
	return out
}

// Capabilities returns a process-shared, configuration-keyed snapshot.
func (c *Client) Capabilities(ctx context.Context) (ports.IndexerCapabilities, error) {
	return c.capabilities(ctx, false)
}

func (c *Client) InvalidateSearchCapability(mode string) {
	key := cacheKey(c.cfg)
	capabilityCache.Lock()
	defer capabilityCache.Unlock()
	entry := capabilityCache.entries[key]
	if entry == nil || entry.ready != nil || entry.err != nil {
		return
	}
	switch mode {
	case "tv":
		entry.caps.TV.Known, entry.caps.TV.Available = true, false
	case "movie":
		entry.caps.Movie.Known, entry.caps.Movie.Available = true, false
	case "generic":
		entry.caps.Generic.Known, entry.caps.Generic.Available = true, false
	}
}

func (c *Client) capabilities(ctx context.Context, force bool) (ports.IndexerCapabilities, error) {
	key := cacheKey(c.cfg)
	capabilityCache.Lock()
	if entry := capabilityCache.entries[key]; entry != nil && !force {
		if entry.ready != nil {
			ready := entry.ready
			capabilityCache.Unlock()
			select {
			case <-ready:
				return c.capabilities(ctx, false)
			case <-ctx.Done():
				return ports.IndexerCapabilities{}, ctx.Err()
			}
		}
		if entry.err != nil {
			if time.Now().Before(entry.retryAfter) {
				caps, err := entry.caps, entry.err
				capabilityCache.Unlock()
				return caps, err
			}
		}
		if time.Since(entry.caps.FetchedAt) < capabilitiesTTL {
			caps, err := entry.caps, entry.err
			capabilityCache.Unlock()
			return caps, err
		}
	}
	previous := capabilityCache.entries[key]
	pending := &capabilityCacheEntry{ready: make(chan struct{})}
	capabilityCache.entries[key] = pending
	capabilityCache.Unlock()

	body, err := c.call(ctx, url.Values{"t": {"caps"}})
	var caps ports.IndexerCapabilities
	if err == nil {
		var doc capsDocument
		if decodeErr := xml.Unmarshal(body, &doc); decodeErr != nil || doc.XMLName.Local != "caps" {
			err = &ports.RemoteError{Category: ports.RemoteInvalidResponse, Cause: errors.New("capabilities XML was not recognized")}
		} else {
			caps = ports.IndexerCapabilities{
				Generic:   capability(doc.Searching.Search.Available, doc.Searching.Search.SupportedParams),
				TV:        capability(doc.Searching.TV.Available, doc.Searching.TV.SupportedParams),
				Movie:     capability(doc.Searching.Movie.Available, doc.Searching.Movie.SupportedParams),
				FetchedAt: time.Now(),
			}
		}
	}
	if err != nil && previous != nil && previous.ready == nil && previous.err == nil && !previous.caps.FetchedAt.IsZero() && !capabilityFatal(err) {
		caps = previous.caps
		caps.Degraded = true
		err = nil
	}
	capabilityCache.Lock()
	ready := pending.ready
	if err != nil && !capabilityFatal(err) {
		delete(capabilityCache.entries, key)
	} else {
		pending.caps, pending.err = caps, err
		pending.retryAfter = capabilityRetryAfter(err)
		pending.ready = nil
	}
	capabilityCache.Unlock()
	close(ready)
	if err != nil && !capabilityFatal(err) {
		return ports.IndexerCapabilities{Degraded: true}, nil
	}
	return caps, err
}

func capabilityRetryAfter(err error) time.Time {
	if err == nil {
		return time.Time{}
	}
	var remote *ports.RemoteError
	if errors.As(err, &remote) && !remote.RetryAt.IsZero() {
		return remote.RetryAt
	}
	return time.Now().Add(time.Minute)
}

func capabilityFatal(err error) bool {
	var remote *ports.RemoteError
	return errors.As(err, &remote) && (remote.Category == ports.RemoteAuth || remote.Category == ports.RemoteRateLimit)
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
		return nil, &ports.RemoteError{Category: ports.RemoteTransport, Cause: errors.New("indexer request failed")}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		category := ports.RemoteTransport
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			category = ports.RemoteAuth
		case http.StatusNotFound:
			category = ports.RemoteNotFound
		case http.StatusTooManyRequests:
			category = ports.RemoteRateLimit
		}
		return nil, &ports.RemoteError{Category: category, HTTPStatus: resp.StatusCode, RetryAt: retryAt(resp.Header.Get("Retry-After"))}
	}
	// Newznab reports errors as XML with code/description attributes.
	if strings.Contains(string(body[:min(len(body), 200)]), "<error") {
		var e struct {
			Code string `xml:"code,attr"`
			Desc string `xml:"description,attr"`
		}
		if xml.Unmarshal(body, &e) == nil && e.Code != "" {
			category := ports.RemoteInvalidResponse
			switch e.Code {
			case "100", "101", "102":
				category = ports.RemoteAuth
			case "200", "201", "202", "203", "910":
				category = ports.RemoteUnsupportedQuery
			case "429":
				category = ports.RemoteRateLimit
			}
			return nil, &ports.RemoteError{Category: category, HTTPStatus: resp.StatusCode, ProtocolCode: e.Code}
		}
	}
	return body, nil
}

func retryAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	parsed, _ := http.ParseTime(raw)
	return parsed
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
	mode := q.Mode
	if mode == "" {
		if q.Season > 0 {
			mode = "tv"
		} else {
			mode = "generic"
		}
	}
	switch mode {
	case "tv":
		params.Set("t", "tvsearch")
		if q.SeasonSet || q.Season > 0 {
			params.Set("season", strconv.Itoa(q.Season))
		}
		if q.EpisodeSet || q.Episode > 0 {
			params.Set("ep", strconv.Itoa(q.Episode))
		}
	case "movie":
		params.Set("t", "movie")
	default:
		params.Set("t", "search")
	}
	if q.ID != nil {
		value := q.ID.Value
		name := q.ID.Provider + "id"
		if mode == "movie" && q.ID.Provider == "imdb" {
			value = strings.TrimPrefix(value, "tt")
		}
		params.Set(name, value)
	} else {
		params.Set("q", q.Q)
	}
	cats := c.cfg.Categories
	if len(cats) == 0 && q.Kind == domain.KindBook {
		switch q.BookType {
		case quality.BookTypeEbook:
			cats = []int{7000, 7020} // Books, Ebook
		case quality.BookTypeAudiobook:
			cats = []int{3030} // Audio/Audiobook
		default:
			cats = []int{7000, 7020, 3030} // broad legacy book search
		}
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
			GUID:      strings.TrimSpace(it.GUID),
			Title:     strings.TrimSpace(it.Title),
			Size:      it.Size,
			Indexer:   c.cfg.Name,
			IndexerID: c.cfg.ID,
			Protocol:  c.cfg.Protocol,
			InfoURL:   infoURL(it.Comments, it.GUID),
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
		r.IDs, r.IDIssues = releaseIdentities(it.Attrs)
		if r.Title != "" && r.DownloadURL != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func releaseIdentities(attrs []struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}) (domain.ExternalIDs, []domain.IdentityIssue) {
	values := map[string][]string{}
	for _, attr := range attrs {
		name := strings.ToLower(strings.TrimSpace(attr.Name))
		switch name {
		case "tvdb", "tvdbid":
			name = "tvdb"
		case "tmdb", "tmdbid":
			name = "tmdb"
		case "imdb", "imdbid":
			name = "imdb"
		default:
			continue
		}
		values[name] = append(values[name], strings.TrimSpace(attr.Value))
	}
	var ids domain.ExternalIDs
	var issues []domain.IdentityIssue
	for provider, raw := range values {
		unique := map[string]bool{}
		var valid []string
		for _, value := range raw {
			canonical, ok := canonicalReleaseID(provider, value)
			if !ok {
				issues = append(issues, domain.IdentityIssue{Code: "malformed_id", Provider: provider, Values: []string{value}})
				continue
			}
			if !unique[canonical] {
				unique[canonical] = true
				valid = append(valid, canonical)
			}
		}
		if len(valid) > 1 {
			issues = append(issues, domain.IdentityIssue{Code: "conflicting_ids", Provider: provider, Values: valid})
			continue
		}
		if len(valid) == 0 {
			continue
		}
		switch provider {
		case "imdb":
			ids.IMDB = valid[0]
		case "tvdb":
			ids.TVDB, _ = strconv.ParseInt(valid[0], 10, 64)
		case "tmdb":
			ids.TMDB, _ = strconv.ParseInt(valid[0], 10, 64)
		}
	}
	return ids, issues
}

func canonicalReleaseID(provider, value string) (string, bool) {
	value = strings.TrimSpace(value)
	if provider == "imdb" {
		value = strings.ToLower(value)
		value = strings.TrimPrefix(value, "tt")
		if len(value) < 7 || len(value) > 12 {
			return "", false
		}
		for _, r := range value {
			if r < '0' || r > '9' {
				return "", false
			}
		}
		return "tt" + value, true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

// infoURL picks the release's human details page: <comments> is the
// canonical Newznab field, <guid> is usually a permalink too — but only
// when it actually is a URL (isPermaLink="false" feeds carry opaque ids).
func infoURL(comments, guid string) string {
	for _, cand := range []string{strings.TrimSpace(comments), strings.TrimSpace(guid)} {
		if strings.HasPrefix(cand, "http://") || strings.HasPrefix(cand, "https://") {
			return cand
		}
	}
	return ""
}

// Test implements ports.Indexer via t=caps, which every implementation
// answers without burning API hits.
func (c *Client) Test(ctx context.Context) error {
	_, err := c.capabilities(ctx, true)
	return err
}
