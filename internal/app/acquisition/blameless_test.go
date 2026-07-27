package acquisition

import (
	"context"
	"testing"

	"github.com/monarr-media/monarr/internal/ports"
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
