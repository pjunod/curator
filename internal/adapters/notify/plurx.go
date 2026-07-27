package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

// Plurx tells a plurx server to index exactly the files that just landed.
//
// The difference from the Plex and Jellyfin notifiers next door is the whole
// reason this exists. Those say "something changed, go and look", and the
// server sweeps an entire library to find one new file — then identifies it
// by searching for its filename, which is the step that puts the 2015
// remake's poster on the 1995 film. This says "index /media/movies/Heat
// (1995)/Heat (1995) [Bluray-1080p].mkv, it is tmdb 949". One folder, and
// nothing left to guess at.
//
// It holds a scoped key (`plx_…`, scope `scan:trigger`), never a plurx admin
// token. That is not a detail: a plurx user token IS that user, so an admin
// token handed over here would also hand Monarr — and anything that reads
// Monarr's database — every secret in plurx's settings.
type Plurx struct {
	URL    string
	APIKey string
}

// plurxAttempts bounds the retry. The dispatcher gives each notifier 20
// seconds for the whole event, and a season pack is a dozen paths, so this
// buys through a restart or a blocked moment without ever being the thing
// that makes an import hang.
const plurxAttempts = 3

// Send implements ports.Notifier.
func (p *Plurx) Send(ctx context.Context, n ports.Notification) error {
	if p.URL == "" || p.APIKey == "" {
		return fmt.Errorf("plurx: url and apiKey required")
	}
	// An event with nothing to index is not a failure — it is a chat-shaped
	// event arriving at a notifier that only speaks about files. Saying
	// nothing is the correct response; reporting an error would light up
	// the health page for a grab notification.
	if n.Import == nil || len(n.Import.Paths) == 0 {
		return nil
	}

	var failures []string
	for _, path := range n.Import.Paths {
		if err := p.scan(ctx, path, n.Import); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) == 0 {
		return nil
	}
	// Partial delivery is worth naming as such: the difference between "one
	// file of a season pack was rejected" and "plurx is down" is the first
	// thing anyone reading this wants to know.
	return fmt.Errorf("plurx: %d of %d paths not indexed: %s",
		len(failures), len(n.Import.Paths), strings.Join(failures, "; "))
}

type plurxScanRequest struct {
	Path          string    `json:"path"`
	IDs           *plurxIDs `json:"ids,omitempty"`
	Hint          string    `json:"hint,omitempty"`
	Series        *plurxIDs `json:"series,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Source        string    `json:"source"`
}

type plurxIDs struct {
	TMDB int64  `json:"tmdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
}

// scanBody builds the request for one path.
//
// Where the ids go depends on what the path is. For an episode, plurx wants
// the SHOW's id under `series` — an episode's own TMDB id is not what
// identifies the series it belongs to, and putting the show's id in `ids`
// would stamp it on the episode row.
func scanBody(path string, info *ports.ImportInfo) plurxScanRequest {
	req := plurxScanRequest{Path: path, CorrelationID: info.Transfer, Source: "monarr"}
	ids := &plurxIDs{TMDB: info.TMDBID, IMDB: info.IMDBID}
	if ids.TMDB == 0 && ids.IMDB == "" {
		ids = nil
	}
	if info.Episode {
		req.Hint = "episode"
		// Only the TMDB id belongs to the show; an IMDb id on a series row
		// is the series', but plurx keys episodes off the series TMDB id
		// alone, so sending more would be inventing precision.
		if info.TMDBID != 0 {
			req.Series = &plurxIDs{TMDB: info.TMDBID}
		}
		return req
	}
	req.Hint = "movie"
	req.IDs = ids
	return req
}

// scan posts one path, retrying only what retrying can fix.
func (p *Plurx) scan(ctx context.Context, path string, info *ports.ImportInfo) error {
	body, err := json.Marshal(scanBody(path, info))
	if err != nil {
		return err
	}
	target := strings.TrimRight(p.URL, "/") + "/api/v1/scan"

	var last error
	for attempt := 1; attempt <= plurxAttempts; attempt++ {
		if attempt > 1 {
			// Short and fixed rather than exponential: the budget for the
			// whole event is 20 s and this is one path of possibly many.
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: %w", path, ctx.Err())
			case <-time.After(time.Duration(attempt-1) * 400 * time.Millisecond):
			}
		}
		err := p.post(ctx, target, body)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%s: %w", path, err)
		// A rejected request is rejected however many times it is sent. A
		// bad path, a key without the scope, a revoked key — retrying those
		// only delays the moment somebody reads the message.
		if !errors.Is(err, errPlurxRetryable) {
			return last
		}
	}
	return last
}

// errPlurxRetryable marks the failures where trying again is the right move:
// the connection, and the server saying it is having a moment.
var errPlurxRetryable = errors.New("retryable")

func (p *Plurx) post(ctx context.Context, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s", errPlurxRetryable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		// 202 means plurx was mid-scan and queued the request. That is a
		// success: it is queued, not dropped, and treating it as a failure
		// would retry work plurx has already promised to do.
		return nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		// The 422 carries plurx's library roots. Carrying that message
		// through verbatim is the difference between "422" and being able
		// to see, in Monarr's own log, that plurx has /media mounted where
		// Monarr has /data/media.
		return fmt.Errorf("plurx does not have this path under any library root — %s",
			summarize(raw))
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("plurx rejected the key (401) — it is unknown, revoked, "+
			"or a plurx USER token, which this route does not accept: %s", summarize(raw))
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("the key is valid but lacks the scan:trigger scope (403): %s",
			summarize(raw))
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: plurx returned %d: %s", errPlurxRetryable, resp.StatusCode, summarize(raw))
	default:
		return fmt.Errorf("plurx returned %d: %s", resp.StatusCode, summarize(raw))
	}
}

// summarize trims a response body down to something a log line can hold.
func summarize(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(no body)"
	}
	return s
}

// Test implements ports.Notifier.
//
// It asks plurx to scan a path that cannot exist under any library root, and
// counts the 422 as the pass. That is deliberate: it exercises the URL, the
// key, and the scope — everything that can be misconfigured — and does not
// start a scan of anything real while doing it. A test that triggered work
// would be a test people learn not to press.
func (p *Plurx) Test(ctx context.Context) error {
	if p.URL == "" || p.APIKey == "" {
		return fmt.Errorf("plurx: url and apiKey required")
	}
	body, err := json.Marshal(plurxScanRequest{
		Path:   "/monarr/connection-test",
		Source: "monarr",
	})
	if err != nil {
		return err
	}
	err = p.post(ctx, strings.TrimRight(p.URL, "/")+"/api/v1/scan", body)
	if err == nil {
		return nil // it somehow matched a root; the connection works either way
	}
	if strings.Contains(err.Error(), "under any library root") {
		return nil
	}
	return err
}
