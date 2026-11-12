// Package cinematest is a fake Cinema (plurx) scan API for tests: the contract
// v2 handler (request_key replay, expected_files re-probed and reported per
// file, wait_ms, typed 422/503 outcomes, the creating key reading its own
// request, probe_paths) and, with Old set, the handler as it was before v2
// (path required by the JSON extractor, a fresh request per POST, one 422 for
// everything outside a root, status reads refused to a scan:trigger key).
//
// It reads the real filesystem: an expected file is reported placed with the
// bytes and mtime_ns it has on disk now, unless the test says otherwise.
//
// Keys: "plx_secret" holds scan:trigger; "plx_status" is a real key without
// it (403 on POST); anything else is unknown (401). The limits are the real
// handler's: at most ports.MaxExpectedFiles expected files within
// ports.MaxExpectedFileBytes, a request_key of at most
// ports.MaxRequestKeyBytes, absolute paths — anything else is a 400.
package cinematest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Request is one scan request the fake recorded.
type Request struct {
	ID            string
	Key           string // request_key, "" from an old client
	Path          string
	Expected      []ports.ExpectedFile
	Removed       []string // removed_paths (contract amendment), "" event when absent
	Event         string
	ProbeOnly     bool   // probe_only
	State         string // queued | running | done | failed
	Error         string // a failed request's error
	Files         []ports.PlacedReport
	ProbeAttempts int
	done          chan struct{}
}

// Cinema is the fake. Zero-value fields are the plain behaviour; set them
// (under Lock/Unlock, or before the first request) to change it.
type Cinema struct {
	Server *httptest.Server

	mu sync.Mutex
	// Roots are the library roots; a path outside every one is a 422.
	Roots []string
	// Old answers as a pre-v2 Cinema.
	Old bool
	// RemovedPaths is the removed-paths amendment: the probe answers
	// features ["removed_paths"] and a request's removed paths are answered
	// gone (absent), present (still there) or unknown (listed in Unknown).
	// Without it the fake is a v2 Cinema that ignores removed_paths.
	RemovedPaths bool
	Unknown      map[string]bool
	// Retained answers a removed path retained with the given code; PresentReason
	// is the reason a still-present path carries.
	Retained      map[string]string
	PresentReason string
	// Down drops every connection without an answer (a stopped Cinema).
	Down bool
	// Unreadable answers 503 path_unreadable (an unmounted share).
	Unreadable bool
	// Queue answers 202 queued with WaitingOn instead of scanning; Release
	// lets the queued requests finish.
	Queue     bool
	WaitingOn string
	// Skip reports an expected path skipped with the given reason.
	Skip map[string]string
	// Cached reports, for a path, a stale record instead of re-probing it
	// (what the size+mtime short-circuit did to a same-path replacement).
	Cached map[string]ports.PlacedReport
	// Delay holds the answer to the n-th POST (1-based) carrying key after
	// recording it — a response that arrives after the caller gave up.
	Delay func(key string, n int) time.Duration
	// FinishAfter, with Queue, finishes a queued request on its own after
	// this long, so a POST's wait_ms can see it finish (200 after the wait).
	FinishAfter time.Duration
	// Running reports queued requests as running.
	Running bool
	// Fail makes the targeted scan of a path fail with this error: the
	// request is failed, and the real handler answers it — first time and
	// every replay of its key — with a bare 500.
	Fail map[string]string

	posts    []string // request_key (or path) of every POST, in order
	polls    []string
	byKey    map[string]*Request
	requests []*Request
	itemIDs  map[string]int64
	nextID   int
}

// New starts a fake Cinema with the given library roots.
func New(roots ...string) *Cinema {
	c := &Cinema{Roots: roots, byKey: map[string]*Request{}, itemIDs: map[string]int64{}}
	c.Server = httptest.NewServer(http.HandlerFunc(c.serve))
	return c
}

// Close stops the server.
func (c *Cinema) Close() { c.Server.Close() }

// URL is the server's base URL.
func (c *Cinema) URL() string { return c.Server.URL }

// Lock and Unlock guard the behaviour fields.
func (c *Cinema) Lock()   { c.mu.Lock() }
func (c *Cinema) Unlock() { c.mu.Unlock() }

// Set changes behaviour under the lock.
func (c *Cinema) Set(f func(*Cinema)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

// Posts returns the key (or, from an old client, the path) of every POST.
func (c *Cinema) Posts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.posts...)
}

// Polls returns the request id of every status read.
func (c *Cinema) Polls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.polls...)
}

// Requests returns every scan request the fake created.
func (c *Cinema) Requests() []Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Request, 0, len(c.requests))
	for _, r := range c.requests {
		out = append(out, *r)
	}
	return out
}

// ItemID is the item id the fake gives a path.
func (c *Cinema) ItemID(path string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.itemID(path)
}

func (c *Cinema) itemID(path string) int64 {
	if id, ok := c.itemIDs[path]; ok {
		return id
	}
	id := int64(1201 + len(c.itemIDs))
	c.itemIDs[path] = id
	return id
}

// Release finishes every queued request.
func (c *Cinema) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Queue = false
	for _, r := range c.requests {
		if r.State == "queued" {
			c.finish(r)
		}
	}
}

// Forget drops every request, as a restart with a pruned store would: a read
// of one of them is now an unknown id.
func (c *Cinema) Forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests, c.byKey = nil, map[string]*Request{}
}

type body struct {
	Path          *string              `json:"path"`
	RequestKey    string               `json:"request_key"`
	ExpectedFiles []ports.ExpectedFile `json:"expected_files"`
	WaitMs        int                  `json:"wait_ms"`
	ProbePaths    []string             `json:"probe_paths"`
	RemovedPaths  []string             `json:"removed_paths"`
	Event         string               `json:"event"`
	ProbeOnly     bool                 `json:"probe_only"`
}

func (c *Cinema) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	down := c.Down
	c.mu.Unlock()
	if down {
		// A stopped Cinema: the connection opens and closes with no answer.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	auth := r.Header.Get("Authorization")
	if auth != "Bearer plx_secret" && auth != "Bearer plx_status" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unknown key"})
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/scan":
		if auth != "Bearer plx_secret" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "key lacks the scan:trigger scope"})
			return
		}
		c.scan(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/scan/requests/"):
		c.status(w, strings.TrimPrefix(r.URL.Path, "/api/v1/scan/requests/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (c *Cinema) scan(w http.ResponseWriter, r *http.Request) {
	var b body
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeText(w, http.StatusBadRequest, "bad json")
		return
	}
	c.mu.Lock()
	if c.Old && b.Path == nil {
		c.mu.Unlock()
		// Axum's Json extractor, before the handler runs: its own text.
		writeText(w, http.StatusUnprocessableEntity,
			"Failed to deserialize the JSON body into the target type: missing field `path` at line 1 column 42")
		return
	}
	if !c.Old && b.Path == nil && b.ProbePaths != nil {
		out := c.probe(b.ProbePaths)
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if b.Path == nil {
		c.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "path_required"})
		return
	}
	path := filepath.Clean(*b.Path)
	key := b.RequestKey
	if c.Old {
		key = "" // ignored (expected_files too; kept here only to list the items it scans)
	} else if code := invalid(*b.Path, b); code != "" {
		c.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": code, "error": "invalid scan request: " + code})
		return
	}
	tag := key
	if tag == "" {
		tag = path
	}
	c.posts = append(c.posts, tag)
	n := 0
	for _, p := range c.posts {
		if p == tag {
			n++
		}
	}
	// Idempotency first: a repeated key is the same request and result.
	if req, ok := c.byKey[key]; ok && key != "" {
		delay := c.delay(key, n)
		c.mu.Unlock()
		hold(r, delay)
		c.respond(w, req)
		return
	}
	if !c.under(path) {
		ans := map[string]any{"error": "path is not under any library root", "path": path, "roots": c.Roots}
		if !c.Old {
			ans["code"] = "path_outside_roots"
		}
		c.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, ans)
		return
	}
	if c.Unreadable && !c.Old {
		c.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": "path_unreadable",
			"error": "library root could not be read: ENOTCONN", "path": path, "retry_after_s": 60})
		return
	}
	c.nextID++
	req := &Request{ID: fmt.Sprintf("sr-%d", c.nextID), Key: key, Path: path, Expected: b.ExpectedFiles, State: "queued",
		Removed: b.RemovedPaths, Event: b.Event, ProbeOnly: b.ProbeOnly, done: make(chan struct{})}
	c.requests = append(c.requests, req)
	if key != "" {
		c.byKey[key] = req
	}
	switch {
	case !c.Queue:
		c.finish(req)
	case c.FinishAfter > 0:
		time.AfterFunc(c.FinishAfter, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if req.State == "queued" {
				c.finish(req)
			}
		})
	}
	delay := c.delay(key, n)
	c.mu.Unlock()
	// wait_ms: hold the answer until the scan finishes, up to 15 s.
	if wait := time.Duration(b.WaitMs) * time.Millisecond; wait > 0 && !c.Old {
		if wait > 15*time.Second {
			wait = 15 * time.Second
		}
		select {
		case <-req.done:
		case <-time.After(wait):
		case <-r.Context().Done():
		}
	}
	hold(r, delay)
	c.respond(w, req)
}

// invalid is the real handler's 400s: a relative path, an oversized key, too
// many or too large expected files, a relative expected path.
func invalid(path string, b body) string {
	if !filepath.IsAbs(path) {
		return "path_not_absolute"
	}
	if len(b.RequestKey) > ports.MaxRequestKeyBytes {
		return "request_key_too_long"
	}
	if len(b.ExpectedFiles) > ports.MaxExpectedFiles {
		return "too_many_expected_files"
	}
	if raw, _ := json.Marshal(b.ExpectedFiles); len(raw) > ports.MaxExpectedFileBytes {
		return "expected_files_too_large"
	}
	for _, e := range b.ExpectedFiles {
		if !filepath.IsAbs(e.Path) {
			return "expected_path_not_absolute"
		}
	}
	return ""
}

// respond writes a request's answer as the real handler does: 200 with the
// per-file report when done, 202 while queued or running, and a bare 500 for
// a failed request — on its first answer and on every replay of its key.
func (c *Cinema) respond(w http.ResponseWriter, req *Request) {
	c.mu.Lock()
	ans, status := c.answer(req)
	c.mu.Unlock()
	writeJSON(w, status, ans)
}

func (c *Cinema) delay(key string, n int) time.Duration {
	if c.Delay == nil {
		return 0
	}
	return c.Delay(key, n)
}

// hold keeps the answer back, giving up when the caller does.
func hold(r *http.Request, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-r.Context().Done():
	}
}

// finish runs the targeted scan: re-probe every expected file.
func (c *Cinema) finish(req *Request) {
	defer func() {
		if req.done != nil {
			select {
			case <-req.done:
			default:
				close(req.done)
			}
		}
	}()
	req.ProbeAttempts++
	req.Files = nil
	if msg, ok := c.Fail[req.Path]; ok {
		req.State, req.Error = "failed", msg
		return
	}
	req.State = "done"
	for _, e := range req.Expected {
		if reason, ok := c.Skip[e.Path]; ok {
			req.Files = append(req.Files, ports.PlacedReport{Path: e.Path, State: "skipped", Reason: reason})
			continue
		}
		if rep, ok := c.Cached[e.Path]; ok {
			req.Files = append(req.Files, rep)
			continue
		}
		fi, err := os.Stat(e.Path)
		if err != nil {
			req.Files = append(req.Files, ports.PlacedReport{Path: e.Path, State: "missing"})
			continue
		}
		req.Files = append(req.Files, ports.PlacedReport{Path: e.Path, State: "placed", ItemID: c.itemID(e.Path),
			Bytes: fi.Size(), MtimeNs: fi.ModTime().UnixNano()})
	}
	if !c.RemovedPaths {
		return
	}
	for _, p := range req.Removed {
		switch _, err := os.Stat(p); {
		case c.Retained[p] != "":
			req.Files = append(req.Files, ports.PlacedReport{Path: p, State: "retained", Code: c.Retained[p]})
		case err == nil:
			req.Files = append(req.Files, ports.PlacedReport{Path: p, State: "present", Reason: c.PresentReason})
		case c.Unknown[p]:
			req.Files = append(req.Files, ports.PlacedReport{Path: p, State: "unknown"})
		default:
			req.Files = append(req.Files, ports.PlacedReport{Path: p, State: "gone", ItemID: c.itemID(p)})
		}
	}
}

func (c *Cinema) items(req *Request) []map[string]any {
	var out []map[string]any
	for _, f := range req.Files {
		if f.State == "placed" {
			out = append(out, map[string]any{"item_id": f.ItemID, "path": f.Path})
		}
	}
	return out
}

// answer is the POST's answer for a request.
func (c *Cinema) answer(req *Request) (map[string]any, int) {
	if req.State == "failed" && !c.Old {
		return map[string]any{"error": req.Error}, http.StatusInternalServerError
	}
	if req.State != "done" {
		ans := map[string]any{"status": "queued", "request_id": req.ID}
		return ans, http.StatusAccepted
	}
	if c.Old {
		// No per-file report: an old Cinema never sent one.
		return map[string]any{"status": "scanned", "request_id": req.ID, "items": c.items(req)}, http.StatusOK
	}
	return map[string]any{"status": "scanned", "request_id": req.ID, "items": c.items(req), "files": req.Files}, http.StatusOK
}

func (c *Cinema) status(w http.ResponseWriter, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls = append(c.polls, id)
	if c.Old {
		// status:read only; the scan:trigger key is refused.
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "key lacks the status:read scope"})
		return
	}
	for _, r := range c.requests {
		if r.ID != id {
			continue
		}
		rec := map[string]any{"request_id": r.ID, "status": r.State, "path": r.Path, "files": r.Files, "items": c.items(r), "error": nil}
		switch r.State {
		case "queued":
			if c.Running {
				rec["status"] = "running"
			}
			rec["waiting_on"] = c.WaitingOn
			rec["files"] = []ports.PlacedReport{}
		case "failed":
			rec["error"], rec["code"] = r.Error, "scan_failed"
			rec["files"] = []ports.PlacedReport{}
		}
		writeJSON(w, http.StatusOK, rec)
		return
	}
	// The creator lookup finds nobody for an unknown id: the real handler
	// answers Forbidden, not Not Found.
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "scan request not readable by this key"})
}

func (c *Cinema) under(path string) bool {
	for _, root := range c.Roots {
		if path == root || strings.HasPrefix(path, strings.TrimRight(root, "/")+"/") {
			return true
		}
	}
	return false
}

func (c *Cinema) probe(paths []string) map[string]any {
	visible, missing, outside := []string{}, []string{}, []string{}
	for _, p := range paths {
		switch {
		case !c.under(filepath.Clean(p)):
			outside = append(outside, p)
		case c.Unreadable:
			missing = append(missing, p)
		default:
			if _, err := os.Stat(p); err != nil {
				missing = append(missing, p)
			} else {
				visible = append(visible, p)
			}
		}
	}
	out := map[string]any{"roots": c.Roots, "visible": visible, "missing": missing, "outside_roots": outside}
	if c.RemovedPaths {
		out["features"] = []string{"removed_paths"}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, status int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(s))
}
