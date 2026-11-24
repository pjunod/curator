package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func availabilityFixture(t *testing.T, db *DB, item domain.MediaItem) int64 {
	t.Helper()
	root, err := db.AddRootFolderWithPresence(context.Background(), "/library/"+string(item.Kind), domain.KindMixed, "volume-1")
	if err != nil {
		t.Fatal(err)
	}
	item.RootFolderID = root.ID
	item.Path = root.Path + "/title"
	id, err := db.CreateMediaItem(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func availabilityProof(t *testing.T, db *DB, id int64, checked time.Time, files ...int64) {
	t.Helper()
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := db.ListRootFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var root domain.RootFolder
	for _, r := range roots {
		if r.ID == item.RootFolderID {
			root = r
		}
	}
	revision, shared, err := db.PresenceRevision(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	identity, generation, err := db.PresenceRootBinding(ctx, root.ID, root.Path)
	if err != nil {
		t.Fatal(err)
	}
	err = db.SavePresence(ctx, PresenceObservation{ItemID: id, ItemRevision: revision, SharedRevision: shared, RootID: root.ID, RootPath: root.Path, RootIdentity: identity, RootGeneration: generation, ScopePath: item.Path, CheckedAt: checked, PresentFileIDs: files})
	if err != nil {
		t.Fatal(err)
	}
}
func lookupOne(t *testing.T, db *DB, target domain.AvailabilityTarget) domain.Availability {
	t.Helper()
	got, err := db.LookupAvailability(context.Background(), []domain.AvailabilityTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatal(got)
	}
	return got[0]
}
func TestAvailabilitySeparatesIdentityFromPrimaryPresence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	series := availabilityFixture(t, db, sampleSeries())
	movie := availabilityFixture(t, db, domain.MediaItem{Kind: domain.KindMovie, Title: "Same numeric ID", IDs: domain.ExternalIDs{TMDB: 100, IMDB: "tt100"}})
	cases := []struct {
		name    string
		target  domain.AvailabilityTarget
		catalog string
		id      int64
	}{
		{"series namespace", domain.AvailabilityTarget{Kind: domain.KindSeries, IDs: domain.ExternalIDs{TMDB: 100}}, "present", series},
		{"movie namespace", domain.AvailabilityTarget{Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 100}}, "present", movie},
		{"conflicting namespace", domain.AvailabilityTarget{Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 100, IMDB: "tt999"}}, "ambiguous", 0},
		{"stale local ID", domain.AvailabilityTarget{LibraryItemID: 99999, Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 100}}, "ambiguous", 0},
		{"no identity", domain.AvailabilityTarget{Kind: domain.KindMovie}, "unknown", 0},
		{"untracked", domain.AvailabilityTarget{Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 999}}, "absent", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := lookupOne(t, db, tc.target)
			if v.Catalog != tc.catalog || v.LibraryItemID != tc.id || v.Presence != "unknown" {
				t.Fatalf("%+v", v)
			}
		})
	}
	item, _ := db.GetMediaItemFull(ctx, movie)
	file, err := db.UpsertFile(ctx, movie, 0, item.Path+"/movie.mkv", 10)
	if err != nil {
		t.Fatal(err)
	}
	target := domain.AvailabilityTarget{Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 100}}
	availabilityProof(t, db, movie, time.Now(), file)
	if v := lookupOne(t, db, target); v.Presence != "present" || v.Present != 1 || v.CheckedAt == nil {
		t.Fatalf("fresh %+v", v)
	}
	availabilityProof(t, db, movie, time.Now().Add(-14*time.Hour), file)
	if v := lookupOne(t, db, target); v.Presence != "unknown" || v.Present != 0 {
		t.Fatalf("stale %+v", v)
	}
	availabilityProof(t, db, movie, time.Now(), file)
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET size=11 WHERE id=?`, file); err != nil {
		t.Fatal(err)
	}
	if v := lookupOne(t, db, target); v.Presence != "unknown" {
		t.Fatalf("revision change %+v", v)
	}
	availabilityProof(t, db, movie, time.Now())
	if v := lookupOne(t, db, target); v.Presence != "missing" {
		t.Fatalf("negative proof %+v", v)
	}
}
func TestAvailabilityAiredScopeAndUnallocatedFiles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	item := sampleSeries()
	item.Seasons = append(item.Seasons, domain.Season{Number: 2, Episodes: []domain.Episode{{SeasonNumber: 2, EpisodeNumber: 1, AirDate: "2999-01-01"}}})
	id := availabilityFixture(t, db, item)
	full, _ := db.GetMediaItemFull(ctx, id)
	f, err := db.UpsertFile(ctx, id, 0, full.Path+"/S01E01.mkv", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, f, full.Seasons[1].Episodes[0].ID); err != nil {
		t.Fatal(err)
	}
	target := domain.AvailabilityTarget{Kind: domain.KindSeries, IDs: item.IDs}
	availabilityProof(t, db, id, time.Now(), f)
	if v := lookupOne(t, db, target); v.Presence != "partial" || v.Expected != 2 || v.Present != 1 {
		t.Fatalf("partial %+v", v)
	}
	obfuscated, err := db.UpsertFile(ctx, id, 0, full.Path+"/opaque.mkv", 10)
	if err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, id, time.Now(), f, obfuscated)
	if v := lookupOne(t, db, target); v.Presence != "unknown" || v.Present != 1 || v.Reason != "unallocated_episode_files" {
		t.Fatalf("unallocated %+v", v)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE episodes SET air_date='invalid' WHERE id=?`, full.Seasons[1].Episodes[1].ID); err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, id, time.Now(), f)
	if v := lookupOne(t, db, target); v.Presence != "unknown" || v.Reason != "episode_scope_incomplete" {
		t.Fatalf("invalid date %+v", v)
	}
}
func TestLibraryScanRejectsInterleavedImportsAndForeignPaths(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := availabilityFixture(t, db, sampleSeries())
	snapshot, _ := db.GetMediaItemFull(ctx, id)
	rev, shared, _ := db.PresenceRevision(ctx, id)
	newFile, err := db.UpsertFile(ctx, id, 0, snapshot.Path+"/new.mkv", 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ApplyLibraryScan(ctx, snapshot, rev, shared, nil, map[int64]bool{0: true}, nil)
	if !errors.Is(err, ErrScanChanged) {
		t.Fatalf("race = %v", err)
	}
	var owner int64
	if err = db.R.QueryRowContext(ctx, `SELECT media_item_id FROM media_files WHERE id=?`, newFile).Scan(&owner); err != nil || owner != id {
		t.Fatalf("import lost: %d %v", owner, err)
	}
	other, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Other", IDs: domain.ExternalIDs{TMDB: 9}})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := db.UpsertFile(ctx, other, 0, "/foreign.mkv", 10)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ = db.GetMediaItemFull(ctx, id)
	rev, shared, _ = db.PresenceRevision(ctx, id)
	_, err = db.ApplyLibraryScan(ctx, snapshot, rev, shared, nil, map[int64]bool{0: true}, []ScannedLibraryFile{{Path: snapshot.Path + "/newly-found.mkv", Size: 3}, {Path: "/foreign.mkv", Size: 10}})
	if !errors.Is(err, ErrScanChanged) {
		t.Fatalf("foreign path=%v", err)
	}
	if err = db.R.QueryRowContext(ctx, `SELECT media_item_id FROM media_files WHERE id=?`, foreign).Scan(&owner); err != nil || owner != other {
		t.Fatalf("ownership stolen %d %v", owner, err)
	}
	var count int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, snapshot.Path+"/newly-found.mkv").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial write %d %v", count, err)
	}
}
func TestVolumeRebindFencesScanAndPresence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := availabilityFixture(t, db, sampleSeries())
	item, _ := db.GetMediaItemFull(ctx, id)
	rev, shared, _ := db.PresenceRevision(ctx, id)
	state, err := db.PresenceRootState(ctx, item.RootFolderID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.BindPresenceRoot(ctx, state, "volume-2"); err != nil {
		t.Fatal(err)
	}
	if err = db.BindPresenceRoot(ctx, state, "volume-3"); !errors.Is(err, ErrScanChanged) {
		t.Fatalf("stale confirmation %v", err)
	}
	_, err = db.ApplyLibraryScan(ctx, item, rev, shared, []ScanRootBinding{{RootID: state.RootID, Path: state.Path, Identity: "volume-1", Generation: 1}}, nil, nil)
	if !errors.Is(err, ErrScanChanged) {
		t.Fatalf("old scan %v", err)
	}
	if err = db.CheckPresenceRoot(ctx, state.RootID, state.Path, "volume-1"); !errors.Is(err, ErrPresenceRootChanged) {
		t.Fatalf("old volume %v", err)
	}
}

func TestVolumeBindingCascadesWhenRootIsDeleted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	root, err := db.AddRootFolderWithPresence(ctx, "/reused", domain.KindMixed, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `DELETE FROM root_folders WHERE id=?`, root.ID); err != nil {
		t.Fatal(err)
	}
	next, err := db.AddRootFolderWithPresence(ctx, "/reused", domain.KindMixed, "new")
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := db.PresenceRootBinding(ctx, next.ID, next.Path)
	if err != nil || identity != "new" {
		t.Fatal(identity, err)
	}
}
func TestVolumeRebindBlockedByTerminalRetainedCustody(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := availabilityFixture(t, db, sampleSeries())
	item, _ := db.GetMediaItemFull(ctx, id)
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: id, State: "failed", Protocol: "manual", ReleaseTitle: "retained"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET superseded=1 WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES('retained','/retained','retained',json_object('download_id',?,'item_id',?),0)`, dl.ID, id); err != nil {
		t.Fatal(err)
	}
	state, err := db.PresenceRootState(ctx, item.RootFolderID)
	if err != nil || state.ActiveWork == 0 {
		t.Fatal("terminal retained custody omitted", state, err)
	}
	if err = db.BindPresenceRoot(ctx, state, "replacement"); err == nil {
		t.Fatal("retained volume rebound")
	}
}

func TestPresenceSurvivesDescriptiveAndQualityRefreshOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := availabilityFixture(t, db, sampleSeries())
	item, _ := db.GetMediaItemFull(ctx, id)
	f, err := db.UpsertFile(ctx, id, 0, item.Path+"/S01E01.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, f, item.Seasons[1].Episodes[0].ID); err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, id, time.Now(), f)
	target := domain.AvailabilityTarget{Kind: domain.KindSeries, LibraryItemID: id}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_items SET overview='Refreshed synopsis',updated_at=updated_at+1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE episodes SET title=title,air_date=air_date WHERE media_item_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET quality='webdl-1080p' WHERE id=?`, f); err != nil {
		t.Fatal(err)
	}
	if v := lookupOne(t, db, target); v.Presence != "partial" || v.Present != 1 {
		t.Fatalf("non-presence update expired check %+v", v)
	}
	if _, err = db.W.ExecContext(ctx, `DELETE FROM media_file_episodes WHERE media_file_id=?`, f); err != nil {
		t.Fatal(err)
	}
	if v := lookupOne(t, db, target); v.Presence != "unknown" || v.Present != 0 {
		t.Fatalf("attribution update retained check %+v", v)
	}
}
