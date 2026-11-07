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
	svc.noteFactMismatch(dl, []ports.DownloadStatus{{Handle: "h1", Transfer: "t-2-bbbb"}})
	if svc.FactMismatches() != 1 {
		t.Fatalf("mismatches = %d, want 1", svc.FactMismatches())
	}
}

func sqliteDownload(handle, transfer string) sqlite.Download {
	return sqlite.Download{Handle: handle, Transfer: transfer}
}

// runner_done_to_import starts when the client is first seen finished and ends
// when the row is imported; import_to_cinema_visible starts at the import and
// stays open until M5.
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
	if cinema.Samples != 0 || cinema.Waiting != 1 {
		t.Errorf("import_to_cinema_visible = %+v, want started and still waiting", cinema)
	}
}
