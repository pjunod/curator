package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Field report 2026-11-01: obfuscated posts deliver the episode as a bare
// hash ("321d9664e6a50c195e7e830b3f78b487.mkv"). The grabbed release names
// the episode; the file does not. These tests pin the import, the loop it
// fed, and the guard that RSS skipped.

const obfuscatedRelease = "Test.Show.S01E01.1080p.WEB-DL.DDP2.0.H.264-RAWR"

func grabAndComplete(t *testing.T, svc *Service, client *fakeClient, itemID int64, release string, files ...string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: release, DownloadURL: "magnet:" + release,
		Indexer: "idx", Protocol: "torrent", Size: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), release)
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(payload, name), []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: release, State: ports.StateCompleted, Progress: 1, SavePath: payload,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestObfuscatedEpisodeFileImportsUnderTheGrabbedRelease(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id := grabAndComplete(t, svc, client, itemID, obfuscatedRelease, "321d9664e6a50c195e7e830b3f78b487.mkv")

	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "imported" {
		t.Fatalf("download state = %s (%s); an obfuscated single-episode payload must import under its grabbed release", dl.State, dl.Error)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Files) != 1 {
		t.Fatalf("files = %+v", item.Files)
	}
	if !strings.Contains(item.Files[0].Path, filepath.Join("Season 1", "Test Show - S01E01 - Pilot")) {
		t.Errorf("placed at %q, want the S01E01 slot", item.Files[0].Path)
	}
	if !item.Seasons[0].Episodes[0].HasFile || item.Seasons[0].Episodes[1].HasFile {
		t.Errorf("episode file flags: %+v", item.Seasons[0].Episodes)
	}
}

// Two anonymous files: nothing says which is which, so nothing is guessed.
func TestObfuscatedFilesAreNotGuessedWhenThePayloadHasSeveral(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id := grabAndComplete(t, svc, client, itemID, obfuscatedRelease,
		"321d9664e6a50c195e7e830b3f78b487.mkv", "15713ef716b33f8a63def50814ec76d0.mkv")

	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "failed" || !strings.Contains(dl.Error, "cannot determine episodes") {
		t.Fatalf("state = %s (%s); two anonymous files must fail visibly, not be guessed", dl.State, dl.Error)
	}
}

func TestReleaseEpisodeIdentityOnlyFillsAnAnonymousSoleFile(t *testing.T) {
	anon := "321d9664e6a50c195e7e830b3f78b487.mkv"
	cases := []struct {
		name, file, release string
		files               int
		grabbedSeason       int
		grabbed             []int
		want                bool
		season              int
		episodes            []int
	}{
		{"sole anonymous file, episode release", anon, obfuscatedRelease, 1, 1, []int{1}, true, 1, []int{1}},
		{"multi-episode release covering the grab", anon, "Test.Show.S01E01E02.1080p.WEB-DL-GRP", 1, 1, []int{1}, true, 1, []int{1, 2}},
		{"season pack release", anon, "Test.Show.S01.1080p.WEB-DL-GRP", 1, 1, []int{1}, false, 0, nil},
		{"several media files in the payload", anon, obfuscatedRelease, 2, 1, []int{1}, false, 0, nil},
		{"file names its own episode", "Test.Show.S01E02.mkv", obfuscatedRelease, 1, 1, []int{1}, false, 0, nil},
		{"no release title", anon, "", 1, 1, []int{1}, false, 0, nil},
		{"no recorded episode target", anon, obfuscatedRelease, 1, -1, nil, false, 0, nil},
		{"release disagrees with the grabbed episode", anon, obfuscatedRelease, 1, 1, []int{2}, false, 0, nil},
		{"release disagrees with the grabbed season", anon, obfuscatedRelease, 1, 2, []int{1}, false, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parser.Parse(tc.file)
			got, ok := releaseEpisodeIdentity(p, "/payload/"+tc.file, tc.release, tc.files, tc.grabbedSeason, tc.grabbed)
			if ok != tc.want {
				t.Fatalf("borrowed = %v, want %v", ok, tc.want)
			}
			if !ok {
				if len(got.Episodes) != len(p.Episodes) || got.Season != p.Season {
					t.Errorf("identity changed without borrowing: %+v → %+v", p, got)
				}
				return
			}
			if got.Season != tc.season || len(got.Episodes) != len(tc.episodes) {
				t.Fatalf("got S%d %v, want S%d %v", got.Season, got.Episodes, tc.season, tc.episodes)
			}
			for i := range tc.episodes {
				if got.Episodes[i] != tc.episodes[i] {
					t.Fatalf("got %v, want %v", got.Episodes, tc.episodes)
				}
			}
		})
	}
}

func TestGrabbedEpisodeTargets(t *testing.T) {
	season, eps := grabbedEpisodeTargets([]string{"episode:7:1:3:c2", "episode:7:1:4", "episode:8:1:9", "season:7:1"}, 7)
	if season != 1 || len(eps) != 2 || eps[0] != 3 || eps[1] != 4 {
		t.Fatalf("got S%d %v", season, eps)
	}
	if season, eps := grabbedEpisodeTargets([]string{"episode:7:1:3", "episode:7:2:1"}, 7); season != -1 || eps != nil {
		t.Fatalf("a grab spanning seasons must yield no target, got S%d %v", season, eps)
	}
	if season, eps := grabbedEpisodeTargets([]string{"season:7:1"}, 7); season != -1 || eps != nil {
		t.Fatalf("a season grab must yield no episode target, got S%d %v", season, eps)
	}
}

// Choosing one of two anonymous files says nothing about which episode it
// is: a manual import of a subset must not borrow the release's episodes.
func TestManualImportOfOneAnonymousFileAmongSeveralIsNotGuessed(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id := grabAndComplete(t, svc, client, itemID, obfuscatedRelease,
		"321d9664e6a50c195e7e830b3f78b487.mkv", "15713ef716b33f8a63def50814ec76d0.mkv")
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ManualImport(ctx, ManualImportRequest{
		Path: dl.ImportPath, Paths: []string{filepath.Join(dl.ImportPath, "321d9664e6a50c195e7e830b3f78b487.mkv")},
		MediaItemID: itemID, DownloadID: id,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot determine episodes") {
		t.Fatalf("manual import of one of two anonymous files: err = %v, want a visible identity failure", err)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Files) != 0 {
		t.Fatalf("a guessed placement landed: %+v", item.Files)
	}
}

// The loop: the bytes are here, the import failed, and automation must not
// fetch the same bytes again — while a different release stays grabbable.
func TestAReleaseAwaitingImportIsNotGrabbedAgain(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id := grabAndComplete(t, svc, client, itemID, obfuscatedRelease,
		"321d9664e6a50c195e7e830b3f78b487.mkv", "15713ef716b33f8a63def50814ec76d0.mkv")
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "failed" || dl.PayloadRemoved {
		t.Fatalf("fixture: state=%s payloadRemoved=%v, want a failed import with its payload retained", dl.State, dl.PayloadRemoved)
	}
	client.statuses = nil

	again := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test Show S01E01 1080p WEB-DL DDP2 0 H 264 RAWR", DownloadURL: "magnet:again",
		Indexer: "idx", Protocol: "torrent", Size: 1000,
	}
	if _, err := svc.Grab(ctx, again); !errors.Is(err, ErrReleaseAwaitingImport) {
		t.Fatalf("re-grab of the delivered release: err = %v, want ErrReleaseAwaitingImport", err)
	}
	if len(client.added) != 1 {
		t.Fatalf("client asked to download %d times, want 1", len(client.added))
	}
	if !svc.skipCandidate(ctx, obfuscatedRelease, "idx") {
		t.Error("candidate selection still offers the release whose bytes are awaiting import")
	}

	other := again
	other.Title, other.DownloadURL = "Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", "magnet:other"
	if svc.skipCandidate(ctx, other.Title, "idx") {
		t.Error("a different release must stay eligible")
	}
	if _, err := svc.Grab(ctx, other); err != nil {
		t.Fatalf("a different release must still be grabbable: %v", err)
	}

	// Removing the download from Activity parks it: an operator's decision
	// to let the release be grabbed again.
	if err := svc.RemoveDownload(ctx, id, false); err != nil {
		t.Fatalf("remove from Activity: %v", err)
	}
	if svc.skipCandidate(ctx, obfuscatedRelease, "idx") {
		row, _ := db.GetDownload(ctx, id)
		t.Errorf("a release removed from Activity must be eligible again (row state=%s parked=%v)", row.State, row.ParkedAt)
	}
}

// A download that failed in the client never reached import: its release
// is not "already here".
func TestAClientFailureIsNotTreatedAsAwaitingImport(t *testing.T) {
	payload := t.TempDir()
	dl := sqlite.Download{State: "failed", ImportPath: payload, Handoff: []sqlite.HandoffEntry{{Step: stepGrabbed}, {Step: stepFailed}}}
	if importFailedOnRetainedPayload(dl) {
		t.Error("a client-side failure was read as a retained import failure")
	}
	dl.Handoff = []sqlite.HandoffEntry{{Step: stepGrabbed}, {Step: stepImporting}, {Step: stepFailed}}
	if !importFailedOnRetainedPayload(dl) {
		t.Error("an import failure with its payload retained was not recognised")
	}
	short := dl
	short.Error = "no files imported from x — ep.mkv: " + mediainfo.TruncatedReasonPrefix + ": the container says 4 GiB"
	if importFailedOnRetainedPayload(short) {
		t.Error("a cut-short payload blocks its release; a fresh grab could complete it")
	}
	gone := dl
	gone.ImportPath = filepath.Join(payload, "absent")
	if importFailedOnRetainedPayload(gone) {
		t.Error("a payload no longer on disk was treated as retained")
	}
	removed := dl
	removed.PayloadRemoved = true
	if importFailedOnRetainedPayload(removed) {
		t.Error("a removed payload was treated as retained")
	}
	parked := dl
	parked.ParkedAt = time.Now()
	if importFailedOnRetainedPayload(parked) {
		t.Error("a row parked by the operator was treated as retained")
	}
}

// RSS is an automatic grab: it must respect the regrab cap like every other
// automatic path. The field loop's fourth grab inside one night came through
// RSS, which never consulted the cap.
func TestRSSRespectsTheRegrabCap(t *testing.T) {
	release := ports.Release{Title: "Test.Show.S01E01.1080p.WEB-DL.x264-FRESH", DownloadURL: "magnet:fresh",
		Protocol: "torrent", Indexer: "idx", Seeders: 10, Size: 1 << 30}
	client := &fakeClient{}
	svc, db, itemID := setup(t, []ports.Release{release}, client)
	ctx := context.Background()

	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantID := ""
	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wanted {
		if strings.HasPrefix(string(w.ID()), "episode:") && strings.HasSuffix(string(w.ID()), ":1:1") {
			wantID = string(w.ID())
		}
	}
	if wantID == "" {
		t.Fatalf("fixture: no S01E01 wantable in %v", wanted)
	}
	for i := 0; i < reGrabLimit; i++ {
		id, err := db.InsertDownload(ctx, sqlite.Download{
			MediaItemID: itemID, WantableIDs: []string{wantID},
			ReleaseTitle: "Burned.Release." + strings.Repeat("x", i+1),
			Protocol:     "torrent", ClientID: clients[0].ID, State: "grabbed",
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		row.State = "failed"
		if err := db.UpdateDownloadHandoff(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	svc.InvalidateWanted()
	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 0 {
		t.Fatalf("RSS grabbed %v past the regrab cap", client.added)
	}
}
