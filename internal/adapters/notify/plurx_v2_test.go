package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/adapters/notify/cinematest"
	"github.com/pjunod/monarr/internal/ports"
)

// Contract v2 section 4.2 on the wire: one directory, its request key with
// the attempt, the files the import placed there, and a short wait.

func subDelivery(dir string, expected ...ports.ExpectedFile) ports.Notification {
	n := movieImport(dir)
	n.Import.ExpectedFiles = map[string][]ports.ExpectedFile{dir: expected}
	n.IdempotencyKey = "t-42-a3f9c1/gen-1/0123456789abcdef/1"
	return n
}

func scanner(t *testing.T, url string) *Plurx {
	t.Helper()
	p, ok := plurxNotifier(url).(*Plurx)
	if !ok {
		t.Fatal("the plurx notifier is not a *Plurx")
	}
	return p
}

func retryOf(t *testing.T, err error) ports.NotifyRetry {
	t.Helper()
	var ne *ports.NotifyError
	if !errors.As(err, &ne) {
		t.Fatalf("error %v is not classified", err)
	}
	return ne.Retry
}

func TestPlurxSendScanCarriesKeyExpectedFilesAndWait(t *testing.T) {
	var got map[string]any
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"status":"scanned","request_id":"sr-1","items":[{"item_id":1201}],
			"files":[{"state":"placed","path":"/media/movies/Heat (1995)/Heat.mkv","item_id":1201,"file_id":5,"bytes":10,"mtime_ns":1794076138123456789}]}`))
	}))
	defer srv.Close()

	res, err := scanner(t, srv.URL).SendScan(context.Background(), subDelivery("/media/movies/Heat (1995)",
		ports.ExpectedFile{Path: "/media/movies/Heat (1995)/Heat.mkv", Bytes: 10, MtimeNs: 1794076138123456789}))
	if err != nil {
		t.Fatal(err)
	}
	if got["path"] != "/media/movies/Heat (1995)" || got["request_key"] != "t-42-a3f9c1/gen-1/0123456789abcdef/1" || got["wait_ms"] != float64(10000) {
		t.Errorf("request = %v", got)
	}
	files, _ := got["expected_files"].([]any)
	if len(files) != 1 {
		t.Fatalf("expected_files = %v", got["expected_files"])
	}
	f := files[0].(map[string]any)
	// The nanoseconds survive the trip: a float64 would round them away.
	if f["path"] != "/media/movies/Heat (1995)/Heat.mkv" || f["bytes"] != float64(10) || !strings.Contains(rawBody, `"mtime_ns":1794076138123456789`) {
		t.Errorf("expected file = %v", f)
	}
	if res.State != "done" || !res.FilesReported || len(res.Files) != 1 || res.Files[0].MtimeNs != 1794076138123456789 || len(res.ItemIDs) != 1 || res.ItemIDs[0] != 1201 {
		t.Errorf("result = %+v", res)
	}
}

func TestPlurxSendScanReadsAQueuedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued","request_id":"sr-9f2","correlation_id":"t-42"}`))
	}))
	defer srv.Close()
	res, err := scanner(t, srv.URL).SendScan(context.Background(), subDelivery("/m/a"))
	if err != nil || res.State != "queued" || res.RequestID != "sr-9f2" || res.FilesReported {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

// Contract section 4.1: 429 is transient on the same key; it was
// classified permanent before v2 (plurx.go:196), which dropped the scan.
func Test429IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := scanner(t, srv.URL).SendScan(context.Background(), subDelivery("/m/a"))
	if err == nil || errors.Is(err, ports.ErrNotifyPermanent) || retryOf(t, err) != ports.RetrySameAttempt {
		t.Fatalf("429 = %v, want transient on the same attempt", err)
	}
}

func TestPlurxClassifiesTheTypedPathOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ports.NotifyRetry
	}{
		{"outside every root is permanent", http.StatusUnprocessableEntity,
			`{"code":"path_outside_roots","error":"path is not under any library root","path":"/x","roots":["/20t/tv"]}`, ports.RetryNever},
		{"an old cinema's single 422 is permanent as before", http.StatusUnprocessableEntity,
			`{"error":"path is not under any library root","path":"/x","roots":["/20t/tv"]}`, ports.RetryNever},
		{"an unreadable root needs a fresh attempt", http.StatusServiceUnavailable,
			`{"code":"path_unreadable","error":"library root could not be read: ENOTCONN","root":"/20t/tv","retry_after_s":60}`, ports.RetryNewAttempt},
		{"an old cinema's plain 503 is the same attempt again", http.StatusServiceUnavailable, `queue refused`, ports.RetrySameAttempt},
		{"a 500 is the same attempt again", http.StatusInternalServerError, ``, ports.RetrySameAttempt},
		{"a 401 is permanent", http.StatusUnauthorized, ``, ports.RetryNever},
		{"a 403 on the POST is permanent", http.StatusForbidden, ``, ports.RetryNever},
		{"a 400 is permanent", http.StatusBadRequest, `{"code":"path_required"}`, ports.RetryNever},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := scanner(t, srv.URL).SendScan(context.Background(), subDelivery("/x"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := retryOf(t, err); got != tc.want {
				t.Errorf("retry = %v, want %v (%v)", got, tc.want, err)
			}
			if tc.status == http.StatusUnprocessableEntity && !strings.Contains(err.Error(), "/20t/tv") {
				t.Errorf("cinema's roots must reach the log: %v", err)
			}
		})
	}
}

func TestPlurxSendScanTreatsNoAnswerAsAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err := scanner(t, url).SendScan(context.Background(), subDelivery("/m/a"))
	if err == nil || retryOf(t, err) != ports.RetrySameAttempt {
		t.Fatalf("unreachable = %v, want the same attempt again (the request may have landed)", err)
	}
}

func TestPlurxReadResultReadsTheCreatingKeysRequest(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "sr-queued"):
			_, _ = w.Write([]byte(`{"request_id":"sr-queued","status":"queued","waiting_on":"full_scan:3","files":[],"error":null}`))
		case strings.HasSuffix(r.URL.Path, "sr-failed"):
			_, _ = w.Write([]byte(`{"request_id":"sr-failed","status":"failed","error":"path_unreadable: ENOTCONN","error_code":"path_unreadable"}`))
		case strings.HasSuffix(r.URL.Path, "sr-old"):
			_, _ = w.Write([]byte(`{"request_id":"sr-old","status":"done","items":[{"item_id":7}],"report":{}}`))
		case strings.HasSuffix(r.URL.Path, "sr-gone"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	p := scanner(t, srv.URL)
	ctx := context.Background()

	res, err := p.ReadResult(ctx, "sr-queued")
	if err != nil || res.State != "queued" || res.WaitingOn != "full_scan:3" || path != "/api/v1/scan/requests/sr-queued" || auth != "Bearer plx_secret" {
		t.Fatalf("queued = %+v %v (%s %s)", res, err, path, auth)
	}
	if res, err = p.ReadResult(ctx, "sr-failed"); err != nil || res.State != "failed" || res.ErrCode != "path_unreadable" || !strings.Contains(res.Err, "ENOTCONN") {
		t.Fatalf("failed = %+v %v", res, err)
	}
	// A record without a files array is an old Cinema whose key happens to
	// read it: not the same as "nothing placed".
	if res, err = p.ReadResult(ctx, "sr-old"); err != nil || res.State != "done" || res.FilesReported || len(res.ItemIDs) != 1 {
		t.Fatalf("old record = %+v %v", res, err)
	}
	if _, err = p.ReadResult(ctx, "sr-gone"); retryOf(t, err) != ports.RetryNewAttempt {
		t.Fatalf("vanished = %v, want a fresh attempt", err)
	}
	if _, err = p.ReadResult(ctx, "sr-other"); retryOf(t, err) != ports.RetryUnsupported {
		t.Fatalf("403 on the poll = %v, want unsupported (old cinema)", err)
	}
}

// Contract section 4.3: the Test sends the library roots and fails with
// Curator's and Cinema's paths side by side.
func TestPlurxTestPathsNamesTheRootCinemaCannotSee(t *testing.T) {
	tv := t.TempDir()
	cinema := cinematest.New(tv, "/20t/movies")
	defer cinema.Close()
	p := scanner(t, cinema.URL())

	note, err := p.TestPaths(context.Background(), []string{tv})
	if err != nil || note != "" {
		t.Fatalf("a visible root = %q, %v", note, err)
	}
	_, err = p.TestPaths(context.Background(), []string{tv, "/data/media/movies", filepath.Join(tv, "gone")})
	if err == nil {
		t.Fatal("a root cinema cannot see must fail the Test")
	}
	for _, want := range []string{"2 of 3", "/data/media/movies", "/20t/movies", filepath.Join(tv, "gone")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if posts := cinema.Posts(); len(posts) != 0 {
		t.Errorf("the path check started scans: %v", posts)
	}
}

// The compat test against the old handler's real behaviour: `path` is
// required, so the JSON extractor rejects a probe_paths-only body with its
// own 422 — no `roots`. The Test falls back to the legacy probe and says path
// checking needs a newer cinema.
func TestOldCinema422WithoutRootsFallsBack(t *testing.T) {
	cinema := cinematest.New("/media")
	cinema.Set(func(c *cinematest.Cinema) { c.Old = true })
	defer cinema.Close()
	note, err := scanner(t, cinema.URL()).TestPaths(context.Background(), []string{"/media"})
	if err != nil {
		t.Fatalf("old cinema: %v", err)
	}
	if !strings.Contains(note, "newer cinema") {
		t.Errorf("note = %q, want it to say path checking needs a newer cinema", note)
	}
	if posts := cinema.Posts(); len(posts) != 1 || posts[0] != "/monarr/connection-test" {
		t.Errorf("posts = %v, want only the legacy probe", posts)
	}
}

// A 422 WITH roots is the handler speaking: the connection works.
func TestPlurxTestPathsTreatsTheHandlers422AsToday(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":["/media"]}`))
	}))
	defer srv.Close()
	if note, err := scanner(t, srv.URL).TestPaths(context.Background(), []string{"/media"}); err != nil || note != "" {
		t.Fatalf("= %q, %v", note, err)
	}
}

func TestPlurxTestPathsFailsOnABadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := scanner(t, srv.URL).TestPaths(context.Background(), []string{"/media"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("= %v, want the 401 named", err)
	}
}

func TestPlurxSendScanNeedsOneDirectoryAndConfig(t *testing.T) {
	_, err := New(ports.NotifierConfig{Type: "plurx", Settings: map[string]string{"url": "http://x"}}).(*Plurx).SendScan(context.Background(), subDelivery("/m"))
	if retryOf(t, err) != ports.RetryNever {
		t.Errorf("no key = %v", err)
	}
	n := subDelivery("/m")
	n.Import.Dirs = []string{"/a", "/b"}
	if _, err := scanner(t, "http://x").SendScan(context.Background(), n); retryOf(t, err) != ports.RetryNever {
		t.Errorf("two directories = %v", err)
	}
}
