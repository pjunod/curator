package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestReplacementBlacklistRequiresExactSourceAndRequestReplayIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Exact blacklist", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	base := ReplacementOperation{
		ID: "blacklist-exact", RequestID: "blacklist-exact-request", Fingerprint: "blacklist-exact-fingerprint",
		State: "accepted", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
		BlacklistSourceRelease: true, ReasonCode: "bad_media", Reason: "exact release evidence",
	}
	if _, _, err := db.CreateReplacementOperation(ctx, base, nil); err == nil {
		t.Fatal("blacklist accepted without an exact source GUID and indexer identity")
	}
	assertBlacklistRows := func(wantOps, wantBlacklist int) {
		t.Helper()
		var ops, blacklisted int
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operations WHERE request_id=?`, base.RequestID).Scan(&ops); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM release_blacklist_identity WHERE media_item_id=?`, itemID).Scan(&blacklisted); err != nil {
			t.Fatal(err)
		}
		if ops != wantOps || blacklisted != wantBlacklist {
			t.Fatalf("replacement blacklist transaction rows: operations=%d blacklist=%d, want %d/%d", ops, blacklisted, wantOps, wantBlacklist)
		}
	}
	assertBlacklistRows(0, 0)

	base.SourceGUID, base.SourceIndexerID = "indexer-guid-2024-release", 47
	created, replay, err := db.CreateReplacementOperation(ctx, base, nil)
	if err != nil || replay || created.ID != base.ID {
		t.Fatalf("exact blacklist operation created=%+v replay=%v err=%v", created, replay, err)
	}
	duplicate := base
	duplicate.ID = "ignored-replay-id"
	duplicate.Reason = "stale replay detail"
	stored, replay, err := db.CreateReplacementOperation(ctx, duplicate, nil)
	if err != nil || !replay || stored.ID != base.ID || stored.Reason != base.Reason {
		t.Fatalf("same-request replay did not return original exact decision: operation=%+v replay=%v err=%v", stored, replay, err)
	}
	changed := base
	changed.Fingerprint = "changed-content"
	if _, _, err := db.CreateReplacementOperation(ctx, changed, nil); !errors.Is(err, ErrReplacementRequestConflict) {
		t.Fatalf("changed request replay error=%v, want conflict", err)
	}
	assertBlacklistRows(1, 1)
	var guid string
	if err := db.R.QueryRowContext(ctx, `SELECT guid FROM release_blacklist_identity WHERE media_item_id=?`, itemID).Scan(&guid); err != nil || guid != base.SourceGUID {
		t.Fatalf("blacklist did not preserve exact source identity: guid=%q err=%v", guid, err)
	}
}

func TestDeleteReplacementFileRecordRollsBackJournalFailureAndReplaysAbsence(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Replacement delete replay", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := "/library/replacement-delete-replay.mkv"
	result, err := db.W.ExecContext(ctx, `INSERT INTO media_files(media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?)`, itemID, path, 99, time.Now().UnixMilli(), "file-generation-1", "source-generation-1")
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	op := ReplacementOperation{
		ID: "replacement-delete-replay", RequestID: "replacement-delete-replay-request", Fingerprint: "replacement-delete-replay-fingerprint",
		State: "removing", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
		SourceGeneration: "source-generation-1", ReasonCode: "bad_media", Reason: "delete exact selected source",
	}
	f := ReplacementFile{MediaFileID: fileID, FileGeneration: "file-generation-1", SourceGeneration: "source-generation-1", Path: path, Size: 99, Disposition: "selected", RemovalState: "protected"}
	if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{f}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_replacement_delete_journal BEFORE UPDATE OF removal_state ON replacement_operation_files WHEN NEW.operation_id='replacement-delete-replay' BEGIN SELECT RAISE(ABORT,'injected removal journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteReplacementFileRecord(ctx, op.ID, f); err == nil {
		t.Fatal("source row deletion committed without its durable removal journal")
	}
	var sourceRows int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=? AND file_generation=?`, fileID, f.FileGeneration).Scan(&sourceRows); err != nil || sourceRows != 1 {
		t.Fatalf("failed removal journal did not roll back source row: rows=%d err=%v", sourceRows, err)
	}
	snapshots, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(snapshots) != 1 || snapshots[0].RemovalState != "protected" {
		t.Fatalf("failed removal changed approved source snapshot: snapshots=%+v err=%v", snapshots, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_replacement_delete_journal`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteReplacementFileRecord(ctx, op.ID, f); err != nil {
		t.Fatalf("exact removal retry failed: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=?`, fileID).Scan(&sourceRows); err != nil || sourceRows != 0 {
		t.Fatalf("successful removal left the source row: rows=%d err=%v", sourceRows, err)
	}
	if err := db.DeleteReplacementFileRecord(ctx, op.ID, f); err != nil {
		t.Fatalf("replay after source-row removal did not finish the journal: %v", err)
	}
	snapshots, err = db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(snapshots) != 1 || snapshots[0].RemovalState != "removed" {
		t.Fatalf("missing-source replay failed to settle exact journal: snapshots=%+v err=%v", snapshots, err)
	}
	stale := f
	stale.FileGeneration = "reused-row-generation"
	if err := db.DeleteReplacementFileRecord(ctx, op.ID, stale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reused row ID with a different generation was accepted: %v", err)
	}
}

func TestCommitReplacementPlacementRequiresCompleteSelectedEpisodeScope(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Selected replacement scope", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, episode := range []struct {
		number int
		title  string
	}{{1, "One"}, {2, "Two"}} {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO episodes(media_item_id,season_number,episode_number,title,monitored) VALUES(?,1,?,?,1)`, itemID, episode.number, episode.title); err != nil {
			t.Fatal(err)
		}
	}
	episodeOne, err := db.GetEpisodeID(ctx, itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	episodeTwo, err := db.GetEpisodeID(ctx, itemID, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := "/library/selected-replacement-source.mkv"
	result, err := db.W.ExecContext(ctx, `INSERT INTO media_files(media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?)`, itemID, sourcePath, 50, time.Now().UnixMilli(), "old-file-generation", "old-source-generation")
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, fileID, episodeOne); err != nil {
		t.Fatal(err)
	}
	op := ReplacementOperation{
		ID: "selected-replacement-scope", RequestID: "selected-replacement-scope-request", Fingerprint: "selected-replacement-scope-fingerprint",
		State: "replacing", Intent: "replace_quality", Scope: "episodes", MediaItemID: itemID,
		SourceGeneration: "old-source-generation", EpisodeIDs: []int64{episodeOne}, ReasonCode: "quality_preference", Reason: "replace one selected episode",
	}
	f := ReplacementFile{MediaFileID: fileID, FileGeneration: "old-file-generation", SourceGeneration: "old-source-generation", Path: sourcePath, EpisodeIDs: []int64{episodeOne}, Disposition: "preserved", RemovalState: "protected"}
	if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{f}); err != nil {
		t.Fatal(err)
	}
	p := Placement{ID: "selected-replacement-placement", ReplacementOperationID: op.ID, ItemID: itemID, Target: "/library/selected-replacement-output.mkv", Size: 100, SHA256: "accepted-output-digest", State: "prepared"}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		name      string
		episodes  []int64
		wantError string
	}{
		{name: "missing selected scope", wantError: "no selected episode scope"},
		{name: "unselected episode", episodes: []int64{episodeTwo}, wantError: "exceeds selected episode scope"},
		{name: "selected and unselected together", episodes: []int64{episodeOne, episodeTwo}, wantError: "exceeds selected episode scope"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			attempt := p
			attempt.EpisodeIDs = invalid.episodes
			if _, err := db.CommitPlacement(ctx, attempt); err == nil || !strings.Contains(err.Error(), invalid.wantError) {
				t.Fatalf("unauthorized placement episodes=%v error=%v, want %q", invalid.episodes, err, invalid.wantError)
			}
			var outputs int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&outputs); err != nil || outputs != 0 {
				t.Fatalf("rejected scope left output metadata: rows=%d err=%v", outputs, err)
			}
		})
	}
	p.EpisodeIDs = []int64{episodeOne}
	fileID, err = db.CommitPlacement(ctx, p)
	if err != nil {
		t.Fatalf("exact selected episode placement did not commit after rejected attempts: %v", err)
	}
	var outputLinks, otherLinks int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes WHERE media_file_id=? AND episode_id=?`, fileID, episodeOne).Scan(&outputLinks); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes WHERE media_file_id=? AND episode_id=?`, fileID, episodeTwo).Scan(&otherLinks); err != nil {
		t.Fatal(err)
	}
	if outputLinks != 1 || otherLinks != 0 {
		t.Fatalf("committed placement escaped its approved episode scope: selected=%d unselected=%d", outputLinks, otherLinks)
	}
}

func TestExplicitDeliveryFileOutcomeRejectsPathAndGenerationReuse(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	receipt, err := db.SaveDiscoveredDelivery(ctx, 81, coverageDelivery("explicit-file-authority"), "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	digest := "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	for _, invalid := range []DeliveryManifestFile{
		{Path: "../outside.mkv", SizeBytes: 5, SHA256: digest, FileGeneration: "file-generation"},
		{Path: "/absolute.mkv", SizeBytes: 5, SHA256: digest, FileGeneration: "file-generation"},
		{Path: "Movie.mkv", SizeBytes: 5, SHA256: "not-a-sha256", FileGeneration: "file-generation"},
		{Path: "Movie.mkv", SizeBytes: 5, SHA256: digest},
	} {
		if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, invalid); err == nil {
			t.Fatalf("invalid explicit file identity was accepted: %+v", invalid)
		}
	}
	files, err := db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil || len(files) != 0 {
		t.Fatalf("rejected file identities left cleanup authority behind: outcomes=%+v err=%v", files, err)
	}
	valid := DeliveryManifestFile{Path: "Movie.mkv", SizeBytes: 5, SHA256: digest, FileGeneration: "file-generation-1"}
	if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, valid); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, valid); err != nil {
		t.Fatalf("same immutable explicit-file replay failed: %v", err)
	}
	changed := valid
	changed.FileGeneration = "reused-file-generation"
	if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, changed); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("reused manifest path with changed generation error=%v, want stale receipt", err)
	}
	files, err = db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil || len(files) != 1 || files[0].FileGeneration != valid.FileGeneration {
		t.Fatalf("stale retry replaced the pinned manifest generation: outcomes=%+v err=%v", files, err)
	}
}
