package dekkoo

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/net/html"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// Metadata is the same keyed TMDB client that supplies the other Discover rows.
// Search is shallow: resolving a show must not fetch all of its episodes.
type Metadata interface {
	Configured(context.Context) bool
	SearchMovies(context.Context, string) ([]ports.SearchResult, error)
	SearchSeries(context.Context, string) ([]ports.SearchResult, error)
}

var _ ports.DiscoverProvider = (*Client)(nil)

func (c *Client) Name() string { return "dekkoo" }
func (c *Client) Configured(ctx context.Context) bool {
	return c.metadata != nil && c.metadata.Configured(ctx)
}
func (c *Client) Lists() []ports.DiscoverList {
	return []ports.DiscoverList{
		{ID: "dekkoo-movies", Title: "Gay movies on Dekkoo", Kind: domain.KindMovie, Source: c.Name(), Blurb: "Recent movies and shorts from Dekkoo’s five gay-themed feeds, matched to TMDB."},
		{ID: "dekkoo-series", Title: "Gay shows on Dekkoo", Kind: domain.KindSeries, Source: c.Name(), Blurb: "Recent shows from the same combined Dekkoo feeds, matched to TMDB."},
	}
}

func (c *Client) Discover(ctx context.Context, listID string, page int) ([]ports.SearchResult, error) {
	var kind domain.MediaKind
	switch listID {
	case "dekkoo-movies":
		kind = domain.KindMovie
	case "dekkoo-series":
		kind = domain.KindSeries
	default:
		return nil, fmt.Errorf("dekkoo: %w: %s", ports.ErrUnknownList, listID)
	}
	if !c.Configured(ctx) {
		return nil, ports.ErrProviderNotConfigured
	}
	if page > 1 {
		return []ports.SearchResult{}, nil
	}
	// Bound the whole row as well as each feed request. Search fanout is small
	// and shares TMDB's existing rate limiter with the rest of the application.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	feed, err := c.feed(ctx)
	if err != nil {
		return nil, err
	}
	candidates := []candidate{}
	seen := map[string]bool{}
	for _, article := range feed.Items {
		for _, title := range titles(article) {
			key := normalized(title.title) + ":" + strconv.Itoa(title.year)
			if title.kind != kind || seen[key] {
				continue
			}
			seen[key] = true
			candidates = append(candidates, title)
			if len(candidates) == 50 {
				break
			}
		}
		if len(candidates) == 50 {
			break
		}
	}
	results := make([]ports.SearchResult, len(candidates))
	errors := make([]error, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, title := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errors[i] = ctx.Err()
				return
			}
			var matches []ports.SearchResult
			var err error
			if kind == domain.KindSeries {
				matches, err = c.metadata.SearchSeries(ctx, title.title)
			} else {
				matches, err = c.metadata.SearchMovies(ctx, title.title)
			}
			if err != nil {
				errors[i] = err
				return
			}
			results[i] = resolve(title, matches)
		}()
	}
	wg.Wait()
	// Do not cache a partly empty row as a successful refresh during an outage.
	// Returning the error lets the shared Discover cache serve its last good row.
	for _, err := range errors {
		if err != nil {
			return nil, fmt.Errorf("dekkoo title lookup: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []ports.SearchResult{}
	ids := map[int64]bool{}
	for _, result := range results {
		if result.TMDBID == 0 || ids[result.TMDBID] {
			continue
		}
		ids[result.TMDBID] = true
		result.Source = c.Name()
		out = append(out, result)
	}
	return out, nil
}

type candidate struct {
	title string
	kind  domain.MediaKind
	year  int
}

var (
	collectionTitle = regexp.MustCompile(`(?i)^\d+\s+.*\b(movies|films|thrillers)\b`)
	seasonSuffix    = regexp.MustCompile(`(?i)\s+(?:with\s+)?season\s+\d+$`)
	yearSuffix      = regexp.MustCompile(`\s*\((19\d\d|20\d\d)\)$`)
	numberPrefix    = regexp.MustCompile(`^\d+[.)]\s+`)
)

// titles reads title-bearing catalog links and collection headings from RSS
// content. It never guesses a media identity from an entire blog headline,
// executes publisher HTML, or follows a publisher-supplied URL.
func titles(article feedArticle) []candidate {
	root, err := html.Parse(strings.NewReader(article.Content))
	if err != nil {
		return nil
	}
	kind := domain.KindMovie
	if slices.Contains(article.Categories, "Gay Series") {
		kind = domain.KindSeries
	}
	collection := collectionTitle.MatchString(article.Title)
	out := []candidate{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key != "href" {
					continue
				}
				u, err := url.Parse(attr.Val)
				if err != nil || u.Scheme != "https" || u.Host != "watch.dekkoo.com" || u.User != nil {
					continue
				}
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				if len(parts) > 2 || (len(parts) == 2 && !strings.HasPrefix(parts[1], "season:")) {
					continue
				}
				slug := parts[0]
				switch slug {
				case "", "movies", "genres", "episodic-series", "dekkoo-originals", "dekkoo-originals-exclusives":
					continue
				}
				if strings.HasPrefix(slug, "dekkoo-selects-") {
					continue
				}
				title := cleanTitle(nodeText(n))
				// This also rejects split links saying only "Watch" or "on Dekkoo",
				// trailers, and navigation links whose labels are generic advertising.
				if normalized(title.title) != normalized(strings.ReplaceAll(slug, "-", " ")) {
					continue
				}
				title.kind = kind
				if len(parts) == 2 {
					title.kind = domain.KindSeries
				}
				if title.title != "" {
					out = append(out, title)
				}
			}
		}
		if collection && n.Type == html.ElementNode && n.Data == "h2" {
			title := cleanTitle(numberPrefix.ReplaceAllString(nodeText(n), ""))
			title.kind = kind
			if title.title != "" {
				out = append(out, title)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return out
}

func nodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(nodeText(child))
	}
	return b.String()
}

func cleanTitle(text string) candidate {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.Trim(text, " .→!\t\n")
	lower := strings.ToLower(text)
	for _, prefix := range []string{"start watching ", "where to watch ", "watch ", "stream ", "start "} {
		if strings.HasPrefix(lower, prefix) {
			text = text[len(prefix):]
			break
		}
	}
	for _, suffix := range []string{" now on dekkoo", " on dekkoo", " now"} {
		if strings.HasSuffix(strings.ToLower(text), suffix) {
			text = text[:len(text)-len(suffix)]
			break
		}
	}
	text = seasonSuffix.ReplaceAllString(text, "")
	out := candidate{}
	if year := yearSuffix.FindStringSubmatch(text); len(year) > 0 {
		out.year, _ = strconv.Atoi(year[1])
		text = yearSuffix.ReplaceAllString(text, "")
	}
	if len([]rune(text)) <= 200 {
		out.title = strings.TrimSpace(text)
	}
	return out
}

func normalized(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// An exact primary/original title and optional explicit release year must
// identify one TMDB work. Popularity order cannot disambiguate a namesake.
func resolve(title candidate, matches []ports.SearchResult) ports.SearchResult {
	var found ports.SearchResult
	for _, match := range matches {
		if match.Kind != title.kind || match.TMDBID <= 0 || (title.year != 0 && match.Year != title.year) {
			continue
		}
		names := append([]string{match.Title}, match.AltTitles...)
		exact := false
		for _, name := range names {
			if normalized(name) == normalized(title.title) {
				exact = true
				break
			}
		}
		if !exact {
			continue
		}
		if found.TMDBID != 0 && found.TMDBID != match.TMDBID {
			return ports.SearchResult{}
		}
		found = match
	}
	return found
}
