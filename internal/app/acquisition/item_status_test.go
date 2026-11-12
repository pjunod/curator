package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func stageOf(t *testing.T, st ItemStatus, key string) StatusStage {
	t.Helper()
	for _, s := range st.Stages {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no %s stage in %+v", key, st.Stages)
	return StatusStage{}
}

func hasAction(s StatusStage, action string) bool {
	for _, a := range s.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// Every search through the shared path leaves a record the item page reads:
// who ran it, the funnel and the main reason nothing was taken.
func TestSearchRecordIsWrittenByTheSharedSearchPath(t *testing.T) {
	ctx := context.Background()
	svc, db, movieID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.CAM.x264-JUNK", 40)}, &fakeClient{})

	if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AutoSearchItem(WithManualSearch(ctx), movieID); err != nil {
		t.Fatal(err)
	}
	recs, err := db.ItemSearchRecords(ctx, movieID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want two", recs)
	}
	if recs[0].Trigger != "manual" || recs[1].Trigger != "automatic" {
		t.Fatalf("triggers = %q, %q; want manual (newest) then automatic", recs[0].Trigger, recs[1].Trigger)
	}
	r := recs[0]
	if r.Seen != 1 || r.Matched != 1 || r.Accepted != 0 || r.TopRejection == "" || r.WantableID != "movie:"+strconv.FormatInt(movieID, 10) || r.Season != -1 {
		t.Fatalf("record = %+v, want 1 seen, 1 matched, 0 accepted with a rejection reason", r)
	}

	st, err := svc.ItemStatus(ctx, movieID, -1)
	if err != nil {
		t.Fatal(err)
	}
	searched := stageOf(t, st, StageSearched)
	if searched.State != StatusWaiting || !hasAction(searched, ActionSearch) || !strings.Contains(st.Headline.Detail, "1 releases found, 0 accepted") {
		t.Fatalf("searched = %+v headline = %+v, want waiting with Search now", searched, st.Headline)
	}
}

func TestSearchRecordsKeepTheNewestTwentyPerWantable(t *testing.T) {
	ctx := context.Background()
	_, db, movieID := autoSetup(t, nil, &fakeClient{})
	for i := 0; i < 25; i++ {
		if err := db.RecordSearch(ctx, sqlite.SearchRecord{MediaItemID: movieID, WantableID: "movie:1", Season: -1, Trigger: "automatic", Seen: i}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := db.ItemSearchRecords(ctx, movieID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != sqlite.SearchRecordsKept || recs[0].Seen != 24 || recs[len(recs)-1].Seen != 5 {
		t.Fatalf("kept %d records (newest seen %d, oldest %d), want the newest %d", len(recs), recs[0].Seen, recs[len(recs)-1].Seen, sqlite.SearchRecordsKept)
	}
}

// The panel relays Runner's own rate and remaining bytes; the ETA is their
// ratio and nothing else.
func TestItemStatusRelaysRunnersRateAndStage(t *testing.T) {
	ctx := context.Background()
	svc, db, movieID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 50)}, &fakeClient{})
	if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	dls, err := db.ItemDownloads(ctx, movieID)
	if err != nil || len(dls) != 1 {
		t.Fatalf("downloads = %v, %v", dls, err)
	}
	dl := dls[0]
	if ok, err := db.UpdateRunnerStatus(ctx, sqlite.RunnerStatusWrite{DownloadID: dl.ID, Transfer: dl.Transfer, State: "downloading",
		Progress: 0.34, RateBps: 10, RemainingBytes: 660}); err != nil || !ok {
		t.Fatalf("status write = %v, %v", ok, err)
	}
	st, err := svc.ItemStatus(ctx, movieID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active || st.Download == nil || st.Download.ETASeconds != 66 || st.Download.RateBps != 10 || st.Download.Bytes != 340 {
		t.Fatalf("download = %+v active=%v, want rate 10, eta 66 s, 340 of 1000 bytes", st.Download, st.Active)
	}
	if s := stageOf(t, st, StageDownloading); s.State != StatusActive || !strings.Contains(s.Detail, "1 min 06 s left") || !hasAction(s, ActionCancelDownload) {
		t.Fatalf("downloading = %+v", s)
	}
	if s := stageOf(t, st, StageSearched); s.State != StatusDone || !strings.Contains(s.Detail, "automatic") {
		t.Fatalf("searched = %+v, want done naming the auto search that grabbed it", s)
	}
	if !strings.HasPrefix(st.Headline.Title, "Downloading, 1 min 06 s left") {
		t.Fatalf("headline = %+v", st.Headline)
	}

	if _, err := db.UpdateRunnerStatus(ctx, sqlite.RunnerStatusWrite{DownloadID: dl.ID, Transfer: dl.Transfer, State: "downloading",
		Progress: 1, Stage: "unpack"}); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.ItemStatus(ctx, movieID, -1)
	processed := stageOf(t, st, StageProcessed)
	if processed.State != StatusActive || processed.Detail != "unpacking" || processed.At == 0 || stageOf(t, st, StageDownloading).State != StatusDone {
		t.Fatalf("processed = %+v, want unpacking since a time with the download done", processed)
	}
	if st.Download.Stage != "unpack" || st.Download.StageSince == 0 {
		t.Fatalf("download = %+v, want stage and since", st.Download)
	}
}

// Cinema's per-file read-back is stored and decides "In cinema".
func TestItemStatusInCinemaFromPerFileReadBack(t *testing.T) {
	ctx := context.Background()
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	item, _ := db.GetMediaItemFull(ctx, movieID)
	path := filepath.Join(item.Path, "Test Movie (2024).mkv")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertFile(ctx, movieID, 0, path, 1); err != nil {
		t.Fatal(err)
	}

	// No record: last known facts, and the way to ask.
	st, err := svc.ItemStatus(ctx, movieID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if s := stageOf(t, st, StageMoved); s.State != StatusDone {
		t.Fatalf("moved = %+v, want done from the file on disk", s)
	}
	if s := stageOf(t, st, StageInCinema); s.State != StatusTodo || !hasAction(s, ActionConfirmCinema) {
		t.Fatalf("in cinema = %+v, want not confirmed with Ask cinema to confirm", s)
	}

	// Ask cinema to confirm needs a cinema notifier.
	if _, err := svc.ConfirmInCinema(ctx, movieID); !errors.Is(err, ErrNothingToConfirm) {
		t.Fatalf("confirm without a notifier = %v, want ErrNothingToConfirm", err)
	}
	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Enabled: true, OnImport: true, Settings: map[string]string{"url": "http://cinema"}}); err != nil {
		t.Fatal(err)
	}
	n, err := svc.ConfirmInCinema(ctx, movieID)
	if err != nil || n != 1 {
		t.Fatalf("confirm = %d, %v; want one folder request", n, err)
	}
	dels, err := db.ListItemDeliveries(ctx, movieID, 10)
	if err != nil || len(dels) != 1 || len(dels[0].ExpectedFiles) != 1 || dels[0].ExpectedFiles[0].Path != path {
		t.Fatalf("deliveries = %+v, %v; want one sub-delivery naming the file", dels, err)
	}
	st, _ = svc.ItemStatus(ctx, movieID, -1)
	if s := stageOf(t, st, StageCinemaNotified); s.State != StatusActive {
		t.Fatalf("cinema notified = %+v, want sending", s)
	}

	del := dels[0]
	del.Status, del.VisibleAt, del.RemoteRequestID, del.ItemIDs = "ok", time.Now().UnixMilli(), "r1", []int64{81244}
	del.FileResults = []sqlite.DeliveryFileResult{{Path: path, State: "placed", ItemID: 81244, Bytes: 1, Matched: true}}
	if err := db.SettleDelivery(ctx, del, sqlite.DeliveryTiming{}); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.ItemStatus(ctx, movieID, -1)
	if s := stageOf(t, st, StageInCinema); s.State != StatusDone || !strings.Contains(s.Detail, "81244") {
		t.Fatalf("in cinema = %+v, want confirmed naming the cinema item", s)
	}
	if st.Cinema == nil || st.Cinema.DeliveryID != del.ID || !strings.HasPrefix(st.Headline.Title, "In cinema, confirmed") {
		t.Fatalf("cinema = %+v headline = %+v", st.Cinema, st.Headline)
	}
	if !st.Done {
		t.Fatalf("status not done: %+v", st.Stages)
	}
	reread, _ := db.ListItemDeliveries(ctx, movieID, 10)
	if len(reread[0].FileResults) != 1 || !reread[0].FileResults[0].Matched {
		t.Fatalf("file results not stored: %+v", reread[0].FileResults)
	}
}

// A season pack is one runner job: its download is shown once, on the
// season, and each episode's strip carries its own share of the later stages.
func TestItemStatusShowsASeasonPackOnceOnTheSeason(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	if _, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, Season: 1, ReleaseTitle: "Test.Show.S01.1080p.WEB-DL",
		Indexer: "idx", Protocol: "torrent", Size: 1000, State: "downloading", Progress: 0.5,
		WantableIDs: []string{"episode:" + strconv.FormatInt(itemID, 10) + ":1:1", "episode:" + strconv.FormatInt(itemID, 10) + ":1:2"}}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.ItemStatus(ctx, itemID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Seasons) != 1 || st.Seasons[0].Download == nil || st.Seasons[0].Download.Episodes != 2 || !st.Active {
		t.Fatalf("seasons = %+v, want season 1 carrying the two-episode pack", st.Seasons)
	}
	if !strings.Contains(st.Seasons[0].Summary, "0 of 2 in cinema · 2 in one season pack") {
		t.Fatalf("summary = %q", st.Seasons[0].Summary)
	}
	st, err = svc.ItemStatus(ctx, itemID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Episodes) != 2 || !st.Episodes[0].InPack || len(st.Episodes[0].Stages) != 6 || !strings.HasPrefix(st.Episodes[0].Summary, "in season pack") {
		t.Fatalf("episodes = %+v, want two pack episodes with six stages", st.Episodes)
	}
}
