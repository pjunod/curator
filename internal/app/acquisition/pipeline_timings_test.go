package acquisition

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The failure -> replacement-grab seam is timed on the existing failure path:
// the clock starts where a replacement is attempted and stops when one has
// been submitted.
func TestFailureToNextGrabIsTimedOnTheOldPath(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.WEB-DL.x264-AAA", 50),
		rel("Test.Movie.2024.1080p.WEB-DL.x264-BBB", 10),
	}, client)
	pipe := pipeline.New(db)
	svc.SetTimings(pipe)
	ctx := context.Background()

	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{Handle: "h1", State: ports.StateFailed, Message: "stalled"}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 2 {
		t.Fatalf("adds = %v, want the replacement grabbed", client.added)
	}
	s, err := pipe.Stats(ctx, pipeline.FailureToNextGrab, pipeline.Window)
	if err != nil {
		t.Fatal(err)
	}
	if s.Samples != 1 || s.Waiting != 0 {
		t.Fatalf("failure_to_next_grab = %+v, want one finished timing and none waiting", s)
	}
}

// A status that names this row's handle under another transfer token is
// counted and never applied.
func TestFactMismatchCounterCountsForeignTransferTokens(t *testing.T) {
	svc, _, _ := autoSetup(t, nil, &fakeClient{})
	dl := sqliteDownload("h1", "t-1-aaaa")
	svc.noteFactMismatch(dl, []ports.DownloadStatus{{Handle: "h1", Transfer: "t-1-aaaa"}})
	if svc.FactMismatches() != 0 {
		t.Fatal("a matching token is not a mismatch")
	}
	foreign := []ports.DownloadStatus{{Handle: "h1", Transfer: "t-2-bbbb"}}
	svc.noteFactMismatch(dl, foreign)
	svc.noteFactMismatch(dl, foreign) // the next poll of the same stuck row
	if svc.FactMismatches() != 1 {
		t.Fatalf("mismatches = %d, want 1 (one per download and token)", svc.FactMismatches())
	}
	svc.noteFactMismatch(dl, []ports.DownloadStatus{{Handle: "h1", Transfer: "t-3-cccc"}})
	if svc.FactMismatches() != 2 {
		t.Fatalf("mismatches = %d, want a new token counted", svc.FactMismatches())
	}
}

func sqliteDownload(handle, transfer string) sqlite.Download {
	return sqlite.Download{Handle: handle, Transfer: transfer}
}

// runner_done_to_import starts when the client is first seen finished and ends
// when the row is imported; import_to_cinema_visible is not started at all
// until M5 can end it.
func TestRunnerDoneToImportIsTimedOnTheOldPath(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	pipe := pipeline.New(db)
	svc.SetTimings(pipe)
	ctx := context.Background()

	id := grabEpisode(t, svc)
	client.statuses = completed(payloadDir(t))
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if dl, _ := db.GetDownload(ctx, id); dl.State != "imported" {
		t.Fatalf("state = %s (%s), want imported", dl.State, dl.Error)
	}
	done, _ := pipe.Stats(ctx, pipeline.RunnerDoneToImport, pipeline.Window)
	if done.Samples != 1 || done.Waiting != 0 {
		t.Errorf("runner_done_to_import = %+v, want one finished timing", done)
	}
	cinema, _ := pipe.Stats(ctx, pipeline.ImportToCinemaVisible, pipeline.Window)
	if cinema.Samples != 0 || cinema.Waiting != 0 {
		t.Errorf("import_to_cinema_visible = %+v, want untouched: no Cinema notifier, no sub-delivery to end it", cinema)
	}
}

// A row held for approval starts runner_done_to_import at approval, not when
// the client finished: an operator's wait is not the pipeline's latency.
func TestManualApprovalWaitIsNotInRunnerDoneToImport(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	pipe := pipeline.New(db)
	svc.SetTimings(pipe)
	ctx := context.Background()
	cfg, _ := db.GetDownloadClient(ctx, 1)
	cfg.ManualApproval = true
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	id := grabEpisode(t, svc)
	client.statuses = completed(payloadDir(t))
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if dl, _ := db.GetDownload(ctx, id); dl.State != "awaiting_import" {
		t.Fatalf("state = %s, want awaiting_import", dl.State)
	}
	if s, _ := pipe.Stats(ctx, pipeline.RunnerDoneToImport, pipeline.Window); s.Samples != 0 || s.Waiting != 0 {
		t.Fatalf("while held: %+v, want no timing", s)
	}
	if err := svc.ImportNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if s, _ := pipe.Stats(ctx, pipeline.RunnerDoneToImport, pipeline.Window); s.Samples != 1 || s.Waiting != 0 {
		t.Fatalf("after approval: %+v, want one finished timing", s)
	}
}

// A failure with nothing to search for must not leave an open timing.
func TestFailureWithNoWantablesStartsNoTiming(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-AAA", 50)}, client)
	pipe := pipeline.New(db)
	svc.SetTimings(pipe)
	ctx := context.Background()
	id, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: movieID, State: "downloading", Protocol: "usenet", ReleaseTitle: "Test.Movie.2024.1080p.WEB-DL.x264-AAA"})
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	svc.handleFailure(ctx, dl, 0, "stalled", true)
	if s, _ := pipe.Stats(ctx, pipeline.FailureToNextGrab, pipeline.Window); s.Samples != 0 || s.Waiting != 0 {
		t.Fatalf("failure_to_next_grab = %+v, want nothing recorded", s)
	}
	if len(client.added) != 0 {
		t.Fatalf("adds = %v, want none", client.added)
	}
}

// A series failure ends its timing only when a season job was actually queued,
// and never leaves it open either way.
func TestSeriesFailureTimingEndsOrIsDropped(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	pipe := pipeline.New(db)
	svc.SetTimings(pipe)
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, _ := db.GetDownload(ctx, id)
	svc.handleFailure(ctx, dl, 0, "stalled", false)
	s, _ := pipe.Stats(ctx, pipeline.FailureToNextGrab, pipeline.Window)
	if s.Waiting != 0 {
		t.Fatalf("failure_to_next_grab = %+v, want no open timing", s)
	}
	var queued int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind).Scan(&queued)
	if want := queued; s.Samples != want {
		t.Fatalf("samples = %d with %d season job(s) queued; a timing may end only when a job was queued", s.Samples, queued)
	}
}
