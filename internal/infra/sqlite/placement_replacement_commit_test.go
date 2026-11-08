package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A replacement placement may only land inside the episode scope the
// operation snapshotted, and a replace_bad one only once its bad source is
// gone. Every refusal rolls the whole commit back: no file row, no episode
// link, no placement receipt, and the placement stays prepared.

func replacementPlacement(f replacementStoreFixture, id string, episodes []int64) Placement {
	return Placement{ID: id, ItemID: 88, Source: "/stage/" + id + ".mkv", Target: "/library/Show/" + id + ".mkv", SHA256: "new-" + id, Size: 20,
		EpisodeIDs: episodes, ReplacementOperationID: f.op.ID, Release: "Show.S01E01.Proper", Indexer: "idx", State: "prepared"}
}

func commitReplacementPlacement(t *testing.T, f replacementStoreFixture, p Placement) (int64, error) {
	t.Helper()
	if err := f.db.PreparePlacement(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return f.db.CommitPlacement(context.Background(), p)
}

func requirePlacementRolledBack(t *testing.T, db *DB, p Placement) {
	t.Helper()
	var files, receipts int
	var state string
	if err := db.R.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM media_files WHERE path=?), (SELECT count(*) FROM media_file_placements WHERE placement_id=?), (SELECT state FROM import_placements WHERE id=?)`, p.Target, p.ID, p.ID).Scan(&files, &receipts, &state); err != nil {
		t.Fatal(err)
	}
	if files != 0 || receipts != 0 || state != "prepared" {
		t.Fatalf("refused placement left files=%d receipts=%d state=%q", files, receipts, state)
	}
}

func TestReplacementPlacementCommitRefusesOutsideSnapshottedScope(t *testing.T) {
	for _, c := range []struct {
		name     string
		episodes []int64
		setup    func(t *testing.T, f replacementStoreFixture)
		want     string
	}{
		{name: "no-episodes", episodes: nil, want: "no selected episode scope"},
		{name: "beyond-operation", episodes: []int64{881, 882}, want: "exceeds selected episode scope"},
		{name: "bad-source-not-removed", episodes: []int64{881}, want: "has not completed its approved cleanup"},
		{name: "removed-source-reappeared", episodes: []int64{881}, want: "reappeared after removal", setup: func(t *testing.T, f replacementStoreFixture) {
			if err := f.db.SetReplacementFileRemovalState(t.Context(), f.op.ID, f.file, "removed"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "episode-outside-file-snapshot", episodes: []int64{881, 882}, want: "episode 882 is outside the snapshotted source scope", setup: func(t *testing.T, f replacementStoreFixture) {
			// The operation reserves both episodes but snapshotted only the
			// first one's file.
			if _, err := f.db.W.ExecContext(t.Context(), `UPDATE replacement_operations SET episode_ids='[881,882]' WHERE operation_id=?`, f.op.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.db.DeleteReplacementFileRecord(t.Context(), f.op.ID, f.file); err != nil {
				t.Fatal(err)
			}
		}},
		// An excess file of a replace_bad operation is kept until the new one
		// lands, so it must still be exactly what was snapshotted.
		{name: "excess-source-disappeared", episodes: []int64{881}, want: "disappeared before commit", setup: func(t *testing.T, f replacementStoreFixture) {
			if _, err := f.db.W.ExecContext(t.Context(), `UPDATE replacement_operation_files SET disposition='excess' WHERE operation_id=?`, f.op.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.W.ExecContext(t.Context(), `DELETE FROM media_files WHERE id=880`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "excess-source-regenerated", episodes: []int64{881}, want: "generation changed before commit", setup: func(t *testing.T, f replacementStoreFixture) {
			if _, err := f.db.W.ExecContext(t.Context(), `UPDATE replacement_operation_files SET disposition='excess' WHERE operation_id=?`, f.op.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.W.ExecContext(t.Context(), `UPDATE media_files SET file_generation='regenerated' WHERE id=880`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file-outside-snapshot", episodes: []int64{881}, want: "outside the snapshotted source scope", setup: func(t *testing.T, f replacementStoreFixture) {
			if _, err := f.db.W.ExecContext(t.Context(), `UPDATE replacement_operation_files SET episode_ids='[882]' WHERE operation_id=?`, f.op.ID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newReplacementStoreFixture(t)
			if c.setup != nil {
				c.setup(t, f)
			}
			p := replacementPlacement(f, "pl-"+c.name, c.episodes)
			_, err := commitReplacementPlacement(t, f, p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("commit error = %v, want %q", err, c.want)
			}
			requirePlacementRolledBack(t, f.db, p)
		})
	}
}

func TestReplacementPlacementCommitsAfterApprovedRemovalAndReplaysExactly(t *testing.T) {
	ctx := context.Background()
	f := newReplacementStoreFixture(t)
	if err := f.db.DeleteReplacementFileRecord(ctx, f.op.ID, f.file); err != nil {
		t.Fatal(err)
	}
	p := replacementPlacement(f, "pl-good", []int64{881})
	fid, err := commitReplacementPlacement(t, f, p)
	if err != nil {
		t.Fatal(err)
	}
	placements, err := f.db.CurrentReplacementPlacements(ctx, f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got *ReplacementPlacement
	for i := range placements {
		if placements[i].PlacementID == p.ID {
			got = &placements[i]
		}
	}
	if got == nil || got.MediaFileID != fid || got.FileGeneration != p.ID || got.Path != p.Target || len(got.EpisodeIDs) != 1 || got.EpisodeIDs[0] != 881 {
		t.Fatalf("committed replacement placement = %+v (all %+v)", got, placements)
	}
	covered, err := f.db.ReplacementHasCommittedCoverage(ctx, f.op)
	if err != nil || !covered {
		t.Fatalf("operation scope not covered after commit: %v %v", covered, err)
	}
	// A replay of the committed receipt returns the same file and writes nothing.
	before := pipelineSnapshot(t, f.db)
	p.State = "committed"
	again, err := f.db.CommitPlacement(ctx, p)
	if err != nil || again != fid {
		t.Fatalf("replay = %d, %v; want %d", again, err, fid)
	}
	if after := pipelineSnapshot(t, f.db); after != before {
		t.Fatal("replaying a committed placement wrote again")
	}
	// A replay whose receipt no longer names the library generation is refused.
	changed := p
	changed.SHA256 = "other"
	if _, err := f.db.CommitPlacement(ctx, changed); !errors.Is(err, ErrPlacementGenerationChanged) {
		t.Fatalf("replay with a changed digest = %v", err)
	}
	if _, err := f.db.W.ExecContext(ctx, `UPDATE media_files SET file_generation='edited' WHERE id=?`, fid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CommitPlacement(ctx, p); !errors.Is(err, ErrPlacementGenerationChanged) {
		t.Fatalf("replay over a regenerated library file = %v", err)
	}
	if _, err := f.db.W.ExecContext(ctx, `DELETE FROM media_files WHERE id=?`, fid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CommitPlacement(ctx, p); !errors.Is(err, ErrPlacementGenerationChanged) {
		t.Fatalf("replay over a removed library file = %v", err)
	}
}

func TestReplacementPlacementCommitStorageFailuresRollBack(t *testing.T) {
	eachWriteFault(t, func(t *testing.T, fault *importFaultDriver) {
		f := newReplacementStoreFixture(t)
		if err := f.db.DeleteReplacementFileRecord(t.Context(), f.op.ID, f.file); err != nil {
			t.Fatal(err)
		}
		p := replacementPlacement(f, "pl-fault", []int64{881})
		if err := f.db.PreparePlacement(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		before := pipelineSnapshot(t, f.db)
		err := withFaultyConn(t, f.db, true, fault, func() error { _, e := f.db.CommitPlacement(t.Context(), p); return e })
		requireWriteFaultOutcome(t, fault, true, err, before, pipelineSnapshot(t, f.db))
	})
}
