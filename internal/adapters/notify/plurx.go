package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain/backoff"
	"github.com/pjunod/monarr/internal/ports"
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
	// Wait is how long Cinema may hold a sub-delivery for its result
	// (wait_ms); 0 means the default, 10 s.
	Wait time.Duration
	// Features is what Cinema's connection test said it supports, as stored
	// on the notifier ("removed_paths", contract amendment 5.3).
	Features []string

	// learned is what this Test's answer said, for the caller to store.
	learned []string

	// delivery accumulates what plurx said about the last Send. A notifier
	// is built fresh per dispatch (see notify.New), so this is per-event
	// state and not something two imports can share.
	delivery []string
}

// Delivery implements ports.DeliveryReporter: what plurx made of the files,
// in one line, for the transfer's trace.
func (p *Plurx) Delivery() string { return strings.Join(p.delivery, "; ") }

// Send implements ports.Notifier: one targeted scan per directory, each
// classified. The delivery worker drives Cinema through SendScan instead, one
// sub-delivery per directory; Send remains for a row written before
// sub-deliveries existed.
func (p *Plurx) Send(ctx context.Context, n ports.Notification) error {
	if p.URL == "" || p.APIKey == "" {
		return fmt.Errorf("plurx: url and apiKey required")
	}
	// An event with nothing to index is not a failure — it is a chat-shaped
	// event arriving at a notifier that only speaks about files. Saying
	// nothing is the correct response; reporting an error would light up
	// the health page for a grab notification.
	if n.Import == nil || len(n.Import.Dirs) == 0 {
		return nil
	}
	p.delivery = nil

	// One request per DIRECTORY, not per file. A season pack is one folder
	// and a dozen files; a dozen requests naming the same folder is a dozen
	// chances for one to fail and eleven scans of work already done.
	var failures []string
	permanent := true
	for _, dir := range n.Import.Dirs {
		res, err := p.scan(ctx, dir, n.Import, "")
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", dir, err))
			permanent = permanent && errors.Is(err, ports.ErrNotifyPermanent)
			continue
		}
		if line := outcomeLine(res); line != "" {
			p.delivery = append(p.delivery, line)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	// Partial delivery is worth naming as such: the difference between "one
	// folder of a multi-season import was rejected" and "plurx is down" is
	// the first thing anyone reading this wants to know.
	err := fmt.Errorf("plurx: %d of %d directories not indexed: %s",
		len(failures), len(n.Import.Dirs), strings.Join(failures, "; "))
	if permanent {
		return fmt.Errorf("%w: %s", ports.ErrNotifyPermanent, err)
	}
	return err
}

// SendScan implements ports.ScanSender: one sub-delivery, one directory,
// with its request key, the files the import placed there, and a short wait
// so a quick targeted scan answers in the same request (contract 4.2).
func (p *Plurx) SendScan(ctx context.Context, n ports.Notification) (ports.NotifyResult, error) {
	if p.URL == "" || p.APIKey == "" {
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNever, Msg: "plurx: url and apiKey required"}
	}
	if n.Import == nil || len(n.Import.Dirs) != 1 {
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNever, Msg: "plurx: a sub-delivery names exactly one directory"}
	}
	dir := n.Import.Dirs[0]
	return p.scan(ctx, dir, n.Import, n.IdempotencyKey)
}

// waitMs is how long Cinema may hold a request for its targeted result. It
// caps at 15 s; 10 s leaves the attempt's own 20 s deadline room to read the
// answer.
const waitMs = 10000

type plurxScanRequest struct {
	Path          string                `json:"path,omitempty"`
	IDs           *plurxIDs             `json:"ids,omitempty"`
	Hint          string                `json:"hint,omitempty"`
	Series        *plurxIDs             `json:"series,omitempty"`
	Book          *ports.BookImportInfo `json:"book,omitempty"`
	CorrelationID string                `json:"correlation_id,omitempty"`
	Source        string                `json:"source"`
	// Contract v2 (section 4.2, 4.3); an older Cinema ignores them.
	RequestKey    string               `json:"request_key,omitempty"`
	ExpectedFiles []ports.ExpectedFile `json:"expected_files,omitempty"`
	WaitMs        int                  `json:"wait_ms,omitempty"`
	ProbePaths    []string             `json:"probe_paths,omitempty"`
	// Contract amendment (removed paths): a Re-match's request names the
	// event and the files that left `path`.
	Event        string   `json:"event,omitempty"`
	RemovedPaths []string `json:"removed_paths,omitempty"`
}

func (p *Plurx) hasFeature(f string) bool {
	for _, x := range p.Features {
		if x == f {
			return true
		}
	}
	return false
}

// LearnedFeatures implements ports.FeatureReporter: what the last Test learned.
func (p *Plurx) LearnedFeatures() []string { return p.learned }

// ProbeFeatures implements ports.FeatureProber: the connection test's answer
// with no paths to check, read for its features. An older Cinema refuses the
// body with a 422 and supports none of them.
func (p *Plurx) ProbeFeatures(ctx context.Context) ([]string, error) {
	if p.URL == "" || p.APIKey == "" {
		return nil, fmt.Errorf("plurx: url and apiKey required")
	}
	body, err := json.Marshal(map[string]any{"probe_paths": []string{}, "source": "monarr"})
	if err != nil {
		return nil, err
	}
	status, raw, _, err := p.do(ctx, http.MethodPost, "/api/v1/scan", body)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return answerFeatures(raw), nil
	case http.StatusUnprocessableEntity:
		return []string{}, nil
	}
	return nil, fmt.Errorf("plurx returned %d to the feature probe: %s", status, summarize(raw))
}

func answerFeatures(raw []byte) []string {
	var body struct {
		Features []string `json:"features"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Features == nil {
		return []string{}
	}
	return body.Features
}

// commonDir is the deepest folder holding every expected file: the new
// folder of a move, which an older Cinema is sent as a plain scan.
func commonDir(files []ports.ExpectedFile) string {
	if len(files) == 0 {
		return ""
	}
	dir := filepath.Dir(files[0].Path)
	for _, f := range files[1:] {
		for dir != "/" && dir != "." && !strings.HasPrefix(f.Path, strings.TrimSuffix(dir, "/")+"/") {
			dir = filepath.Dir(dir)
		}
	}
	return dir
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
	if info.Kind == "book" {
		// The scanner still decides what each file actually is. Curator's
		// explicit work/edition facts avoid a second provider match and allow
		// related editions without ever joining on title + author.
		req.Hint = "book"
		req.Book = info.Book
		return req
	}
	if info.Kind == "series" {
		req.Hint = "episode"
		// Only the TMDB id belongs to the show; plurx keys episodes off the
		// series TMDB id alone, so sending an IMDb id here would be
		// inventing precision it does not use.
		if info.TmdbID != 0 {
			req.Series = &plurxIDs{TMDB: info.TmdbID}
		}
		return req
	}
	req.Hint = "movie"
	if info.TmdbID != 0 || info.ImdbID != "" {
		req.IDs = &plurxIDs{TMDB: info.TmdbID, IMDB: info.ImdbID}
	}
	return req
}

// scan posts one directory.
//
// No retry loop here: retrying is the delivery queue's job, and it does it
// durably across restarts, which a loop inside one call cannot. What this
// owns is the CLASSIFICATION — whether trying again could possibly help, and
// whether the next try needs a fresh attempt — because that knowledge lives
// with the status codes.
func (p *Plurx) scan(ctx context.Context, dir string, info *ports.ImportInfo, key string) (ports.NotifyResult, error) {
	req := scanBody(dir, info)
	sentRemoved := false
	if key != "" {
		req.RequestKey = key
		req.ExpectedFiles = info.ExpectedFiles[dir]
		req.WaitMs = waitMs
		if p.Wait > 0 {
			req.WaitMs = int(p.Wait / time.Millisecond)
		}
		if len(info.RemovedPaths) > 0 {
			if p.hasFeature(ports.FeatureRemovedPaths) {
				req.Event, req.RemovedPaths, sentRemoved = "moved", info.RemovedPaths, true
			} else {
				// An older Cinema would full-scan the common parent and
				// cannot drop the old entry: send the new folder as a plain
				// import-shaped scan (contract amendment 6).
				if len(req.ExpectedFiles) == 0 {
					return ports.NotifyResult{State: "done", FilesReported: true}, nil
				}
				req.Path = commonDir(req.ExpectedFiles)
			}
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNever, Msg: err.Error()}
	}
	status, raw, header, err := p.do(ctx, http.MethodPost, "/api/v1/scan", body)
	if err != nil {
		return ports.NotifyResult{}, err
	}
	res, err := classifyScan(req.Path, status, raw, header, key != "")
	res.RemovedPathsSent = sentRemoved
	return res, err
}

// ReadResult implements ports.ResultReader: GET /api/v1/scan/requests/{id},
// which a scan:trigger key may read for the requests it created.
func (p *Plurx) ReadResult(ctx context.Context, requestID string) (ports.NotifyResult, error) {
	status, raw, _, err := p.do(ctx, http.MethodGet, "/api/v1/scan/requests/"+url.PathEscape(requestID), nil)
	if err != nil {
		return ports.NotifyResult{}, err
	}
	switch status {
	case http.StatusOK:
		return parseAnswer(raw)
	case http.StatusForbidden:
		// Either an older Cinema, which lets only status:read keys read
		// requests (the scan was accepted, its result will never be
		// reported, contract section 6), or a current one that no longer
		// knows this id (its creator lookup finds nobody). Only the caller
		// can tell them apart — by whether a read of this request has
		// answered before.
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryUnsupported, Status: status,
			Msg: "cinema does not let this key read its requests (403): " + summarize(raw)}
	case http.StatusUnauthorized:
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNever, Status: status,
			Msg: "plurx rejected the key (401) while reading request " + requestID + ": " + summarize(raw)}
	case http.StatusNotFound:
		// Cinema answers an unknown id with 403 (above); a 404 is a server
		// in front of it, or one built differently, saying the same thing.
		// Post again as a new attempt, so the answer is a fresh scan.
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNewAttempt, Status: status,
			Msg: "cinema no longer knows request " + requestID}
	}
	return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetrySameAttempt, Status: status,
		Msg: fmt.Sprintf("plurx returned %d reading request %s: %s", status, requestID, summarize(raw))}
}

// do sends one request. A transport failure (no response, a timeout, a reset
// after send) is ambiguous: the request may have landed, so the next try
// reuses its key.
func (p *Plurx) do(ctx context.Context, method, path string, body []byte) (int, []byte, http.Header, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.URL, "/")+path, rd)
	if err != nil {
		return 0, nil, nil, &ports.NotifyError{Retry: ports.RetryNever, Msg: err.Error()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, &ports.NotifyError{Retry: ports.RetrySameAttempt, Msg: fmt.Sprintf("cannot reach plurx: %s", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return resp.StatusCode, raw, resp.Header, nil
}

// classifyScan reads POST /api/v1/scan's answer (contract section 4.1):
// permanent only on 401/403, 400 and 422 path_outside_roots; 503
// path_unreadable is a confirmed retryable result; 429, other 5xx and
// anything unrecognised are transient on the same key.
//
// keyed says the POST carried a request_key. Cinema answers a request whose
// scan FAILED with a 500 — inside wait_ms, and on every replay of that key —
// so a 500 to a keyed POST is a confirmed retryable result: the next try
// needs a new attempt, or it would replay the same failure hourly forever.
// Advancing is cheap: Cinema coalesces duplicate targeted work per library.
func classifyScan(dir string, status int, raw []byte, header http.Header, keyed bool) (ports.NotifyResult, error) {
	var body struct {
		Status        string    `json:"status"`
		Code          string    `json:"code"`
		Error         string    `json:"error"`
		Roots         *[]string `json:"roots"`
		RetryAfterSec int       `json:"retry_after_s"`
	}
	_ = json.Unmarshal(raw, &body)
	never := func(msg string) error {
		return &ports.NotifyError{Retry: ports.RetryNever, Status: status, Code: body.Code, Msg: msg}
	}
	switch {
	case status == http.StatusOK || status == http.StatusAccepted:
		return parseAnswer(raw)
	case status == http.StatusUnprocessableEntity && (body.Code == "path_outside_roots" || (body.Code == "" && body.Roots != nil)):
		// The 422 carries plurx's library roots. Carrying that message
		// through verbatim is the difference between "422" and being able
		// to see, in Monarr's own log, that plurx has /media mounted where
		// Monarr has /data/media.
		return ports.NotifyResult{}, never(fmt.Sprintf("plurx does not have %s under any library root — %s", dir, summarize(raw)))
	case status == http.StatusBadRequest:
		return ports.NotifyResult{}, never("plurx refused the request body (400): " + summarize(raw))
	case status == http.StatusUnauthorized:
		return ports.NotifyResult{}, never("plurx rejected the key (401) — it is unknown, revoked, " +
			"or a plurx USER token, which this route does not accept: " + summarize(raw))
	case status == http.StatusForbidden:
		return ports.NotifyResult{}, never("the key is valid but lacks the scan:trigger scope (403): " + summarize(raw))
	case status == http.StatusServiceUnavailable && body.Code == "path_unreadable":
		// The share under the root is not readable right now (unmounted,
		// EIO): a retry after it comes back must scan afresh.
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNewAttempt, Status: status, Code: body.Code,
			RetryAfter: retryAfter(header, body.RetryAfterSec),
			Msg:        "cinema cannot read " + dir + " right now: " + summarize(raw)}
	case keyed && (status == http.StatusInternalServerError || body.Status == "failed"):
		msg := "cinema could not complete the scan"
		if body.Code != "" {
			msg += " (" + body.Code + ")"
		}
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetryNewAttempt, Status: status, Code: body.Code,
			Msg: fmt.Sprintf("%s: plurx returned %d: %s", msg, status, summarize(raw))}
	}
	// 429 is plurx asking for room, 502/504 a gateway that may or may not
	// have passed the request on, another 503 a queue refusing it before it
	// was recorded; none is a reason to drop the scan or to start a new one.
	return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetrySameAttempt, Status: status, Code: body.Code,
		RetryAfter: retryAfter(header, body.RetryAfterSec),
		Msg:        fmt.Sprintf("plurx returned %d: %s", status, summarize(raw))}
}

// retryAfter reads how long the far side asked to be left alone: the
// Retry-After header in seconds, else the body's retry_after_s. Capped at an
// the outbox ladder's last rung (backoff.Outbox.Max), its own ceiling.
func retryAfter(header http.Header, bodySec int) time.Duration {
	sec := bodySec
	if v := strings.TrimSpace(header.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			sec = n
		}
	}
	if sec <= 0 {
		return 0
	}
	if d := time.Duration(sec) * time.Second; d < backoff.Outbox.Max() {
		return d
	}
	return backoff.Outbox.Max()
}

// parseAnswer reads a scan answer or a request record: status, request id,
// waiting_on, items, and the per-file report when there is one.
func parseAnswer(raw []byte) (ports.NotifyResult, error) {
	var body struct {
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
		WaitingOn string `json:"waiting_on"`
		Items     []struct {
			ItemID int64 `json:"item_id"`
		} `json:"items"`
		Files *[]ports.PlacedReport `json:"files"`
		Error json.RawMessage       `json:"error"`
		Code  string                `json:"code"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ports.NotifyResult{}, &ports.NotifyError{Retry: ports.RetrySameAttempt,
			Msg: "plurx answered with something that is not JSON: " + summarize(raw)}
	}
	res := ports.NotifyResult{RequestID: body.RequestID, WaitingOn: body.WaitingOn, ErrCode: body.Code}
	switch body.Status {
	case "scanned", "done":
		res.State = "done"
	default:
		res.State = body.Status
	}
	if body.Files != nil {
		res.Files, res.FilesReported = *body.Files, true
	}
	var msg string
	if json.Unmarshal(body.Error, &msg) == nil {
		res.Err = msg
	} else if len(body.Error) > 0 && string(body.Error) != "null" {
		res.Err = string(body.Error)
	}
	seen := map[int64]bool{}
	add := func(id int64) {
		if id != 0 && !seen[id] {
			seen[id] = true
			res.ItemIDs = append(res.ItemIDs, id)
		}
	}
	for _, f := range res.Files {
		if f.State == "placed" {
			add(f.ItemID)
		}
	}
	for _, it := range body.Items {
		add(it.ItemID)
	}
	return res, nil
}

// outcomeLine renders plurx's answer as one short line for the trace.
//
// The item id is the payoff: it is what joins "monarr imported this" to
// "plurx made item 1201 of it", and without it the trace stops at the
// boundary and somebody has to go and look in two UIs.
func outcomeLine(res ports.NotifyResult) string {
	switch res.State {
	case "done":
		ids := make([]string, 0, len(res.ItemIDs))
		for _, id := range res.ItemIDs {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		if len(ids) == 0 {
			return "scanned"
		}
		return "scanned → plurx item " + strings.Join(ids, ", ")
	case "queued":
		return "queued as " + res.RequestID
	}
	return ""
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

// Test implements ports.Notifier: the legacy probe.
//
// It asks plurx to scan a path that cannot exist under any library root, and
// counts the 422 as the pass. That is deliberate: it exercises the URL, the
// key, and the scope — everything that can be misconfigured — and does not
// start a scan of anything real while doing it. A test that triggered work
// would be a test people learn not to press. TestPaths is the Test a
// current Cinema answers; this is what an older one gets.
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
	status, raw, header, err := p.do(ctx, http.MethodPost, "/api/v1/scan", body)
	if err != nil {
		return err
	}
	if status == http.StatusUnprocessableEntity {
		return nil // the handler spoke: URL, key and scope all work
	}
	if _, err := classifyScan("/monarr/connection-test", status, raw, header, false); err != nil {
		return err
	}
	return nil // it somehow matched a root; the connection works either way
}

// TestPaths implements ports.PathTester (contract section 4.3): send the
// library roots as probe_paths and fail, with Curator's and Cinema's paths
// side by side, when Cinema cannot see one. Today a mount mismatch is found
// one import at a time; this finds it at setup.
//
// An older Cinema requires `path`, and its JSON extractor rejects the body
// with a 422 of its own — no `roots`, unlike the handler's "not under any
// library root" 422. That is told apart by body, and answered with the
// legacy probe plus a note that path checking needs a newer Cinema.
func (p *Plurx) TestPaths(ctx context.Context, roots []string) (string, error) {
	if p.URL == "" || p.APIKey == "" {
		return "", fmt.Errorf("plurx: url and apiKey required")
	}
	body, err := json.Marshal(plurxScanRequest{ProbePaths: roots, Source: "monarr"})
	if err != nil {
		return "", err
	}
	status, raw, header, err := p.do(ctx, http.MethodPost, "/api/v1/scan", body)
	if err != nil {
		return "", err
	}
	var probe struct {
		Roots        []string  `json:"roots"`
		Visible      *[]string `json:"visible"`
		Missing      []string  `json:"missing"`
		OutsideRoots []string  `json:"outside_roots"`
	}
	_ = json.Unmarshal(raw, &probe)
	if status == http.StatusOK {
		p.learned = answerFeatures(raw)
	} else if status == http.StatusUnprocessableEntity {
		p.learned = []string{}
	}
	switch {
	case status == http.StatusOK && probe.Visible != nil:
		return "", probeFailure(roots, probe.Roots, probe.Missing, probe.OutsideRoots)
	case status == http.StatusUnprocessableEntity && probe.Roots == nil:
		if err := p.Test(ctx); err != nil {
			return "", err
		}
		return "connected; checking the library paths needs a newer cinema", nil
	case status == http.StatusUnprocessableEntity:
		return "", nil // the handler spoke, as the legacy probe counts it
	case status == http.StatusOK:
		return "connected; cinema did not report on the library paths", nil
	}
	_, err = classifyScan("probe_paths", status, raw, header, false)
	if err == nil {
		err = fmt.Errorf("plurx returned %d to the path check: %s", status, summarize(raw))
	}
	return "", err
}

// probeFailure names every library root Cinema cannot see, beside what
// Cinema does have.
func probeFailure(asked, cinemaRoots, missing, outside []string) error {
	if len(missing) == 0 && len(outside) == 0 {
		return nil
	}
	theirs := "none"
	if len(cinemaRoots) > 0 {
		theirs = strings.Join(cinemaRoots, ", ")
	}
	lines := make([]string, 0, len(missing)+len(outside))
	for _, m := range outside {
		lines = append(lines, fmt.Sprintf("Curator %s ↔ cinema: not under any of its library roots (%s)", m, theirs))
	}
	for _, m := range missing {
		lines = append(lines, fmt.Sprintf("Curator %s ↔ cinema: under its library root but it cannot see it", m))
	}
	return fmt.Errorf("cinema cannot see %d of %d library root(s): %s", len(lines), len(asked), strings.Join(lines, "; "))
}
