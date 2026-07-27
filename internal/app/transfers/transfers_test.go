package transfers

import (
	"testing"
	"time"
)

// A job is at exactly one stage, so entering a new one leaves the last.
//
// The stages are steps in a sequence — a release that is unpacking is no
// longer downloading — and a view that showed both would be describing two
// jobs where there is one. It also means no caller has to remember to end the
// previous stage, which is the kind of thing that gets remembered on the path
// somebody was thinking about and forgotten on the other one. That is exactly
// how a deleted job ended up sitting on the panel forever.
func TestAJobIsAtOneStageAtATime(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 7, Stage: StageDownloading, Total: 100})
	r.Begin(Transfer{DownloadID: 7, Stage: StageParVerify})
	r.Begin(Transfer{DownloadID: 7, Stage: StageUnpack})

	if n := r.Count(""); n != 1 {
		t.Fatalf("in flight = %d, want 1 — the earlier stages were not left", n)
	}
	got, ok := r.Stage(7)
	if !ok || got.Stage != StageUnpack {
		t.Fatalf("stage = %+v, want unpack", got)
	}
}

// A notify still retrying must not hide the import that is moving bytes.
//
// Ordering by pipeline position would pick the notify, because notifying
// comes last — so a job busy copying 60 GB would describe itself as
// "notifying" on the strength of a background retry.
func TestALingeringNotifyDoesNotMaskTheImport(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 7, Stage: StageNotifying})
	time.Sleep(2 * time.Millisecond)
	r.Begin(Transfer{DownloadID: 7, Stage: StageImporting, Total: 100})

	got, ok := r.Stage(7)
	if !ok || got.Stage != StageImporting {
		t.Fatalf("stage = %+v, want importing", got)
	}
	if n := r.Count(""); n != 2 {
		t.Errorf("in flight = %d, want 2 — notifying genuinely overlaps and must survive", n)
	}
}

// Re-observing the same stage must not restart its clock.
//
// The elapsed time answers "how long has this been repairing". Restarting it
// on every poll would answer "how long since the last poll" — a number that is
// always about thirty seconds and never useful.
func TestRepeatedObservationsKeepTheOriginalClock(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 7, Stage: StageParRepair})
	first, _ := r.Stage(7)
	r.Begin(Transfer{DownloadID: 7, Stage: StageParRepair})
	again, _ := r.Stage(7)

	if !again.StartedAt.Equal(first.StartedAt) {
		t.Error("the stage clock restarted on a repeated observation of the same stage")
	}
}

// A job that dies mid-post-processing leaves no stage behind.
func TestEndClientStagesClearsPostProcessingToo(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 7, Stage: StageUnpack})
	r.EndClientStages(7)
	if n := r.Count(""); n != 0 {
		t.Errorf("in flight = %d, want 0 — a job that failed during unpack left an "+
			"'extracting' row claiming to be in progress", n)
	}
}
