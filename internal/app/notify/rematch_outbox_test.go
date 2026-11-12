package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/adapters/notify/cinematest"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A Re-match's moved sub-delivery (contract amendment): keyed by operation,
// path the common parent, expected the placed files, removed_paths the files
// that left. Visible only when every expected file is placed and every
// removed path is gone or unknown.
func (e *outboxEnv) moved(newRel, oldRel string) (string, string) {
	e.t.Helper()
	placed := e.place(newRel, "moved bytes")
	old := filepath.Join(e.root, filepath.FromSlash(oldRel))
	fi, err := os.Stat(placed)
	if err != nil {
		e.t.Fatal(err)
	}
	expected := []ports.ExpectedFile{{Path: placed, Bytes: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}}
	n := ports.Notification{Event: "moved", Title: "Re-matched", Import: &ports.ImportInfo{MediaItemID: 2, FromItemID: 1, Kind: "movie", TmdbID: 438631,
		Paths: []string{placed}, Dirs: []string{e.root}, ExpectedFiles: map[string][]ports.ExpectedFile{e.root: expected}, RemovedPaths: []string{old}}}
	raw, _ := json.Marshal(n)
	if err := e.db.EnqueueDeliveries(e.ctx, []sqlite.OutboxWrite{{NotifierID: 1, Directory: e.root, IdempotencyKey: acquisition.RematchDeliveryKey("rm-1", e.root),
		ExpectedFiles: expected, Event: "moved", Payload: string(raw), OperationID: "rm-1"}}); err != nil {
		e.t.Fatal(err)
	}
	if now := time.Now(); now.After(e.clock) {
		e.clock = now
	}
	return placed, old
}

func TestMovedDeliveryIsVisibleWhenRemovedPathIsGone(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.RemovedPaths = true })
	placed, old := e.moved("Dune (2021) {tmdb-438631}/Dune (2021).mkv", "Dune (1984)/Dune (1984).mkv")
	e.pass()
	rows := e.rows()
	if len(rows) != 1 || rows[0].VisibleAt == 0 || rows[0].Status != "ok" || strings.Contains(rows[0].Result, "warning") {
		t.Fatalf("rows = %+v, want visible with no warning", rows)
	}
	reqs := e.cinema.Requests()
	if len(reqs) != 1 || reqs[0].Event != "moved" || len(reqs[0].Removed) != 1 || reqs[0].Removed[0] != old || reqs[0].Path != e.root || reqs[0].Expected[0].Path != placed {
		t.Fatalf("requests = %+v, want one moved request at the common parent naming the removed path", reqs)
	}
	cfg, _ := e.db.GetNotifier(e.ctx, 1)
	if !cfg.HasFeature(ports.FeatureRemovedPaths) {
		t.Fatalf("features = %v, want removed_paths re-read before the first moved send", cfg.Features)
	}
}

// present is a lasting answer: the row is visible for the new files, names
// the reason, and is not retried.
func TestMovedDeliveryWarnsWhenRemovedPathIsPresent(t *testing.T) {
	for reason, words := range map[string]string{
		"": "it still exists in the library", ports.ReasonStillPresent: "it still exists in the library",
		ports.ReasonSymlinkInPath: "a symlink is in its path", ports.ReasonCaseOnly: "only the case of its name differs",
	} {
		e := newOutboxEnv(t)
		e.cinema.Set(func(c *cinematest.Cinema) { c.RemovedPaths, c.PresentReason = true, reason })
		_, old := e.moved("New/Dune (2021).mkv", "Old/Dune (1984).mkv")
		e.place("Old/Dune (1984).mkv", "still here")
		e.pass()
		r := e.rows()[0]
		if r.VisibleAt == 0 || r.Status != "ok" || !strings.Contains(r.Result, "warning: cinema still has "+old+": "+words) {
			t.Fatalf("reason %q: row = %+v, want a settled warning naming %q", reason, r, words)
		}
	}
}

// retained: expected_not_placed, prune_bound and conversion_hold are
// transient (retried on the schedule); root_identity_* is a lasting warning.
func TestMovedDeliveryRetainedCodes(t *testing.T) {
	for code, transient := range map[string]bool{
		ports.RetainedExpectedNotPlaced: true, ports.RetainedPruneBound: true, ports.RetainedConversionHold: true,
		ports.RetainedRootIdentityUnset: false, ports.RetainedRootIdentityMismatch: false,
	} {
		e := newOutboxEnv(t)
		_, old := e.moved("New/Dune (2021).mkv", "Old/Dune (1984).mkv")
		e.cinema.Set(func(c *cinematest.Cinema) { c.RemovedPaths, c.Retained = true, map[string]string{old: code} })
		e.pass()
		r := e.rows()[0]
		if transient {
			if r.VisibleAt != 0 || r.Status != "pending" || !strings.Contains(r.Result, "cinema is holding "+old) {
				t.Fatalf("%s: row = %+v, want a retry with a warning", code, r)
			}
			continue
		}
		if r.VisibleAt == 0 || r.Status != "ok" || !strings.Contains(r.Result, "cinema kept "+old) || !strings.Contains(r.Result, "it will drop it at its next full scan") {
			t.Fatalf("%s: row = %+v, want a settled warning", code, r)
		}
	}
}

// gone and unknown satisfy the removal.
func TestMovedDeliveryUnknownIsSatisfied(t *testing.T) {
	e := newOutboxEnv(t)
	_, old := e.moved("New/Dune (2021).mkv", "Old/Dune (1984).mkv")
	e.cinema.Set(func(c *cinematest.Cinema) { c.RemovedPaths, c.Unknown = true, map[string]bool{old: true} })
	e.pass()
	if r := e.rows()[0]; r.VisibleAt == 0 || strings.Contains(r.Result, "warning") {
		t.Fatalf("row = %+v, want visible without a warning", r)
	}
}

// Every chunk of a moved request is probe-only, including one that carries
// only expected files.
func TestMovedRequestsAreProbeOnlyOnEveryChunk(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.RemovedPaths = true })
	placed, _ := e.moved("New/Dune (2021).mkv", "Old/Dune (1984).mkv")
	e.pass()
	// A second chunk with expected files and no removed paths.
	fi, err := os.Stat(placed)
	if err != nil {
		t.Fatal(err)
	}
	expected := []ports.ExpectedFile{{Path: placed, Bytes: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}}
	n := ports.Notification{Event: "moved", Title: "Re-matched", Import: &ports.ImportInfo{MediaItemID: 2, FromItemID: 1, Kind: "movie",
		Paths: []string{placed}, Dirs: []string{e.root}, ExpectedFiles: map[string][]ports.ExpectedFile{e.root: expected}}}
	raw, _ := json.Marshal(n)
	if err := e.db.EnqueueDeliveries(e.ctx, []sqlite.OutboxWrite{{NotifierID: 1, Directory: e.root, IdempotencyKey: acquisition.RematchDeliveryKey("rm-1", e.root) + "-1",
		ExpectedFiles: expected, Event: "moved", Payload: string(raw), OperationID: "rm-1"}}); err != nil {
		t.Fatal(err)
	}
	if now := time.Now(); now.After(e.clock) {
		e.clock = now
	}
	e.pass()
	reqs := e.cinema.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v, want two chunks", reqs)
	}
	for i, q := range reqs {
		if !q.ProbeOnly || q.Event != "moved" {
			t.Fatalf("request %d = %+v, want event moved with probe_only", i, q)
		}
	}
}

// Against a Cinema without the feature: the new folder only, as a plain
// scan (no full scan of the root), visible for the new files, with the line
// that the old entry stays. Not a failure, no retry loop.
func TestMovedDeliveryAgainstCinemaWithoutRemovedPaths(t *testing.T) {
	e := newOutboxEnv(t)
	placed, _ := e.moved("New Folder/Dune (2021).mkv", "Old/Dune (1984).mkv")
	e.pass()
	r := e.rows()[0]
	if r.VisibleAt == 0 || r.Status != "ok" || !strings.Contains(r.Result, "the old entry stays until its next scan") {
		t.Fatalf("row = %+v, want visible with the old-cinema line", r)
	}
	reqs := e.cinema.Requests()
	if len(reqs) != 1 || reqs[0].Path != filepath.Dir(placed) || len(reqs[0].Removed) != 0 || reqs[0].Event != "" {
		t.Fatalf("requests = %+v, want only the new folder, without removed paths", reqs)
	}
}

// The backstop: an answer with no entry for a removed path means the
// Cinema does not support them.
func TestJudgeRemovedBackstop(t *testing.T) {
	v := judgeRemoved([]string{"/a", "/b"}, []ports.PlacedReport{{Path: "/a", State: "gone"}})
	if v.answered || len(v.warn)+len(v.retry) != 0 {
		t.Fatalf("verdict = %+v, want unanswered", v)
	}
	v = judgeRemoved([]string{"/a", "/b"}, []ports.PlacedReport{{Path: "/a", State: "unknown"}, {Path: "/b", State: "present"}})
	if !v.answered || len(v.warn) != 1 || len(v.retry) != 0 {
		t.Fatalf("verdict = %+v, want /b a lasting warning", v)
	}
}

// A queued moved request is judged, when read back, by what its POST sent
// (stored on the row), not by its result text.
func TestQueuedMovedRequestIsReadBackByWhatWasSent(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.RemovedPaths, c.Queue = true, true })
	e.moved("New/Dune (2021).mkv", "Old/Dune (1984).mkv")
	e.pass()
	r := e.rows()[0]
	if r.Status != "pending" || r.RemoteRequestID == "" || !r.RemovedSent {
		t.Fatalf("row = %+v, want queued with removed paths recorded as sent", r)
	}
	e.cinema.Release()
	e.clock = e.clock.Add(time.Hour)
	e.pass()
	r = e.rows()[0]
	if r.VisibleAt == 0 || strings.Contains(r.Result, "warning") {
		t.Fatalf("row = %+v, want visible once the read shows the removed path gone", r)
	}
}

// A stored removed_paths feature that cannot be confirmed is not trusted
// with a common-parent request: nothing is posted, and it is tried again.
func TestMovedRequestWaitsWhenFeaturesCannotBeReRead(t *testing.T) {
	e := newOutboxEnv(t)
	if err := e.db.SetNotifierFeatures(e.ctx, 1, []string{ports.FeatureRemovedPaths}); err != nil {
		t.Fatal(err)
	}
	e.moved("New/Dune (2021).mkv", "Old/Dune (1984).mkv")
	e.cinema.Set(func(c *cinematest.Cinema) { c.Down = true })
	e.pass()
	r := e.rows()[0]
	if r.Status != "pending" || !strings.Contains(r.LastError, "could not be re-read") || len(e.cinema.Posts()) != 0 {
		t.Fatalf("row = %+v posts = %v, want nothing posted and a retry", r, e.cinema.Posts())
	}
}
