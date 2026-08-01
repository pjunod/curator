package acquisition

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Activity was a table that only grew: every grab monarr had ever made, and
// every event it had ever recorded, kept forever and rendered a hundred at a
// time. These are the three things that make it finite — a window, a page,
// and a button.

func insertRow(t *testing.T, db *sqlite.DB, itemID, clientID int64, title, state string, age time.Duration) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"episode:1:1:1"},
		ReleaseTitle: title, Protocol: "usenet", ClientID: clientID, State: "grabbed",
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	row.State = state
	if err := db.UpdateDownloadHandoff(ctx, row); err != nil {
		t.Fatal(err)
	}
	// Backdate through the write handle. Every wrapper stamps updated_at
	// with now(), which is right for the daemon and useless for a test that
	// needs a row to be sixty days old.
	if age > 0 {
		if _, err := db.W.ExecContext(ctx,
			"UPDATE downloads SET updated_at = ? WHERE id = ?",
			time.Now().Add(-age).UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Retention ages out terminal rows and old events — and only those.
func TestRetentionAgesOutTerminalRowsOnly(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cid := clients[0].ID

	old := 60 * 24 * time.Hour
	insertRow(t, db, itemID, cid, "Old.Imported", "imported", old)
	insertRow(t, db, itemID, cid, "Old.Failed", "failed", old)
	recent := insertRow(t, db, itemID, cid, "Recent.Imported", "imported", time.Hour)
	// Still moving, and ancient. A row stuck for two months is a bug to
	// look at, not litter to hide.
	stuck := insertRow(t, db, itemID, cid, "Ancient.Grabbed", "grabbed", old)

	if err := db.AddHistory(ctx, "grabbed", itemID, "Old.Imported", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, "UPDATE history_events SET ts = ?",
		time.Now().Add(-old).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetRetentionDays(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if err := svc.PruneActivity(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if n, err := db.HistoryCount(ctx); err != nil || n != 0 {
		t.Errorf("history events left after the sweep: %d (err %v)", n, err)
	}
	counts, err := db.QueueCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["imported"] != 1 || counts["failed"] != 0 {
		t.Errorf("after the sweep: %v — want one recent import and no old rows", counts)
	}
	for _, id := range []int64{recent, stuck} {
		if _, err := db.GetDownload(ctx, id); err != nil {
			t.Errorf("row %d was swept and should not have been: %v", id, err)
		}
	}
}

// 0 is an answer, not an unset field.
func TestRetentionZeroKeepsEverything(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)
	insertRow(t, db, itemID, clients[0].ID, "Ancient", "imported", 3650*24*time.Hour)

	if err := svc.SetRetentionDays(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := svc.RetentionDays(ctx); got != 0 {
		t.Fatalf("retention days %d, want 0", got)
	}
	if err := svc.PruneActivity(ctx); err != nil {
		t.Fatal(err)
	}
	counts, _ := db.QueueCounts(ctx)
	if counts["imported"] != 1 {
		t.Errorf("a ten-year-old row was deleted with retention off: %v", counts)
	}
	// And an unset setting is the default, not zero — "keep everything"
	// has to be chosen, never fallen into.
	fresh, _, _ := setupUsenet(t, &fakeClient{})
	if got := fresh.RetentionDays(context.Background()); got != DefaultRetentionDays {
		t.Errorf("default retention %d, want %d", got, DefaultRetentionDays)
	}
}

// The page asks for one group at a time, and gets counts without rows.
func TestQueuePagingFilteringAndCounts(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)
	cid := clients[0].ID

	for i := 0; i < 30; i++ {
		insertRow(t, db, itemID, cid, fmt.Sprintf("Done.Show.S01E%02d", i), "imported", time.Hour)
	}
	insertRow(t, db, itemID, cid, "Moving.Show.S02E01", "downloading", 0)
	insertRow(t, db, itemID, cid, "Broken.Show.S03E01", "failed", time.Hour)

	counts, err := svc.QueueCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["imported"] != 30 || counts["downloading"] != 1 || counts["failed"] != 1 {
		t.Fatalf("counts %v", counts)
	}

	// Active is a group, not a state: everything that is not terminal.
	act, err := svc.QueuePage(ctx, sqlite.QueueActive, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(act) != 1 || act[0].State != "downloading" {
		t.Errorf("active page: %d rows, %+v", len(act), act)
	}

	// Paged, and the pages do not overlap.
	first, err := svc.QueuePage(ctx, sqlite.QueueImported, "", 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.QueuePage(ctx, sqlite.QueueImported, "", 25, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 25 || len(second) != 5 {
		t.Fatalf("pages: %d then %d, want 25 then 5", len(first), len(second))
	}
	seen := map[int64]bool{}
	for _, d := range append(first, second...) {
		if seen[d.ID] {
			t.Fatalf("row %d appeared on both pages", d.ID)
		}
		seen[d.ID] = true
	}

	// Search is case-insensitive and crosses the groups it is given.
	hits, err := svc.QueuePage(ctx, sqlite.QueueFailed, "broken", 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("search hit %d rows, want 1", len(hits))
	}
}

// "Clear finished" removes receipts, and nothing else.
func TestClearFinishedRemovesOnlyImportedRows(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)
	cid := clients[0].ID

	insertRow(t, db, itemID, cid, "Done.One", "imported", 0)
	insertRow(t, db, itemID, cid, "Done.Two", "imported", 0)
	keepFailed := insertRow(t, db, itemID, cid, "Broken", "failed", 0)
	keepActive := insertRow(t, db, itemID, cid, "Moving", "downloading", 0)

	n, err := svc.ClearFinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("cleared %d rows, want 2", n)
	}
	for _, id := range []int64{keepFailed, keepActive} {
		if _, err := db.GetDownload(ctx, id); err != nil {
			t.Errorf("clearing finished took row %d with it: %v", id, err)
		}
	}
	counts, _ := db.QueueCounts(ctx)
	if counts["imported"] != 0 {
		t.Errorf("finished rows survived: %v", counts)
	}
}

// "Clear failed" dismisses failures and leaves every other state alone.
func TestClearFailedRemovesOnlyFailedRows(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)
	cid := clients[0].ID

	insertRow(t, db, itemID, cid, "Broken.One", "failed", 0)
	insertRow(t, db, itemID, cid, "Broken.Two", "failed", 0)
	keepFinished := insertRow(t, db, itemID, cid, "Done", "imported", 0)
	keepActive := insertRow(t, db, itemID, cid, "Moving", "downloading", 0)

	n, err := svc.ClearFailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("cleared %d rows, want 2", n)
	}
	for _, id := range []int64{keepFinished, keepActive} {
		if _, err := db.GetDownload(ctx, id); err != nil {
			t.Errorf("clearing failed took row %d with it: %v", id, err)
		}
	}
	counts, _ := db.QueueCounts(ctx)
	if counts["failed"] != 0 {
		t.Errorf("failed rows survived: %v", counts)
	}
}

// The timeline pages too, and a per-item view is a filter on the same data.
func TestHistoryPaging(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if err := db.AddHistory(ctx, "grabbed", itemID, fmt.Sprintf("Rel.%02d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddHistory(ctx, "grabbed", itemID+999, "Someone.Else", nil); err != nil {
		t.Fatal(err)
	}

	page, err := svc.HistoryPage(ctx, 0, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 5 {
		t.Fatalf("history page: %d rows, want 5", len(page))
	}
	mine, err := svc.HistoryPage(ctx, itemID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 12 {
		t.Errorf("item timeline: %d events, want 12 (the other item's must not be here)", len(mine))
	}
}
