package acquisition

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/ports"
)

// The download client's post-processing stage must survive the trip.
//
// nzbd reports `par_repair` on both channels — in the queue as
// {"post":{"stage":"par_repair"}} and on the event stream as job_pp_stage —
// and Monarr threw the name away on both. The poll turned it into the
// sentence "post-processing: par repair" and dropped the machine-readable
// half; the event carried it on a field the reconciler never read. So the
// whole of post-processing collapsed back to "downloading", and a release
// spending twenty minutes rebuilding a damaged archive was indistinguishable
// from one still pulling articles.
func TestThePostProcessingStageSurvivesToTheQueueRow(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := setup(t, nil, client)
	ctx := context.Background()
	svc.SetRegistry(transfers.New())

	id := grabEpisode(t, svc)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateDownloading,
		Progress: 1, Message: "post-processing: par repair", Stage: "par_repair",
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	live, ok := svc.LiveStage(id)
	if !ok {
		t.Fatal("nothing in flight; the job vanished from the live view during post-processing")
	}
	if live.Stage != transfers.StageParRepair {
		t.Errorf("stage = %q, want %q — repairing an archive is not downloading it",
			live.Stage, transfers.StageParRepair)
	}
	// Post-processing has no byte count, and a bar drawn at zero would claim
	// nothing has happened.
	if _, known := live.Fraction(); known {
		t.Error("post-processing reported a progress fraction it cannot know")
	}
}

// Moving on to the next stage replaces the last, rather than stacking.
func TestAdvancingThroughPostProcessingReplacesTheStage(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := setup(t, nil, client)
	ctx := context.Background()
	reg := transfers.New()
	svc.SetRegistry(reg)

	id := grabEpisode(t, svc)
	for _, stage := range []string{"par_verify", "par_repair", "unpack", "move"} {
		client.statuses = []ports.DownloadStatus{{
			Handle: "h1", Name: epRelease, State: ports.StateDownloading,
			Progress: 1, Stage: stage,
		}}
		if err := svc.RefreshQueue(ctx); err != nil {
			t.Fatal(err)
		}
		live, ok := reg.Stage(id)
		if !ok || live.Stage != stage {
			t.Fatalf("stage = %+v, want %q", live, stage)
		}
		if n := reg.Count(""); n != 1 {
			t.Fatalf("in flight = %d at %q, want 1 — the earlier stages are still listed",
				n, stage)
		}
	}
}
