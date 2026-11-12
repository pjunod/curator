package acquisition

import (
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// overdue puts a grabbed row in a non-terminal state past its deadline.
func (f *v2Fixture) overdue(dl sqlite.Download, state string) sqlite.Download {
	f.t.Helper()
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state=?, deadline_at=?, submission_phase='submitted' WHERE id=?`,
		state, time.Now().Add(-91*time.Hour).UnixMilli(), dl.ID); err != nil {
		f.t.Fatal(err)
	}
	return f.row(dl.ID)
}

func (f *v2Fixture) regrabFor(id int64) int {
	return f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, id)
}

// Field data 2026-11-12: after Runner moved hosts its receipts still said
// "paused" or "fetching" for jobs no longer in its queue, and the overdue
// rows sat 91 h. A stale receipt (or none) with the job gone from the queue
// fails the row blamelessly and grabs the want again; an unreachable Runner
// decides nothing.
func TestOverdueRowWithAStaleReceiptFailsAndRegrabs(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.overdue(f.grab(1, epOne), "downloading")
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: dl.Handle, Outcome: "paused"}
	f.runner.admitErr = errFake
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, map[string]ports.DownloadStatus{}, map[string]ports.DownloadStatus{})
	if got := f.row(dl.ID); got.State != "downloading" {
		t.Fatalf("unreachable Runner: row = %s, want unchanged", got.State)
	}
	f.runner.admitErr = nil
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, map[string]ports.DownloadStatus{}, map[string]ports.DownloadStatus{})
	got := f.row(dl.ID)
	if got.State != "failed" || got.RunnerBlamesRelease || !got.DeadlineAt.IsZero() || f.regrabFor(dl.ID) != 1 {
		t.Fatalf("stale receipt: row = %s blames=%v deadline=%v regrab=%d (%s)", got.State, got.RunnerBlamesRelease, got.DeadlineAt, f.regrabFor(dl.ID), got.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM blocklist`); n != 0 {
		t.Fatalf("blocklist = %d, want none (blameless)", n)
	}

	unknown := f.overdue(f.grab(2, epTwo), "downloading")
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, map[string]ports.DownloadStatus{}, map[string]ports.DownloadStatus{})
	if got := f.row(unknown.ID); got.State != "failed" || f.regrabFor(unknown.ID) != 1 {
		t.Fatalf("unknown to Runner: row = %s regrab=%d", got.State, f.regrabFor(unknown.ID))
	}
}

// A terminal Runner's receipt carries is applied; a terminal Curator
// recorded on a row that never left its state ends it.
func TestOverdueRowTakesRunnersTerminal(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.overdue(f.grab(1, epOne), "downloading")
	blames := false
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: dl.Handle, Outcome: "failed",
		FailureReason: ports.ReasonPar, BlamesRelease: &blames, Salvage: "none"}
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, map[string]ports.DownloadStatus{}, map[string]ports.DownloadStatus{})
	if got := f.row(dl.ID); got.State != "failed" || got.RunnerOutcomeReason != ports.ReasonPar {
		t.Fatalf("row = %s/%s, want Runner's par failure", got.State, got.RunnerOutcomeReason)
	}

	lagging := f.overdue(f.grab(2, epTwo), "downloaded")
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET runner_outcome='failed', runner_outcome_reason='par' WHERE id=?`, lagging.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, map[string]ports.DownloadStatus{}, map[string]ports.DownloadStatus{})
	if got := f.row(lagging.ID); got.State != "failed" || got.RunnerOutcomeReason != "par" {
		t.Fatalf("lagging row = %s/%s, want failed keeping Runner's reason", got.State, got.RunnerOutcomeReason)
	}
}

// A job Runner still lists extends its deadline while it moves; one that
// has not moved for the stall limit, or that Runner holds past Runner's own
// deadline, fails.
func TestOverdueRowStillInRunnersQueue(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.overdue(f.grab(1, epOne), "downloading")
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: dl.Handle, Outcome: "fetching"}
	bytes := int64(100)
	queue := map[string]ports.DownloadStatus{dl.Transfer: {Transfer: dl.Transfer, Handle: ports.Handle(dl.Handle), State: ports.StateDownloading, BytesCompleted: &bytes}}
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, queue, map[string]ports.DownloadStatus{})
	got := f.row(dl.ID)
	if got.State != "downloading" || !got.DeadlineAt.After(time.Now()) {
		t.Fatalf("progressing: row = %s deadline %v, want the deadline extended", got.State, got.DeadlineAt)
	}
	// Overdue again with nothing moved for longer than the stall limit.
	dl = f.overdue(got, "downloading")
	f.svc.overdueMarks.Store(dl.ID, overdueMark{bytes: bytes, state: ports.StateDownloading, since: time.Now().Add(-overdueStallLimit - time.Minute)})
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, queue, map[string]ports.DownloadStatus{})
	if got := f.row(dl.ID); got.State != "failed" || f.regrabFor(dl.ID) != 1 {
		t.Fatalf("stalled: row = %s regrab=%d", got.State, f.regrabFor(dl.ID))
	}

	blocked := f.overdue(f.grab(2, epTwo), "downloading")
	f.runner.admissions[blocked.Transfer] = ports.RequestAdmission{RequestToken: blocked.Transfer, JobID: blocked.Handle, Outcome: "post_processing"}
	past := time.Now().Add(-time.Hour).Unix()
	q2 := map[string]ports.DownloadStatus{blocked.Transfer: {Transfer: blocked.Transfer, State: ports.StateDownloading, BytesCompleted: &bytes,
		Control: &ports.DownloadControl{Version: 1, Lifecycle: "running", Cause: "storage_unavailable", DeadlineUnix: &past}}}
	f.svc.exitOverdue(f.ctx, f.cfg, f.runner, q2, map[string]ports.DownloadStatus{})
	if got := f.row(blocked.ID); got.State != "failed" {
		t.Fatalf("blocked past Runner's deadline: row = %s", got.State)
	}
}

// A row whose client is gone or disabled is ended by the hourly pass.
func TestOverdueRowOfADisabledClientEnds(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.overdue(f.grab(1, epOne), "downloading")
	if n, err := f.svc.exitOrphanedOverdue(f.ctx); err != nil || n != 0 {
		t.Fatalf("enabled client: ended %d (%v), want its poll to decide", n, err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE download_clients SET enabled=0 WHERE id=?`, dl.ClientID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.exitOrphanedOverdue(f.ctx); err != nil || n != 1 {
		t.Fatalf("disabled client: ended %d (%v)", n, err)
	}
	if got := f.row(dl.ID); got.State != "failed" || f.regrabFor(dl.ID) != 1 {
		t.Fatalf("row = %s regrab=%d", got.State, f.regrabFor(dl.ID))
	}
}

// Field data 2026-11-12 (publications 3, 5, 7): an attempt epoch sealed by
// an earlier, never-completed attempt refused every retry with "import
// selection changed; explicit retry required". The import now opens a fresh
// epoch and the next offer imports.
func TestSealedPlanOfAnEarlierAttemptDoesNotWedgeTheImport(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	pub := f.publish(epOne+".job-9.runner-p1", "p1", map[string]string{epOne + ".mkv": "video"})
	a, err := f.db.EnsureImportAttempt(f.ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SealImportAttempt(f.ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: "/elsewhere/old.mkv", Kind: "media"}}, false); err != nil {
		t.Fatal(err)
	}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	if p.State != "discovered" {
		t.Fatalf("after the refused plan: publication = %s (%s), want returned for the next attempt", p.State, p.Error)
	}
	next, err := f.db.GetImportAttempt(f.ctx, dl.ID, dl.GenerationKey)
	if err != nil || next.Epoch <= a.Epoch || next.PlanSealed {
		t.Fatalf("attempt = %+v (%v), want a fresh epoch", next, err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET retry_at=0 WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.offerWaitingPublications(f.ctx); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = f.db.PublicationByPath(f.ctx, pub.Path); p.State != "imported" && p.State != "removed" {
		t.Fatalf("next offer: publication = %s (%s), want imported", p.State, p.Error)
	}
}
