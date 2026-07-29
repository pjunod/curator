package acquisition

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/ports"
)

// Deleting a job in the download client must not ban the release.
//
// A client-reported failure normally means a bad release, and blocklisting it
// is how Monarr avoids re-grabbing the same broken copy all night. But an
// operator clicking delete arrives on the same channel, and the nzbd adapter's
// own comment claimed the mapping avoided a blocklist while the code
// blocklisted anyway — so removing a job in nzbd's UI permanently banned a
// perfectly good release, silently, with no way to tell it had happened.
func TestAnOperatorDeletingTheJobDoesNotBlocklistTheRelease(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()

	id := grabEpisode(t, svc)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateFailed, Progress: 0.4,
		Message: "removed in nzbd", Blameless: true,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "failed" {
		t.Fatalf("state = %q, want failed — the download did stop", dl.State)
	}
	blocked, err := db.IsBlocklisted(ctx, epRelease, "idx")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("the release was blocklisted because someone clicked delete in " +
			"the download client — that burns a good copy and sends the " +
			"re-search after a worse one")
	}
}

// Deleting a job must not make Monarr go and grab another copy.
//
// This is the half the blameless flag never covered. Blameless guards the
// blocklist and nothing else, so a deletion still went down the failure path
// and the failure path re-searches: delete a job in nzbd, and seconds later
// Monarr has grabbed a different release for the same want. Delete that one
// too and it grabs a third. From the outside it looks like two applications
// fighting over a decision the operator already made.
func TestDeletingAJobDoesNotGrabAReplacement(t *testing.T) {
	client := &fakeClient{}
	// A release is available, so a re-search would find something. If nothing
	// were grabbable, this test would pass without proving anything.
	svc, db, _ := setup(t, []ports.Release{{
		Title: epRelease, DownloadURL: "u-replacement", Protocol: "torrent",
		Indexer: "idx", Seeders: 99,
	}}, client)
	ctx := context.Background()

	id := grabEpisode(t, svc)
	grabsBefore := len(client.added)

	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateRemoved, Progress: 0.4,
		Message: "removed in nzbd", Blameless: true,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	if n := len(client.added) - grabsBefore; n != 0 {
		t.Errorf("Monarr grabbed %d replacement(s) after the operator deleted the job; "+
			"a deletion is an instruction, not a failure to recover from", n)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "failed" {
		t.Errorf("state = %q, want failed — the download did stop", dl.State)
	}
	blocked, err := db.IsBlocklisted(ctx, epRelease, "idx")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("a deleted job blocklisted the release")
	}
}

// A removal must also clear the release from the in-flight view immediately.
//
// The view is fed by observations, so nothing ends a stage except an
// observation that supersedes it. Deleting a job in nzbd stops the
// observations, which meant the last thing Monarr ever heard about it was
// "downloading" — and it sat there claiming to be in progress, with a
// progress bar, indefinitely.
func TestARemovedDownloadLeavesTheInFlightView(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := setup(t, nil, client)
	ctx := context.Background()
	reg := transfers.New()
	svc.SetRegistry(reg)

	id := grabEpisode(t, svc)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateDownloading, Progress: 0.4,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := reg.Count(transfers.StageDownloading); n != 1 {
		t.Fatalf("in flight while downloading = %d, want 1", n)
	}

	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateRemoved,
		Message: "removed in nzbd", Blameless: true,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := reg.Count(""); n != 0 {
		t.Errorf("in flight after the job was deleted = %d, want 0 — download %d "+
			"is still showing a progress bar for work nobody is doing", n, id)
	}
}

// And the same when the client simply stops mentioning it.
//
// Push is the fast path, but it is explicitly never load-bearing: a client
// that cannot stream, or a stream that dropped the one event that mattered,
// must not leave the view wrong forever. The sweep reconciles what it is
// showing against what the client just said.
func TestAVanishedDownloadLeavesTheInFlightView(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := setup(t, nil, client)
	ctx := context.Background()
	reg := transfers.New()
	svc.SetRegistry(reg)

	grabEpisode(t, svc)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateDownloading, Progress: 0.4,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := reg.Count(transfers.StageDownloading); n != 1 {
		t.Fatalf("in flight while downloading = %d, want 1", n)
	}

	// The client answers, and no longer knows about it. Not an error, not a
	// timeout — simply gone.
	client.statuses = nil
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := reg.Count(""); n != 0 {
		t.Errorf("in flight after the client stopped reporting it = %d, want 0", n)
	}
}

// The ordinary case still bans it. A blameless failure is the exception, and
// an exception that swallowed the rule would be worse than the bug it fixed:
// a genuinely broken release would be re-grabbed forever.
func TestAGenuineDownloadFailureStillBlocklists(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()

	id := grabEpisode(t, svc)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: epRelease, State: ports.StateFailed, Progress: 0.9,
		Message: "PAR_FAILURE",
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := db.GetDownload(ctx, id); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.IsBlocklisted(ctx, epRelease, "idx")
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Error("a release that failed par repair must be blocklisted, or the " +
			"next backlog pass grabs the same broken copy")
	}
}
