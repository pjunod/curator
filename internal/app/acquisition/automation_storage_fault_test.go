package acquisition

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// The automatic grab passes write a download row and submit it to the
// client. A storage failure anywhere in a pass, followed by a clean pass,
// must never grab the wantable twice nor leave the client holding a grab
// Curator has no download row for.

func TestAutomaticGrabStorageFaultsConvergeOnOneGrab(t *testing.T) {
	ctx := context.Background()
	passes := map[string]func(svc *Service, itemID int64) error{
		"rss":     func(svc *Service, _ int64) error { return svc.SyncRSS(ctx) },
		"backlog": func(svc *Service, _ int64) error { return svc.BacklogSearch(ctx) },
		"item": func(svc *Service, itemID int64) error {
			_, err := svc.AutoSearchItem(ctx, itemID)
			return err
		},
	}
	for name, pass := range passes {
		for _, write := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/write=%t", name, write), func(t *testing.T) {
				eachStorageFault(t, func(t *testing.T, f *storageFault) {
					client := &fakeClient{}
					svc, db, itemID := autoSetupOn(t, migratedTestDB(t), []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 50)}, client)
					var faulted error
					withStorageFault(t, db, write, f, func() { faulted = pass(svc, itemID) })
					if faulted != nil && !errors.Is(faulted, errStorageFault) && f.fired.Load() {
						t.Logf("failure surfaced as %v", faulted)
					}
					svc.InvalidateWanted()
					if err := pass(svc, itemID); err != nil {
						t.Fatalf("pass after the failure: %v", err)
					}
					var active, all int
					if err := db.R.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state!='failed'), count(*) FROM downloads WHERE media_item_id=?`, itemID).Scan(&active, &all); err != nil {
						t.Fatal(err)
					}
					client.mu.Lock()
					added := len(client.added)
					client.mu.Unlock()
					// RSS is rate-limited per indexer and a submission whose
					// outcome is unknown is retried by its own sweep, so the
					// clean pass need not grab; it must never grab twice, and
					// the client must never hold a grab Curator has no row for.
					if active > 1 || added > 1 || added > all {
						t.Fatalf("after failure (fired=%t, err=%v) and a clean pass: %d active of %d downloads, %d client adds", f.fired.Load(), faulted, active, all, added)
					}
					if !f.fired.Load() && (active != 1 || added != 1) {
						t.Fatalf("unfaulted passes: %d active downloads, %d client adds", active, added)
					}
				})
			})
		}
	}
}

func TestAutomationReadsFailClosedOnStorageFaults(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := autoSetupOn(t, migratedTestDB(t), nil, &fakeClient{})
	readFaultMatrix(t, db, map[string]func() error{
		"wanted-list": func() error { svc.InvalidateWanted(); _, err := svc.WantedList(ctx); return err },
		"lifecycle-review": func() error {
			_, _, err := svc.ListLibraryLifecycleReview(ctx, itemID)
			return err
		},
	})
}
