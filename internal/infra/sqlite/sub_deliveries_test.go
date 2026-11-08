package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Migration 0074: a sub-delivery is one row per (notifier, key). Writing the
// same key again — the same directory of the same publication imported again
// — refreshes the row and advances its attempt; it never adds a second one.
func TestSubDeliveryRewriteAdvancesTheAttempt(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	row := OutboxWrite{NotifierID: 1, DownloadID: 7, PublicationID: 3, Directory: "/tv/Show/Season 01",
		IdempotencyKey: "t-7-x/gen/0123456789abcdef", Event: "import", Payload: `{}`,
		ExpectedFiles: []ports.ExpectedFile{{Path: "/tv/Show/Season 01/a.mkv", Bytes: 1, MtimeNs: 1794076138123456789}}}
	if err := d.EnqueueDeliveries(ctx, []OutboxWrite{row}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.ListDeliveries(ctx, 1, 10)
	if len(got) != 1 || got[0].Attempt != 1 || got[0].ExpectedFiles[0].MtimeNs != 1794076138123456789 || got[0].RequestKey() != "t-7-x/gen/0123456789abcdef/1" {
		t.Fatalf("first write = %+v", got)
	}
	got[0].Status, got[0].VisibleAt, got[0].ItemIDs = "ok", time.Now().UnixMilli(), []int64{1201}
	if err := d.SettleDelivery(ctx, got[0], DeliveryTiming{}); err != nil {
		t.Fatal(err)
	}
	row.ExpectedFiles[0].Bytes = 2
	if err := d.EnqueueDeliveries(ctx, []OutboxWrite{row}); err != nil {
		t.Fatal(err)
	}
	got, _ = d.ListDeliveries(ctx, 1, 10)
	if len(got) != 1 || got[0].Attempt != 2 || got[0].Status != "pending" || got[0].VisibleAt != 0 || len(got[0].ItemIDs) != 0 || got[0].ExpectedFiles[0].Bytes != 2 {
		t.Fatalf("rewrite = %+v", got)
	}
	// A row without a key (a chat notifier's) is never merged.
	chat := OutboxWrite{NotifierID: 1, Event: "import", Payload: `{}`}
	if err := d.EnqueueDeliveries(ctx, []OutboxWrite{chat, chat}); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.ListDeliveries(ctx, 1, 10); len(got) != 3 {
		t.Fatalf("rows = %d, want the sub-delivery and two chat rows", len(got))
	}
}

// import_to_cinema_visible ends when the last pending sub-delivery of the
// import becomes visible, not the first.
func TestVisibleEndsTheTimingOnlyForTheLastDirectory(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	rows := []OutboxWrite{
		{NotifierID: 1, DownloadID: 7, PublicationID: 3, Directory: "/a", IdempotencyKey: "t/g/a", Event: "import", Payload: `{}`},
		{NotifierID: 1, DownloadID: 7, PublicationID: 3, Directory: "/b", IdempotencyKey: "t/g/b", Event: "import", Payload: `{}`},
	}
	if err := d.RecordImported(ctx, HistoryWrite{Type: "imported"}, rows, []TimingStart{{Kind: TimingImportToCinemaVisible, Transfer: "t", DownloadID: 7}}); err != nil {
		t.Fatal(err)
	}
	open := func() int {
		n, _, err := d.PipelineWaiting(ctx, TimingImportToCinemaVisible, 0)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	got, _ := d.ListDeliveries(ctx, 1, 10)
	for i, r := range got {
		r.Status, r.VisibleAt = "ok", time.Now().UnixMilli()
		if err := d.SettleDelivery(ctx, r, DeliveryTiming{Transfer: "t"}); err != nil {
			t.Fatal(err)
		}
		if want := 1 - i; open() != want {
			t.Fatalf("after %d visible: open timings = %d, want %d", i+1, open(), want)
		}
	}
}
