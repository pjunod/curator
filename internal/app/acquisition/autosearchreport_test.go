package acquisition

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// The bug this file exists for, in one sentence: a download row that ended in
// 'failed' counted as "in flight" forever, so the wantable it belonged to was
// invisible to every search path — Auto Search, RSS, and the backlog — from
// that moment on, and the UI reported it as a search that had started.
//
// Three separate paths park a row at 'failed': the client reported the
// download broke (handleFailure), the user deleted the job in their client
// (handleRemoved), and a failed import handoff. All three are terminal, and
// nothing ever moves a row out of 'failed' — only the user deleting it from
// the queue removes it. So one failure, months ago, permanently retired the
// want. That is the shape of "I click Auto Search and nothing happens, but
// interactive search finds copies and grabbing one works instantly".

// A download that failed must not stop the next search. The blocklist is what
// keeps us off the same bad release; suppressing the whole wantable kept us
// off every OTHER release too, which is precisely the one thing that should
// have happened next.
func TestAutoSearchRunsAgainAfterADownloadFailed(t *testing.T) {
	client := &fakeClient{}
	best := "Test.Movie.2024.1080p.BluRay.x264-GOOD"
	svc, db, movieID := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.720p.WEB-DL.x264-BAD", 5),
		rel(best, 50),
	}, client)
	ctx := context.Background()

	// First search grabs the best release.
	if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	dls, _ := db.ListRecentDownloads(ctx)
	if len(dls) != 1 {
		t.Fatalf("downloads after the first search = %d, want 1", len(dls))
	}

	// It dies in the client. The row goes terminal and stays there.
	if err := db.UpdateDownloadState(ctx, dls[0].ID, "failed", 0, "boom"); err != nil {
		t.Fatal(err)
	}

	// The want is wanted again, so a second search must actually search.
	out, err := svc.AutoSearchItem(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Targets) != 1 {
		t.Fatalf("targets = %+v, want the movie", out.Targets)
	}
	if s := out.Targets[0].Skipped; s != "" {
		t.Fatalf("target skipped as %q after a FAILED download; the failed row is terminal, "+
			"not in flight — this is the bug", s)
	}
	if out.Grabbed != 1 {
		t.Fatalf("grabbed = %d, want 1 (tally: %+v)", out.Grabbed, out.Targets[0])
	}
	if len(client.added) != 2 {
		t.Fatalf("client adds = %v, want a replacement grab", client.added)
	}
}

// The mirror image: a download that is genuinely still moving must still
// suppress. Fixing the terminal-row bug must not turn every poll into a
// duplicate grab.
func TestAutoSearchStillSkipsAWantableThatIsActuallyDownloading(t *testing.T) {
	client := &fakeClient{}
	svc, _, movieID := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 50),
	}, client)
	ctx := context.Background()

	if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	out, err := svc.AutoSearchItem(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Targets) != 1 || out.Targets[0].Skipped != SkipInFlight {
		t.Fatalf("targets = %+v, want the movie skipped as %q", out.Targets, SkipInFlight)
	}
	if len(client.added) != 1 {
		t.Fatalf("client adds = %v, want no duplicate grab", client.added)
	}
}

// Auto Search must be able to say WHY it grabbed nothing. Every one of these
// used to be the same 202 and the same hopeful banner, which is how the
// in-flight bug survived: "found nothing" and "never looked" were the same
// observation from outside.
func TestAutoSearchReportsWhyItGrabbedNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("no releases came back at all", func(t *testing.T) {
		svc, _, movieID := autoSetup(t, nil, &fakeClient{})
		out, err := svc.AutoSearchItem(ctx, movieID)
		if err != nil {
			t.Fatal(err)
		}
		got := out.Targets[0]
		if got.Seen != 0 || got.Grabbed != "" || got.Skipped != "" {
			t.Fatalf("target = %+v, want a search that ran and saw nothing", got)
		}
	})

	t.Run("releases came back but none were this item", func(t *testing.T) {
		svc, _, movieID := autoSetup(t, []ports.Release{
			rel("Some.Other.Film.2019.1080p.BluRay.x264-NOPE", 40),
		}, &fakeClient{})
		out, err := svc.AutoSearchItem(ctx, movieID)
		if err != nil {
			t.Fatal(err)
		}
		got := out.Targets[0]
		if got.Seen != 1 || got.Matched != 0 {
			t.Fatalf("target = %+v, want seen=1 matched=0", got)
		}
	})

	t.Run("they matched but the profile turned them down", func(t *testing.T) {
		// CAM is below every profile's floor, so it matches and is declined.
		svc, _, movieID := autoSetup(t, []ports.Release{
			rel("Test.Movie.2024.CAM.x264-JUNK", 40),
		}, &fakeClient{})
		out, err := svc.AutoSearchItem(ctx, movieID)
		if err != nil {
			t.Fatal(err)
		}
		got := out.Targets[0]
		if got.Matched != 1 || got.Accepted != 0 || got.Grabbed != "" {
			t.Fatalf("target = %+v, want matched=1 accepted=0", got)
		}
	})
}

// An unmonitored target is reported as skipped rather than silently dropped.
// "Nothing happened because you turned monitoring off" is an answer; the
// absence of an answer is not.
func TestAutoSearchNamesAnUnmonitoredTarget(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 50),
	}, client)
	ctx := context.Background()

	no := false
	if err := db.BulkUpdateItem(ctx, movieID, &no, nil); err != nil {
		t.Fatal(err)
	}

	out, err := svc.AutoSearchItem(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Targets) != 1 || out.Targets[0].Skipped != SkipUnmonitored {
		t.Fatalf("targets = %+v, want one skipped as %q", out.Targets, SkipUnmonitored)
	}
	if out.Targets[0].Label == "" {
		t.Error("skipped target has no label; the person needs to know WHICH target")
	}
	if len(client.added) != 0 {
		t.Errorf("grabbed for an unmonitored item: %v", client.added)
	}
}
