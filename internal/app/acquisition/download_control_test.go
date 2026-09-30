package acquisition

import (
	"context"
	"encoding/json"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
	"testing"
)

func TestResourceHoldPersistsAndSuppressesReplacement(t *testing.T) {
	client := &fakeClient{}
	svc, db, item := setup(t, nil, client)
	ctx := context.Background()
	id := grabEpisode(t, svc)
	cfg, _ := db.GetDownloadClient(ctx, 1)
	dl, _ := db.GetDownload(ctx, id)
	held := &ports.DownloadControl{Version: 1, Revision: "9007199254740993", Lifecycle: "held", Cause: "capacity", Stage: "download_write", RetryPolicy: "resume_same_job", Message: "Waiting for storage capacity", Instance: "fixture"}
	svc.reconcileDownload(ctx, dl, cfg, ports.DownloadStatus{Handle: "h1", State: ports.StateFailed, Progress: 1, Control: held}, "event")
	dl, _ = db.GetDownload(ctx, id)
	if dl.State != "downloading" || dl.RunnerControl == "" {
		t.Fatalf("hold not durable: %+v", dl)
	}
	active, _ := db.ListActiveDownloads(ctx)
	if len(active) != 1 {
		t.Fatalf("active holds: %d", len(active))
	}
	// A restarted service has no in-memory hold map.
	restarted := New(db, nil, nil, nil, svc.newClient)
	for _, state := range []ports.DownloadState{ports.StateFailed, ports.StateCompleted, ports.StateDownloading} {
		restarted.reconcileDownload(ctx, dl, cfg, ports.DownloadStatus{Handle: "h1", State: state, SavePath: "/synthetic/payload"}, "poll")
	}
	duplicate, err := svc.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Title: "Different.Release.S01E01.1080p.WEB-DL", Protocol: "torrent", DownloadURL: "replacement"})
	if err != nil || duplicate != id || len(client.added) != 1 {
		t.Fatalf("replacement after hold: %d %v adds=%v", duplicate, err, client.added)
	}
	dl, _ = db.GetDownload(ctx, id)
	if dl.State != "downloading" {
		t.Fatalf("late terminal erased hold: %s", dl.State)
	}
}

func TestControlRevisionOrderingAndContinuity(t *testing.T) {
	svc, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, _ := db.GetDownload(ctx, id)
	c := ports.DownloadControl{Version: 1, Revision: "42", Lifecycle: "held", Cause: "quota", Stage: "finalize", RetryPolicy: "resume_same_job", Instance: "one"}
	raw, _ := json.Marshal(c)
	dl.RunnerControl = string(raw)
	dl.State = "downloading"
	if err := db.UpdateDownloadControl(ctx, dl); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"41", "42", "invalid"} {
		next := c
		next.Revision = revision
		st := ports.DownloadStatus{State: ports.StateFailed, Control: &next}
		if svc.acceptDownloadControl(ctx, &dl, &st) {
			t.Fatalf("accepted stale revision %s", revision)
		}
	}
	next := c
	next.Revision = "43"
	next.Instance = "restored-store"
	st := ports.DownloadStatus{Control: &next}
	if svc.acceptDownloadControl(ctx, &dl, &st) {
		t.Fatal("accepted identity continuity loss")
	}
	next = c
	next.Revision = "43"
	next.Lifecycle = "running"
	st = ports.DownloadStatus{State: ports.StateDownloading, Control: &next}
	if !svc.acceptDownloadControl(ctx, &dl, &st) {
		t.Fatal("rejected explicit resource resolution")
	}
}

func TestHeldSeasonPackSuppressesEpisodeAndPack(t *testing.T) {
	c := ports.DownloadControl{Version: 1, Lifecycle: "held"}
	raw, _ := json.Marshal(c)
	row := sqlite.Download{MediaItemID: 5, RunnerControl: string(raw), WantableIDs: []string{"season:5:1"}}
	if !heldWantOverlap(row, 5, 0, []string{"episode:5:1:5"}) {
		t.Fatal("held pack failed to suppress episode")
	}
	if heldWantOverlap(row, 5, 2, []string{"episode:5:1:5:c2"}) {
		t.Fatal("suppressed unrelated copy")
	}
	row.WantableIDs = []string{"episode:5:1:5"}
	if !heldWantOverlap(row, 5, 0, []string{"season:5:1"}) {
		t.Fatal("episode hold failed to suppress pack")
	}
}
