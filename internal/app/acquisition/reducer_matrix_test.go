package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The reducer's fact x state matrix (pipeline contract v2, section 3.2):
// identity, provisional re-open, holds, the cursor's single owner, and
// continuity loss. The Runner is the fake from pipeline_v2_test.go; the
// database and the reducer are the real ones.

func boolPtr(b bool) *bool { return &b }

// A request receipt is read as facts: admitted, and the terminal the receipt
// records, typed when Runner M1a wrote the fields and by the legacy string
// rule otherwise.
func TestAdmissionReceiptAsFacts(t *testing.T) {
	cases := []struct {
		name     string
		adm      ports.RequestAdmission
		terminal bool
		want     ports.RunnerTerminal
	}{
		{name: "completed without a publication path is no terminal", adm: ports.RequestAdmission{Outcome: "completed"}},
		{name: "completed", adm: ports.RequestAdmission{Outcome: "completed", PublicationPath: "/p/x", SourceGeneration: "g1"}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Salvage: "none"}},
		{name: "deleted is cancelled", adm: ports.RequestAdmission{Outcome: "deleted"}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeCancelled, Salvage: "none", Message: "removed in Runner"}},
		{name: "cancelled", adm: ports.RequestAdmission{Outcome: "cancelled"}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeCancelled, Salvage: "none", Message: "removed in Runner"}},
		{name: "typed failure keeps its blame and salvage",
			adm: ports.RequestAdmission{Outcome: "failed", FailureReason: ports.ReasonFetch, FetchCause: ports.FetchCauseGone, FailureStage: "fetch",
				FailureCause: "404", BlamesRelease: boolPtr(true), Salvage: "pending"}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonFetch, FetchCause: ports.FetchCauseGone, Stage: "fetch",
				Message: "404", BlamesRelease: true, Salvage: "pending"}},
		{name: "typed failure without salvage says none",
			adm: ports.RequestAdmission{Outcome: "failed", FailureReason: ports.ReasonIO}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonIO, Salvage: "none"}},
		{name: "legacy string failure with its cause",
			adm: ports.RequestAdmission{Outcome: "failed", TerminalOutcome: "UNPACK_FAILURE", FailureCause: "crc"}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonUnpack, Stage: "unpack", Message: "UNPACK_FAILURE: crc",
				BlamesRelease: true, Salvage: "none", LegacyPPStatus: "UNPACK_FAILURE"}},
		{name: "a failed receipt saying SUCCESS is an io failure, never a completion",
			adm: ports.RequestAdmission{Outcome: "failed", TerminalOutcome: "SUCCESS"}, terminal: true,
			want: ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonIO, Message: "SUCCESS", Salvage: "none"}},
		{name: "an admitted receipt has no terminal", adm: ports.RequestAdmission{Outcome: "admitted"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := admissionTerminal(tc.adm)
			if ok != tc.terminal {
				t.Fatalf("terminal = %v, want %v", ok, tc.terminal)
			}
			if !ok {
				return
			}
			pub := got.Publication
			got.Publication = nil
			if got != tc.want {
				t.Fatalf("terminal = %+v\nwant       %+v", got, tc.want)
			}
			if tc.want.Outcome == ports.OutcomeCompleted && (pub == nil || pub.Path != "/p/x" || pub.Generation != "g1" || pub.Kind != "completed") {
				t.Fatalf("publication = %+v", pub)
			}
		})
	}
	facts := admissionFacts(ports.RequestAdmission{RequestToken: "t-1", JobID: "7", Outcome: "cancelled"})
	if len(facts) != 2 || facts[0].Kind != ports.FactAdmitted || facts[0].Handle != "7" || facts[1].Kind != ports.FactTerminal || facts[1].Transfer != "t-1" {
		t.Fatalf("facts = %+v, want admitted then the terminal", facts)
	}
	if facts := admissionFacts(ports.RequestAdmission{RequestToken: "t-1", JobID: "0", Outcome: "admitted"}); len(facts) != 0 {
		t.Fatalf("job 0 with no outcome = %+v, want no facts", facts)
	}
}

// What a failed job fetched, for the re-grab budget: articles first, then
// bytes against the release size, else unmeasured.
func TestFetchedProgress(t *testing.T) {
	cases := []struct {
		f    ports.Fetched
		size int64
		want float64
	}{
		{ports.Fetched{SuccessArticles: 25, TotalArticles: 100}, 0, 0.25},
		{ports.Fetched{SuccessArticles: 200, TotalArticles: 100}, 0, 1},
		{ports.Fetched{Bytes: 500}, 1000, 0.5},
		{ports.Fetched{Bytes: 5000}, 1000, 1},
		{ports.Fetched{Bytes: 500}, 0, -1},
		{ports.Fetched{}, 1000, -1},
	}
	for _, tc := range cases {
		if got := fetchedProgress(tc.f, tc.size); got != tc.want {
			t.Errorf("fetchedProgress(%+v, %d) = %v, want %v", tc.f, tc.size, got, tc.want)
		}
	}
}

// Whether a failure writes a replacement grab, and why not (contract 3.3).
func TestReplacementDueMatrix(t *testing.T) {
	s := &Service{}
	want := []string{"episode:1:1:1"}
	cases := []struct {
		name        string
		dl          sqlite.Download
		t           ports.RunnerTerminal
		provisional bool
		grab        bool
		why         string
	}{
		{"provisional", sqlite.Download{WantableIDs: want}, failed(ports.ReasonSubmit, false), true, false, "provisional (submit)"},
		{"cancelled", sqlite.Download{WantableIDs: want}, ports.RunnerTerminal{Outcome: ports.OutcomeCancelled}, false, false, "cancelled"},
		{"indexer outage", sqlite.Download{WantableIDs: want}, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonFetch, FetchCause: ports.FetchCauseIndexerUnavailable}, false, false, "indexer was unavailable"},
		{"rejected submission", sqlite.Download{WantableIDs: want, SubmissionPhase: "rejected"}, failed(ports.ReasonHealth, true), false, false, "refused the submission"},
		{"manual additional", sqlite.Download{WantableIDs: want, Intent: GrabIntentManualAdditional}, failed(ports.ReasonHealth, true), false, false, "one-shot"},
		{"replacement operation", sqlite.Download{WantableIDs: want, ReplacementOperationID: "op-1"}, failed(ports.ReasonHealth, true), false, false, "operation owns the next grab"},
		{"no wantable", sqlite.Download{}, failed(ports.ReasonHealth, true), false, false, "names no wantable"},
		{"blamed failure", sqlite.Download{WantableIDs: want}, failed(ports.ReasonHealth, true), false, true, ""},
		{"io failure still grabs", sqlite.Download{WantableIDs: want}, failed(ports.ReasonIO, false), false, true, ""},
	}
	for _, tc := range cases {
		wants, why := s.replacementDue(tc.dl, tc.t, tc.provisional)
		if (len(wants) > 0) != tc.grab || !strings.Contains(why, tc.why) {
			t.Errorf("%s: wants=%v why=%q, want grab=%v why~%q", tc.name, wants, why, tc.grab, tc.why)
		}
		if tc.grab && (len(wants) != 1 || wants[0] != want[0]) {
			t.Errorf("%s: wants = %v, want a copy of the row's wantables", tc.name, wants)
		}
	}
}

// Status facts by row state: what moves, what is ignored, and how a hold's
// deadline is chosen.
func TestStatusFactMatrix(t *testing.T) {
	deadline := int64(600)
	cases := []struct {
		name      string
		prepare   func(f *v2Fixture, dl sqlite.Download)
		status    func(dl sqlite.Download) ports.DownloadStatus
		wantState string
		wantError string
		check     func(t *testing.T, before, got sqlite.Download)
	}{
		{name: "removed in the queue is not a terminal",
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateRemoved}
			},
			wantState: "downloading"},
		{name: "completed in the queue waits for Runner's record",
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateCompleted}
			},
			wantState: "downloading",
			check: func(t *testing.T, _, got sqlite.Download) {
				if got.RunnerOutcome != "" {
					t.Fatal("a queue completion recorded a terminal")
				}
			}},
		{name: "a failed queue state is shown, not decided",
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateFailed, Message: "par2 missing", Progress: 0.3}
			},
			wantState: "downloading", wantError: "par2 missing",
			check: func(t *testing.T, _, got sqlite.Download) {
				if got.RunnerOutcome != "" || got.Progress != 0.3 {
					t.Fatalf("outcome %q progress %.2f", got.RunnerOutcome, got.Progress)
				}
			}},
		{name: "zero progress keeps the last measured progress",
			prepare: func(f *v2Fixture, dl sqlite.Download) {
				st := ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateDownloading, Progress: 0.6}
				f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "poll"})
			},
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateDownloading}
			},
			wantState: "downloading",
			check: func(t *testing.T, _, got sqlite.Download) {
				if got.Progress != 0.6 {
					t.Fatalf("progress = %.2f, want the stored 0.6", got.Progress)
				}
			}},
		{name: "an older Runner's hold gets the legacy 24 h deadline",
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued, OperationalState: "held", Message: "no space"}
			},
			wantState: "held", wantError: "no space",
			check: func(t *testing.T, _, got sqlite.Download) {
				if d := time.Until(got.DeadlineAt); d < 23*time.Hour || d > 25*time.Hour {
					t.Fatalf("deadline in %s, want about 24 h", d)
				}
				if last := got.Handoff[len(got.Handoff)-1]; last.Step != stepHeld || !strings.Contains(last.Detail, "no space") {
					t.Fatalf("handoff = %+v, want the hold traced with its reason", last)
				}
			}},
		{name: "a repeated legacy hold keeps its first deadline",
			prepare: func(f *v2Fixture, dl sqlite.Download) {
				st := ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued, OperationalState: "held"}
				f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "poll"})
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET deadline_at=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), dl.ID); err != nil {
					f.t.Fatal(err)
				}
			},
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued, OperationalState: "held"}
			},
			wantState: "held",
			check: func(t *testing.T, before, got sqlite.Download) {
				if !got.DeadlineAt.Equal(before.DeadlineAt) {
					t.Fatalf("deadline moved from %s to %s", before.DeadlineAt, got.DeadlineAt)
				}
				holds := 0
				for _, h := range got.Handoff {
					if h.Step == stepHeld {
						holds++
					}
				}
				if holds != 1 {
					t.Fatalf("held handoff entries = %d, want one for the whole hold", holds)
				}
			}},
		{name: "a hold message comes from the control when the status has none",
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued,
					Control: &ports.DownloadControl{Version: 1, Lifecycle: "held", Message: "disk full", DeadlineInS: &deadline}}
			},
			wantState: "held", wantError: "disk full",
			check: func(t *testing.T, _, got sqlite.Download) {
				if d := time.Until(got.DeadlineAt); d < 14*time.Minute || d > 16*time.Minute {
					t.Fatalf("deadline in %s, want Runner's 10 min + 5 min grace", d)
				}
				if !strings.Contains(got.RunnerControl, "disk full") {
					t.Fatalf("control = %q, want stored for display", got.RunnerControl)
				}
			}},
		{name: "a released hold is traced",
			prepare: func(f *v2Fixture, dl sqlite.Download) {
				st := ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued, OperationalState: "held"}
				f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "poll"})
			},
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateDownloading, Progress: 0.1}
			},
			wantState: "downloading",
			check: func(t *testing.T, _, got sqlite.Download) {
				if last := got.Handoff[len(got.Handoff)-1]; last.Step != stepDownloading || !strings.Contains(last.Detail, "released the hold") {
					t.Fatalf("handoff = %+v, want the release traced", last)
				}
				if !got.DeadlineAt.After(time.Now().Add(20 * time.Hour)) {
					t.Fatalf("deadline %s, want a fresh torrent deadline", got.DeadlineAt)
				}
			}},
		{name: "a terminal row ignores later status",
			prepare: func(f *v2Fixture, dl sqlite.Download) {
				f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCancelled, Salvage: "none"}, "event 1"))
			},
			status: func(dl sqlite.Download) ports.DownloadStatus {
				return ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateDownloading, Progress: 0.9}
			},
			wantState: "failed",
			check: func(t *testing.T, _, got sqlite.Download) {
				if got.Progress == 0.9 {
					t.Fatal("status after the terminal moved progress")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			f.deferGrabs()
			dl := f.grab(1, epOne)
			if tc.prepare != nil {
				tc.prepare(f, dl)
			}
			before := f.row(dl.ID)
			st := tc.status(before)
			f.observe(before, ports.RunnerFact{Kind: ports.FactStatus, Transfer: before.Transfer, Status: &st, Source: "poll"})
			got := f.row(dl.ID)
			if got.State != tc.wantState || (tc.wantError != "" && got.Error != tc.wantError) {
				t.Fatalf("row = %s %q, want %s %q", got.State, got.Error, tc.wantState, tc.wantError)
			}
			if tc.check != nil {
				tc.check(t, before, got)
			}
		})
	}
}

// A submission whose response was lost is bound by the first fact naming its
// job: a status with a handle admits it and starts it downloading.
func TestStatusAdmitsASubmittingRow(t *testing.T) {
	f := setupV2(t, nil)
	f.runner.addErr = errors.New("connection reset")
	id, _ := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: 1, Title: epOne,
		DownloadURL: "http://dl/" + epOne, Indexer: "idx", Protocol: "torrent", Size: 1000})
	f.runner.addErr = nil
	dl := f.row(id)
	if dl.State != "submitting" || dl.Handle != "" {
		t.Fatalf("row = %s handle %q, want submitting without a job", dl.State, dl.Handle)
	}
	st := ports.DownloadStatus{Handle: "555", Transfer: dl.Transfer, State: ports.StateDownloading, Progress: 0.2, Message: "fetching articles"}
	f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Handle: "555", Status: &st, Source: "poll"})
	got := f.row(id)
	if got.Handle != "555" || got.State != "downloading" || got.SubmissionPhase != "submitted" {
		t.Fatalf("row = %s handle %q phase %s, want admitted and downloading", got.State, got.Handle, got.SubmissionPhase)
	}
	// A grabbed row the queue now lists starts downloading, traced in
	// Runner's own words.
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='grabbed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	f.observe(f.row(id), ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "poll"})
	got = f.row(id)
	if last := got.Handoff[len(got.Handoff)-1]; got.State != "downloading" || last.Step != stepDownloading || !strings.Contains(last.Detail, "fetching articles") {
		t.Fatalf("row %s handoff = %+v, want downloading in Runner's own words", got.State, last)
	}
}

// Terminal shapes: what each records on the row, its history and its
// blocklist.
func TestTerminalShapeMatrix(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(f *v2Fixture, dl sqlite.Download) sqlite.Download
		cfg     func(cfg ports.ClientConfig) ports.ClientConfig
		t       func(f *v2Fixture) ports.RunnerTerminal
		check   func(t *testing.T, f *v2Fixture, dl sqlite.Download)
	}{
		{name: "a completion without its publication waits for the path",
			t: func(*v2Fixture) ports.RunnerTerminal {
				return ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Salvage: "none"}
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.RunnerOutcome != "" || dl.State != "downloading" || f.count(`SELECT COUNT(*) FROM publications`) != 0 {
					t.Fatalf("row = %s/%s, want nothing recorded", dl.State, dl.RunnerOutcome)
				}
			}},
		{name: "a remote path is mapped and both are traced",
			cfg: func(cfg ports.ClientConfig) ports.ClientConfig {
				cfg.PathMappings = []ports.PathMapping{{Remote: "/runner/completed", Local: "LOCAL"}}
				return cfg
			},
			t: func(f *v2Fixture) ports.RunnerTerminal {
				f.publish("Test.Show.S01E01.job-101.runner-map", "map", map[string]string{epOne + ".mkv": "video"})
				return ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Salvage: "none",
					Publication: &ports.RunnerPublication{Path: "/runner/completed/Test.Show.S01E01.job-101.runner-map", Generation: "map", Kind: "completed"}}
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.State != "imported" || dl.SavePath != "/runner/completed/Test.Show.S01E01.job-101.runner-map" {
					t.Fatalf("row = %s save %q (%s), want imported from the mapped path", dl.State, dl.SavePath, dl.Error)
				}
				found := false
				for _, h := range dl.Handoff {
					if h.Step == stepDownloaded && strings.Contains(h.Detail, "(mapped to "+f.root) {
						found = true
					}
				}
				if !found {
					t.Fatalf("handoff = %+v, want the mapping traced", dl.Handoff)
				}
			}},
		{name: "a cancelled terminal keeps Runner's message",
			t: func(*v2Fixture) ports.RunnerTerminal {
				return ports.RunnerTerminal{Outcome: ports.OutcomeCancelled, Message: "deleted by Paul in Runner", Salvage: "none"}
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.State != "failed" || dl.Error != "deleted by Paul in Runner" {
					t.Fatalf("row = %s %q", dl.State, dl.Error)
				}
			}},
		{name: "a legacy client's words are the whole reason",
			t: func(*v2Fixture) ports.RunnerTerminal {
				return ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: "client", Message: "tracker unregistered", Salvage: "none"}
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.Error != "tracker unregistered" {
					t.Fatalf("error = %q", dl.Error)
				}
			}},
		{name: "a failure with no words says failed and counts in full",
			t: func(*v2Fixture) ports.RunnerTerminal {
				return ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Salvage: "none"}
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.Error != "failed" || dl.Progress != 1 {
					t.Fatalf("error %q progress %.2f, want failed and 1", dl.Error, dl.Progress)
				}
			}},
		{name: "a reason with the same message is not repeated",
			t: func(*v2Fixture) ports.RunnerTerminal {
				return ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonPar, Message: ports.ReasonPar, Salvage: "none"}
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.Error != ports.ReasonPar {
					t.Fatalf("error = %q", dl.Error)
				}
			}},
		{name: "fetched articles set the budget's progress",
			t: func(*v2Fixture) ports.RunnerTerminal {
				tm := failed(ports.ReasonHealth, true)
				tm.Fetched = ports.Fetched{SuccessArticles: 40, TotalArticles: 100}
				return tm
			},
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if dl.Progress != 0.4 {
					t.Fatalf("progress = %.2f, want 0.4 from the articles", dl.Progress)
				}
			}},
		{name: "a refused submission is not blocklisted and not replaced",
			prepare: func(f *v2Fixture, dl sqlite.Download) sqlite.Download {
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET submission_phase='rejected' WHERE id=?`, dl.ID); err != nil {
					f.t.Fatal(err)
				}
				return f.row(dl.ID)
			},
			t: func(*v2Fixture) ports.RunnerTerminal { return failed(ports.ReasonHealth, true) },
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if len(mustBlocklist(t, f)) != 0 || f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 {
					t.Fatal("a refused submission was blocklisted or replaced")
				}
				if f.count(`SELECT COUNT(*) FROM history_events WHERE type=? AND data LIKE '%refused the submission%'`, HistoryNoReplacement) != 1 {
					t.Fatal("no_replacement reason not recorded")
				}
			}},
		{name: "a row naming no wantable writes no grab and says so",
			prepare: func(f *v2Fixture, dl sqlite.Download) sqlite.Download {
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET wantables='[]' WHERE id=?`, dl.ID); err != nil {
					f.t.Fatal(err)
				}
				return f.row(dl.ID)
			},
			t: func(*v2Fixture) ports.RunnerTerminal { return failed(ports.ReasonHealth, true) },
			check: func(t *testing.T, f *v2Fixture, dl sqlite.Download) {
				if f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 {
					t.Fatal("a grab was written for no wantable")
				}
				if len(mustBlocklist(t, f)) != 1 {
					t.Fatal("the blamed release was not blocklisted")
				}
				if f.count(`SELECT COUNT(*) FROM history_events WHERE type=? AND data LIKE '%names no wantable%'`, HistoryNoReplacement) != 1 {
					t.Fatal("no_replacement reason not recorded")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			f.deferGrabs()
			dl := f.grab(1, epOne)
			if tc.prepare != nil {
				dl = tc.prepare(f, dl)
			}
			cfg := f.cfg
			if tc.cfg != nil {
				cfg = tc.cfg(cfg)
				for i := range cfg.PathMappings {
					if cfg.PathMappings[i].Local == "LOCAL" {
						cfg.PathMappings[i].Local = f.root
					}
				}
			}
			tm := tc.t(f)
			if err := f.svc.observe(f.ctx, dl, cfg, terminalFact(dl, tm, "event 1")); err != nil {
				t.Fatal(err)
			}
			tc.check(t, f, f.row(dl.ID))
		})
	}
}

// A legacy client's own measure of what it fetched is used when the
// terminal carries none.
func TestLegacyProgressFeedsTheBudget(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	won, err := f.svc.recordTerminalWith(f.ctx, dl, f.cfg, failed(ports.ReasonHealth, true), "legacy poll", 0, false, 1.7)
	if err != nil || !won {
		t.Fatalf("won=%v err=%v", won, err)
	}
	if got := f.row(dl.ID); got.Progress != 1 {
		t.Fatalf("progress = %.2f, want the legacy measure capped at 1", got.Progress)
	}
	again, err := f.svc.recordTerminalWith(f.ctx, dl, f.cfg, failed(ports.ReasonHealth, true), "legacy poll", 0, false, 0.5)
	if err != nil || again {
		t.Fatalf("a replayed terminal won again (%v, %v)", again, err)
	}
}

// A failed attempt of a replacement operation hands the next grab back to the
// operation, which advances with the failed bytes retained (contract 3.3).
func TestReplacementOperationOwnsTheNextGrab(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	op, _, err := f.db.CreateReplacementOperation(f.ctx, sqlite.ReplacementOperation{ID: "op-reducer", RequestID: "op-reducer", Fingerprint: "op-reducer",
		State: "replacing", Intent: "replace_quality", Scope: "download", MediaItemID: f.item, ReasonCode: "quality_preference", Reason: "better copy"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET replacement_operation_id=? WHERE id=?`, op.ID, dl.ID); err != nil {
		t.Fatal(err)
	}
	dl = f.row(dl.ID)
	f.observe(dl, terminalFact(dl, failed(ports.ReasonHealth, true), "event 1"))
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs`); n != 0 {
		t.Fatalf("replacement_grabs = %d, want the operation to own the grab", n)
	}
	got, err := f.db.GetReplacementOperation(f.ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "replacing" || !strings.Contains(got.LastError, "choosing another release") {
		t.Fatalf("operation = %s %q, want replacing with the retry reason", got.State, got.LastError)
	}
}

// Rows the reducer does not touch, and facts with nothing in them.
func TestObserveIgnoresWhatIsNotAFact(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	for _, fact := range []ports.RunnerFact{
		{Kind: ports.FactStatus, Transfer: dl.Transfer, Source: "poll"},
		{Kind: ports.FactTerminal, Transfer: dl.Transfer, Source: "poll"},
		{Kind: ports.FactPublicationReady, Transfer: dl.Transfer, Source: "poll"},
		{Kind: "unknown", Transfer: dl.Transfer, Source: "poll"},
	} {
		f.observe(dl, fact)
	}
	if got := f.row(dl.ID); got.State != "downloading" || got.RunnerOutcome != "" {
		t.Fatalf("empty facts moved the row to %s/%s", got.State, got.RunnerOutcome)
	}
	// A caller holding a row from another client is not this row's fact.
	other := dl
	other.ClientID = 99
	tm := failed(ports.ReasonHealth, true)
	f.observe(other, terminalFact(other, tm, "poll"))
	if f.row(dl.ID).RunnerOutcome != "" {
		t.Fatal("a fact for a stale row identity was applied")
	}
	// A planned row is not on the reducer yet.
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='planned' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.observe(f.row(dl.ID), terminalFact(dl, tm, "poll"))
	if f.row(dl.ID).RunnerOutcome != "" {
		t.Fatal("a planned row took a terminal")
	}
}

// A terminal naming a job reaches a row that never learned its handle, and
// binds the handle first.
func TestTerminalBindsTheHandleFirst(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	f.runner.addErr = errors.New("connection reset")
	id, _ := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: 1, Title: epOne,
		DownloadURL: "http://dl/" + epOne, Indexer: "idx", Protocol: "torrent", Size: 1000})
	f.runner.addErr = nil
	dl := f.row(id)
	tm := failed(ports.ReasonHealth, true)
	f.observe(dl, ports.RunnerFact{Kind: ports.FactTerminal, Transfer: dl.Transfer, Handle: "321", Terminal: &tm, Source: "history"})
	got := f.row(id)
	if got.Handle != "321" || got.RunnerOutcome != "failed" {
		t.Fatalf("row handle %q outcome %q, want the job bound and the failure recorded", got.Handle, got.RunnerOutcome)
	}
}

// A provisional submit failure with no job yet re-opens to submitting, not
// downloading: Runner knows the token but has not said which job it is.
func TestProvisionalReopenWithoutAJobIsSubmitting(t *testing.T) {
	f := setupV2(t, nil)
	f.runner.addErr = errors.New("connection reset")
	id, _ := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: 1, Title: epOne,
		DownloadURL: "http://dl/" + epOne, Indexer: "idx", Protocol: "torrent", Size: 1000})
	f.runner.addErr = nil
	dl := f.row(id)
	if _, err := f.svc.recordTerminal(f.ctx, dl, f.cfg, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonSubmit}, "admission", 0, true); err != nil {
		t.Fatal(err)
	}
	if got := f.row(id); !got.Provisional {
		t.Fatal("submit failure is not provisional")
	}
	// Runner's receipt says it holds the token but names no job yet.
	f.observe(f.row(id), ports.RunnerFact{Kind: ports.FactAdmitted, Transfer: dl.Transfer, Source: "admission"})
	got := f.row(id)
	if got.Provisional || got.RunnerOutcome != "" || got.State != "submitting" {
		t.Fatalf("row = %s/%s provisional=%v, want re-opened to submitting", got.State, got.RunnerOutcome, got.Provisional)
	}
	if last := got.Handoff[len(got.Handoff)-1]; last.Step != stepReopened {
		t.Fatalf("handoff = %+v, want the re-open traced", last)
	}
}

// A completed row from the deliveries feed for a download Runner finally
// failed is a salvage: the row stays failed, a salvage publication imports.
func TestDeliveriesCompletionOfAFailedRowIsASalvage(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	tm := failed(ports.ReasonHealth, true)
	tm.Salvage = "pending"
	f.observe(dl, terminalFact(dl, tm, "event 1"))
	pub := f.publish("Test.Show.S01E01.job-101.runner-salv", "salv", map[string]string{epOne + ".mkv": "salvaged"})
	pub.Kind = ""
	failedRow := f.row(dl.ID)
	f.observe(failedRow, terminalFact(failedRow, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "deliveries 4"))
	got := f.row(dl.ID)
	p, ok, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	if !ok || p.Kind != "salvage" || p.State != "imported" {
		t.Fatalf("publication = %+v, want an imported salvage", p)
	}
	if got.State != "failed" || got.RunnerOutcome != "failed" {
		t.Fatalf("row = %s/%s, want still failed for re-grab purposes", got.State, got.RunnerOutcome)
	}
	if f.count(`SELECT COUNT(*) FROM history_events WHERE type=?`, stepSalvage) != 1 {
		t.Fatal("salvage history not recorded")
	}
	// The same salvage again is a no-op: one publication, one history row.
	m := ports.Manifest{Generation: "salv"}
	pub.Manifest = &m
	f.observe(got, ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: dl.Transfer, Publication: &pub, Source: "event 9"})
	if f.count(`SELECT COUNT(*) FROM publications`) != 1 || f.count(`SELECT COUNT(*) FROM history_events WHERE type=?`, stepSalvage) != 1 {
		t.Fatal("a replayed salvage made a second publication")
	}
	// A salvage with no path is not actionable.
	empty := ports.RunnerPublication{Kind: "salvage"}
	f.observe(got, ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: dl.Transfer, Publication: &empty, Source: "event 10"})
	if f.count(`SELECT COUNT(*) FROM publications`) != 1 {
		t.Fatal("a pathless salvage made a publication")
	}
}

// A v1 record arriving after a manifest-less publication was recorded is
// attached to it, once, and only to its own download's publication.
func TestManifestIsAttachedToAManifestlessPublication(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-att", "att", map[string]string{epOne + ".mkv": "video"})
	raw := mustReadManifest(t, pub.Path)
	if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "att", Kind: "completed", Path: pub.Path, Cohort: "live", Source: "none"}); err != nil {
		t.Fatal(err)
	}
	f.svc.attachManifest(f.ctx, dl.ID+1, pub.Path, raw) // another download's: ignored
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	if p.ManifestSource != "none" {
		t.Fatalf("a foreign download attached a manifest: %s", p.ManifestSource)
	}
	f.svc.attachManifest(f.ctx, dl.ID, pub.Path, raw)
	p, _, _ = f.db.PublicationByPath(f.ctx, pub.Path)
	if p.ManifestSource != "v1_record" || !strings.Contains(p.ManifestJSON, "att") {
		t.Fatalf("publication = %s %q, want the v1 record attached", p.ManifestSource, p.ManifestJSON)
	}
	raw.Source = "file"
	f.svc.attachManifest(f.ctx, dl.ID, pub.Path, raw)
	if p, _, _ = f.db.PublicationByPath(f.ctx, pub.Path); p.ManifestSource != "v1_record" {
		t.Fatal("a second manifest replaced the first")
	}
}

// historyFeed wraps the fake Runner's history with failures.
type historyFeed struct {
	*fakeRunner
	err error
}

func (h historyFeed) HistorySince(ctx context.Context, c ports.HistoryCursor, limit int) ([]ports.RunnerFact, ports.HistoryCursor, bool, error) {
	if h.err != nil {
		return nil, c, false, h.err
	}
	return h.fakeRunner.HistorySince(ctx, c, limit)
}

func (h historyFeed) DeliveriesSince(ctx context.Context, after uint64, limit int) ([]ports.RunnerFact, uint64, error) {
	if h.err != nil {
		return nil, after, h.err
	}
	return h.fakeRunner.DeliveriesSince(ctx, after, limit)
}

// The history cursor: who moves it, and when it must not move.
func TestHistoryCursorMatrix(t *testing.T) {
	t.Run("first poll reconciles, then trusts the head", func(t *testing.T) {
		f := setupV2(t, nil)
		f.runner.incarnation = "inc"
		f.runner.addHistory(ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-unrelated", HistorySeq: 12,
			Terminal: &ports.RunnerTerminal{Outcome: ports.OutcomeCancelled}})
		f.svc.pollHistory(f.ctx, f.cfg, f.runner)
		c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID)
		if !c.Stored || c.Incarnation != "inc" || c.SinceSeq != 12 {
			t.Fatalf("cursor = %+v, want stored at the head", c)
		}
	})
	t.Run("a read error leaves the cursor", func(t *testing.T) {
		f := setupV2(t, nil)
		_ = f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "inc", SinceSeq: 3})
		f.svc.pollHistory(f.ctx, f.cfg, historyFeed{f.runner, errors.New("runner down")})
		if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.SinceSeq != 3 {
			t.Fatalf("cursor = %d, want unchanged 3", c.SinceSeq)
		}
	})
	t.Run("no new rows but a new head records the head only", func(t *testing.T) {
		f := setupV2(t, nil)
		f.runner.incarnation = "inc"
		f.runner.retainedMin = 2
		_ = f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "inc", SinceSeq: 9})
		f.runner.addHistory(ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-old", HistorySeq: 9,
			Terminal: &ports.RunnerTerminal{Outcome: ports.OutcomeCancelled}})
		f.svc.pollHistory(f.ctx, f.cfg, f.runner)
		c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID)
		if c.SinceSeq != 9 || c.HeadSeq != 9 || c.RetainedMinSeq != 2 {
			t.Fatalf("cursor = %+v, want since 9 with the head and retention recorded", c)
		}
	})
	t.Run("a fact that cannot be applied holds the cursor", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		f.runner.incarnation = "inc"
		_ = f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "inc", SinceSeq: 1})
		fact := terminalFact(dl, failed(ports.ReasonHealth, true), "history inc:2")
		fact.HistorySeq = 2
		f.runner.addHistory(fact)
		if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER outage BEFORE UPDATE OF runner_outcome ON downloads BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
			t.Fatal(err)
		}
		f.svc.pollHistory(f.ctx, f.cfg, f.runner)
		if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.SinceSeq != 1 {
			t.Fatalf("cursor = %d, want held at 1 while its page is uncommitted", c.SinceSeq)
		}
		if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER outage`); err != nil {
			t.Fatal(err)
		}
		f.deferGrabs()
		f.svc.pollHistory(f.ctx, f.cfg, f.runner)
		if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.SinceSeq != 2 || f.row(dl.ID).RunnerOutcome != "failed" {
			t.Fatalf("cursor = %d outcome %q, want the replayed page applied and the cursor at 2", c.SinceSeq, f.row(dl.ID).RunnerOutcome)
		}
	})
	t.Run("continuity loss with an unreachable receipt store does not move the cursor", func(t *testing.T) {
		f := setupV2(t, nil)
		f.grab(1, epOne)
		_ = f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "old", SinceSeq: 50})
		f.runner.incarnation = "rebuilt"
		f.runner.admitErr = errors.New("receipts unavailable")
		f.svc.pollHistory(f.ctx, f.cfg, f.runner)
		if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.Incarnation != "old" || c.SinceSeq != 50 {
			t.Fatalf("cursor = %+v, want the old one kept until every transfer is looked up", c)
		}
		// Receipts back, the transfer unknown to them: the cursor is reset.
		f.runner.admitErr = nil
		f.svc.pollHistory(f.ctx, f.cfg, f.runner)
		if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.Incarnation != "rebuilt" {
			t.Fatalf("cursor = %+v, want the new incarnation", c)
		}
	})
	t.Run("a cancelled poll stops the continuity lookups", func(t *testing.T) {
		f := setupV2(t, nil)
		f.grab(1, epOne)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if err := f.svc.reconcileAfterContinuityLoss(ctx, f.cfg); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want cancelled", err)
		}
	})
	t.Run("a non-Runner client has nothing to reconcile", func(t *testing.T) {
		f := setupV2(t, nil)
		f.grab(1, epOne)
		f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return &f.runner.fakeClient }
		if err := f.svc.reconcileAfterContinuityLoss(f.ctx, f.cfg); err != nil {
			t.Fatal(err)
		}
	})
}

// The deliveries feed raises facts page by page; its cursor moves only past
// pages whose facts applied.
func TestDeliveriesFeedCursor(t *testing.T) {
	f := setupV2(t, nil)
	for i := 0; i < 150; i++ {
		f.runner.deliveries = append(f.runner.deliveries, ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-none",
			Terminal: &ports.RunnerTerminal{Outcome: ports.OutcomeCancelled}, Source: "deliveries"})
	}
	f.svc.pollDeliveries(f.ctx, f.cfg, f.runner)
	if c, _ := f.db.DeliveryCursor(f.ctx, f.cfg.ID); c != 150 {
		t.Fatalf("delivery cursor = %d, want 150 after two pages", c)
	}
	f.runner.deliveries = append(f.runner.deliveries, ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-late",
		Terminal: &ports.RunnerTerminal{Outcome: ports.OutcomeCancelled}})
	f.svc.pollDeliveries(f.ctx, f.cfg, historyFeed{f.runner, errors.New("feed down")})
	if c, _ := f.db.DeliveryCursor(f.ctx, f.cfg.ID); c != 150 {
		t.Fatalf("delivery cursor = %d after a read error, want unchanged", c)
	}
	// A fact for a row that cannot take it holds the cursor.
	dl := f.grab(1, epOne)
	f.runner.deliveries = append(f.runner.deliveries, terminalFact(dl, failed(ports.ReasonHealth, true), "deliveries 152"))
	if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER outage BEFORE UPDATE OF runner_outcome ON downloads BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	f.svc.pollDeliveries(f.ctx, f.cfg, f.runner)
	if c, _ := f.db.DeliveryCursor(f.ctx, f.cfg.ID); c != 150 {
		t.Fatalf("delivery cursor = %d, want held at 150", c)
	}
}

// Absence and admission confirmation: Runner's positive "no such token" over
// successful polls ends a row provisionally; an unreachable Runner never does.
func TestAbsenceAndAdmissionMatrix(t *testing.T) {
	t.Run("absent within the grace is waited for", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		f.svc.detectGone(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{})
		if got := f.row(dl.ID); got.RunnerOutcome != "" {
			t.Fatal("a fresh absence was given up on")
		}
	})
	t.Run("an unreachable receipt store resets the absence", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		f.svc.absentSince.Store(dl.ID, time.Now().Add(-time.Hour))
		f.runner.admitErr = errors.New("timeout")
		f.svc.detectGone(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{})
		if _, still := f.svc.absentSince.Load(dl.ID); still || f.row(dl.ID).RunnerOutcome != "" {
			t.Fatal("an unreachable Runner counted towards gone")
		}
	})
	t.Run("a receipt found while absent applies its terminal", func(t *testing.T) {
		f := setupV2(t, nil)
		f.deferGrabs()
		dl := f.grab(1, epOne)
		f.svc.absentSince.Store(dl.ID, time.Now().Add(-time.Hour))
		f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: dl.Handle, Outcome: "cancelled"}
		f.svc.detectGone(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{})
		if got := f.row(dl.ID); got.RunnerOutcome != ports.OutcomeCancelled || got.Provisional {
			t.Fatalf("row = %s provisional=%v, want Runner's cancelled", got.RunnerOutcome, got.Provisional)
		}
	})
	t.Run("seen rows and rows without a job are never absent", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		f.svc.absentSince.Store(dl.ID, time.Now().Add(-time.Hour))
		f.svc.detectGone(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{dl.ID: true})
		if _, still := f.svc.absentSince.Load(dl.ID); still {
			t.Fatal("a seen row kept its absence clock")
		}
	})
	t.Run("a submitting row's receipt admits it", func(t *testing.T) {
		f := setupV2(t, nil)
		f.runner.addErr = errors.New("connection reset")
		id, _ := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: 1, Title: epOne,
			DownloadURL: "http://dl/" + epOne, Indexer: "idx", Protocol: "torrent", Size: 1000})
		f.runner.addErr = nil
		dl := f.row(id)
		f.svc.notFoundSince.Store(dl.ID, time.Now().Add(-time.Minute))
		f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: "808", Outcome: "admitted"}
		f.svc.confirmAdmissions(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{})
		if got := f.row(id); got.Handle != "808" || got.State != "downloading" {
			t.Fatalf("row = %s handle %q, want admitted by its receipt", got.State, got.Handle)
		}
		if _, still := f.svc.notFoundSince.Load(dl.ID); still {
			t.Fatal("a found receipt kept the not-found clock")
		}
	})
	t.Run("an unreachable receipt store never fails a submission", func(t *testing.T) {
		f := setupV2(t, nil)
		f.runner.addErr = errors.New("connection reset")
		id, _ := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: 1, Title: epOne,
			DownloadURL: "http://dl/" + epOne, Indexer: "idx", Protocol: "torrent", Size: 1000})
		f.runner.addErr = nil
		dl := f.row(id)
		f.svc.notFoundSince.Store(dl.ID, time.Now().Add(-time.Hour))
		f.runner.admitErr = errors.New("timeout")
		f.svc.confirmAdmissions(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{})
		if got := f.row(id); got.RunnerOutcome != "" {
			t.Fatal("an unreachable Runner failed the submission")
		}
		f.runner.admitErr = nil
		f.svc.confirmAdmissions(f.ctx, f.cfg, f.runner, []sqlite.Download{dl}, map[int64]bool{})
		if got := f.row(id); got.RunnerOutcome != "" {
			t.Fatal("the not-found clock did not restart after the outage")
		}
	})
}

// The deadline sweep: a stale hold is reconciled by its receipt; an overdue
// publication is import_failed{stalled}, and its completed row with it.
func TestDeadlineSweepMatrix(t *testing.T) {
	t.Run("a stale hold is marked once and reconciled", func(t *testing.T) {
		f := setupV2(t, nil)
		f.deferGrabs()
		dl := f.grab(1, epOne)
		st := ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued, OperationalState: "held"}
		f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "event 1"})
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET deadline_at=? WHERE id=?`, time.Now().Add(-time.Minute).UnixMilli(), dl.ID); err != nil {
			t.Fatal(err)
		}
		f.svc.sweepDeadlines(f.ctx, f.cfg, f.runner)
		f.svc.sweepDeadlines(f.ctx, f.cfg, f.runner)
		if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='held_stale'`); n != 1 {
			t.Fatalf("held_stale history = %d, want marked once", n)
		}
		blames := true
		f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: dl.Handle, Outcome: "failed",
			FailureReason: ports.ReasonHoldExpired, BlamesRelease: &blames}
		f.svc.sweepDeadlines(f.ctx, f.cfg, f.runner)
		if got := f.row(dl.ID); got.RunnerOutcome != "failed" || got.RunnerOutcomeReason != ports.ReasonHoldExpired {
			t.Fatalf("row = %s/%s, want Runner's hold_expired from the receipt", got.RunnerOutcome, got.RunnerOutcomeReason)
		}
	})
	t.Run("an overdue publication fails its import", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		dir := f.root + "/Test.Show.S01E01.job-101.runner-late"
		writeCompleted(t, dir+"/"+epOne+".mkv", 5) // no manifest: it waits
		pub := ports.RunnerPublication{Path: dir, Generation: "late", Kind: "completed"}
		f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
		p, _, _ := f.db.PublicationByPath(f.ctx, dir)
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET deadline_at=?, error='no manifest' WHERE id=?`, time.Now().Add(-time.Minute).UnixMilli(), p.ID); err != nil {
			t.Fatal(err)
		}
		f.svc.sweepDeadlines(f.ctx, f.cfg, f.runner)
		p, _ = f.db.GetPublication(f.ctx, p.ID)
		got := f.row(dl.ID)
		if p.State != "import_failed" || !strings.Contains(p.Error, "stalled") || !strings.Contains(p.Error, "(no manifest)") {
			t.Fatalf("publication = %s %q, want import_failed{stalled} with its last error", p.State, p.Error)
		}
		if got.State != "failed" || !strings.Contains(got.Error, "import_failed: stalled") {
			t.Fatalf("row = %s %q, want failed naming the stalled publication", got.State, got.Error)
		}
	})
	t.Run("another client's overdue publication is not this sweep's", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "x", Kind: "completed", Path: f.root + "/x", Cohort: "live", Source: "none"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET deadline_at=?`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		other := f.cfg
		other.ID = 42
		f.svc.sweepDeadlines(f.ctx, other, f.runner)
		p, _, _ := f.db.PublicationByPath(f.ctx, f.root+"/x")
		if p.State != "discovered" {
			t.Fatalf("publication = %s, want untouched by another client's sweep", p.State)
		}
	})
}

// The event stream: progress is coalesced; a fact for a job id another row
// carries under a different token is a mismatch; a handle-only fact finds
// its legacy row.
func TestEventStreamMatrix(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pending := map[int64]float64{}
	flushed := time.Now()
	progress := func(p float64) {
		st := ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateDownloading, Progress: p}
		f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Kind: ports.EventProgress,
			Fact: &ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "event"}}, pending, &flushed)
	}
	progress(0.3)
	if pending[dl.ID] != 0.3 || f.row(dl.ID).Progress != 0 {
		t.Fatalf("pending = %v progress %.2f, want coalesced and not written", pending, f.row(dl.ID).Progress)
	}
	flushed = time.Now().Add(-time.Minute)
	progress(0.5)
	if len(pending) != 0 || f.row(dl.ID).Progress != 0.5 {
		t.Fatalf("pending = %v progress %.2f, want flushed", pending, f.row(dl.ID).Progress)
	}
	// A decision is applied at once and drops the row's pending progress.
	pending[dl.ID] = 0.7
	st := ports.DownloadStatus{Transfer: dl.Transfer, State: ports.StateQueued, OperationalState: "held"}
	f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Kind: ports.EventStage,
		Fact: &ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "event"}}, pending, &flushed)
	if _, ok := pending[dl.ID]; ok || f.row(dl.ID).State != "held" {
		t.Fatal("a hold was coalesced like progress")
	}
	// The same job id under another token: dropped and counted against the row.
	before := f.svc.FactMismatches()
	tm := failed(ports.ReasonHealth, true)
	f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Fact: &ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-reused",
		Handle: ports.Handle(dl.Handle), Terminal: &tm, Source: "event"}}, pending, &flushed)
	if f.svc.FactMismatches() != before+1 || f.row(dl.ID).RunnerOutcome != "" {
		t.Fatal("a reused job id's terminal was not dropped and counted")
	}
	// A legacy row with no token is found by its handle.
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET transfer='' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.deferGrabs()
	f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Fact: &ports.RunnerFact{Kind: ports.FactTerminal,
		Handle: ports.Handle(dl.Handle), Terminal: &tm, Source: "event"}}, pending, &flushed)
	if got := f.row(dl.ID); got.RunnerOutcome != "failed" {
		t.Fatalf("handle-only terminal on a legacy row = %q, want applied", got.RunnerOutcome)
	}
}

// The poll's queue read: an import in flight is seen, a foreign token on this
// row's job id is counted, and a failed poll resets absence.
func TestQueuePollMatrix(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	f.runner.statuses = []ports.DownloadStatus{{Handle: ports.Handle(dl.Handle), Transfer: "t-after-wipe", State: ports.StateDownloading}}
	before := f.svc.FactMismatches()
	f.svc.reconcileRunnerClient(f.ctx, f.cfg, f.runner, f.runner, []sqlite.Download{dl})
	if f.svc.FactMismatches() != before+1 {
		t.Fatal("a reused job id in the queue was not counted")
	}
	if _, absent := f.svc.absentSince.Load(dl.ID); !absent {
		t.Fatal("the row the queue does not name did not start its absence clock")
	}
	f.svc.reconcileRunnerClient(f.ctx, f.cfg, failingStatuses{f.runner}, f.runner, []sqlite.Download{dl})
	if _, absent := f.svc.absentSince.Load(dl.ID); absent {
		t.Fatal("a failed poll counted towards absence")
	}
}

type failingStatuses struct{ *fakeRunner }

func (failingStatuses) Statuses(context.Context) ([]ports.DownloadStatus, error) {
	return nil, errors.New("runner unreachable")
}

// Outstanding provisional rows join the poll once, even when the active list
// already has them.
func TestOutstandingRowsJoinThePollOnce(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	if _, err := f.svc.recordTerminal(f.ctx, dl, f.cfg, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonLost}, "poll", 0, true); err != nil {
		t.Fatal(err)
	}
	prov := f.row(dl.ID)
	rows := f.svc.withOutstandingRunnerRows(f.ctx, f.cfg.ID, []sqlite.Download{prov})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the provisional row once", len(rows))
	}
	if rows := f.svc.withOutstandingRunnerRows(f.ctx, f.cfg.ID, nil); len(rows) != 1 || rows[0].ID != dl.ID {
		t.Fatalf("rows = %+v, want the provisional row added", rows)
	}
}

func mustReadManifest(t *testing.T, dir string) ports.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ports.ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m ports.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
