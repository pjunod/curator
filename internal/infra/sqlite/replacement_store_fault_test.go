package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The replacement store owns an episode's file through removal, the
// re-grab and the new placement. Its writes run while the library is being
// changed, so a storage failure must be visible to the worker and must not
// half-apply a removal journal or an admission.

type replacementStoreFixture struct {
	db         *DB
	generation string // file 880's file_generation
	op         ReplacementOperation
	file       ReplacementFile
	flagID     int64
}

func newReplacementStoreFixture(t *testing.T) replacementStoreFixture {
	t.Helper()
	ctx := context.Background()
	db := openMigratedCopy(t)
	for _, q := range []string{
		`INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(88,'series','Replaced Show','replaced show',1,1,1)`,
		`INSERT INTO episodes(id,media_item_id,season_number,episode_number) VALUES(881,88,1,1),(882,88,1,2)`,
		`INSERT INTO media_files(id,media_item_id,path,size,added_at,source_generation) VALUES(880,88,'/library/Show/S01E01.mkv',10,1,'src-gen'),(890,88,'/library/Show/S01E02.mkv',11,1,'src-gen-2')`,
		`INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(880,881),(890,882)`,
	} {
		if _, err := db.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var gen, gen2 string
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=880`).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=890`).Scan(&gen2); err != nil {
		t.Fatal(err)
	}
	file := ReplacementFile{MediaFileID: 880, FileGeneration: gen, SourceGeneration: "src-gen", Path: "/library/Show/S01E01.mkv", SHA256: "aa", EpisodeIDs: []int64{881}, Disposition: "selected", RemovalState: "pending"}
	op := ReplacementOperation{ID: "op-fixture", RequestID: "req-fixture", Fingerprint: "fp", State: "searching", Intent: "replace_bad", Scope: "episodes", MediaItemID: 88, EpisodeIDs: []int64{881}}
	op, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_file_placements(placement_id,media_file_id,file_generation,path,sha256,committed_at,replacement_operation_id) VALUES('pl-890',890,?,'/library/Show/S01E02.mkv','bb',1,?)`, []any{gen2, op.ID}},
		{`INSERT INTO downloads(media_item_id,release_title,protocol,state,added_at,updated_at,replacement_operation_id) VALUES(88,'Replacement','usenet','grabbed',?,?,?)`, []any{now, now, op.ID}},
		{`INSERT INTO replacement_file_observations(observation_id,request_id,media_item_id,media_file_id,file_generation,source_generation,path,storage_identity,state,sha256,created_at,updated_at) VALUES('obs-ready','obs-req-ready',88,880,?,'src-gen','/library/Show/S01E01.mkv','nfs:lib','ready','aa',1,1)`, []any{gen}},
		{`INSERT INTO replacement_file_observations(observation_id,request_id,media_item_id,media_file_id,file_generation,source_generation,path,storage_identity,state,created_at,updated_at) VALUES('obs-pending','obs-req-pending',88,880,?,'src-gen','/library/Show/S01E01.mkv','nfs:lib','pending',2,2)`, []any{gen}},
	} {
		if _, err := db.W.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if err := db.CreateLibraryReviewFlag(ctx, 88, 880, gen, "damaged_output", "probe failed", `{}`); err != nil {
		t.Fatal(err)
	}
	flags, err := db.ListOpenMediaFileReviewFlags(ctx, 88)
	if err != nil || len(flags) != 1 {
		t.Fatalf("fixture flag: %+v %v", flags, err)
	}
	return replacementStoreFixture{db: db, generation: gen, op: op, file: file, flagID: flags[0].ID}
}

func TestReplacementStoreWriteFailuresAreReportedAndAtomic(t *testing.T) {
	ctx := context.Background()
	type writeCase struct {
		atomic bool
		run    func(f replacementStoreFixture) error
	}
	cases := map[string]writeCase{
		"create-operation": {atomic: true, run: func(f replacementStoreFixture) error {
			// A second episode's operation, with its file snapshot and an
			// exact-identity blacklist, admitted in one transaction.
			var gen2 string
			if err := f.db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=890`).Scan(&gen2); err != nil {
				return err
			}
			_, replay, err := f.db.CreateReplacementOperation(ctx, ReplacementOperation{ID: "op-2", RequestID: "req-2", Fingerprint: "fp2", State: "accepted", Intent: "replace_bad", Scope: "episodes",
				MediaItemID: 88, EpisodeIDs: []int64{882}, BlacklistSourceRelease: true, SourceGUID: "guid", SourceIndexerID: 3, Reason: "bad"},
				[]ReplacementFile{{MediaFileID: 890, FileGeneration: gen2, SourceGeneration: "src-gen-2", Path: "/library/Show/S01E02.mkv", EpisodeIDs: []int64{882}, Disposition: "selected", RemovalState: "protected"}})
			if err == nil && replay {
				return errors.New("fresh request reported as a replay")
			}
			return err
		}},
		"create-replay": {atomic: true, run: func(f replacementStoreFixture) error {
			_, replay, err := f.db.CreateReplacementOperation(ctx, f.op, nil)
			if err == nil && !replay {
				return errors.New("replayed request admitted twice")
			}
			if err == nil {
				// A replay commits nothing new; mark the run so the matrix
				// sees the unfaulted call reach the database.
				_, err = f.db.W.ExecContext(ctx, `UPDATE replacement_operations SET last_error='replayed' WHERE operation_id=?`, f.op.ID)
			}
			return err
		}},
		"transition-blocked": {atomic: true, run: func(f replacementStoreFixture) error {
			return f.db.TransitionReplacementOperation(ctx, f.op.ID, "waiting_for_release", "no release yet")
		}},
		"detach-episodes": {atomic: true, run: func(f replacementStoreFixture) error {
			return f.db.DetachReplacementEpisodes(ctx, f.op.ID, f.file, []int64{881})
		}},
		"delete-file-record": {atomic: true, run: func(f replacementStoreFixture) error {
			return f.db.DeleteReplacementFileRecord(ctx, f.op.ID, f.file)
		}},
		"start-observation": {atomic: true, run: func(f replacementStoreFixture) error {
			_, _, err := f.db.StartReplacementFileObservation(ctx, ReplacementFileObservation{ID: "obs-new", RequestID: "obs-req-new", MediaItemID: 88, MediaFileID: 880, FileGeneration: f.generation, StorageIdentity: "nfs:lib"})
			return err
		}},
		"removal-state": {run: func(f replacementStoreFixture) error {
			return f.db.SetReplacementFileRemovalState(ctx, f.op.ID, f.file, "blocked")
		}},
		"finish-observation": {run: func(f replacementStoreFixture) error {
			return f.db.FinishReplacementFileObservation(ctx, ReplacementFileObservation{ID: "obs-pending", State: "ready", SHA256: "aa", Size: 10, MediaItemID: 88, MediaFileID: 880, FileGeneration: f.generation, Path: f.file.Path})
		}},
		"review-flag": {run: func(f replacementStoreFixture) error {
			return f.db.CreateLibraryReviewFlag(ctx, 88, 880, f.generation, "damaged_output", "probe failed again", `{"n":2}`)
		}},
		"resolve-flag": {run: func(f replacementStoreFixture) error {
			return f.db.ResolveMediaFileReviewFlag(ctx, 88, f.flagID, f.generation)
		}},
		"pin-storage": {run: func(f replacementStoreFixture) error {
			return f.db.PinReplacementStorageIdentity(ctx, "/library", "nfs:lib", "obs-ready")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			eachWriteFault(t, func(t *testing.T, fault *importFaultDriver) {
				f := newReplacementStoreFixture(t)
				before := pipelineSnapshot(t, f.db)
				err := withFaultyConn(t, f.db, true, fault, func() error { return c.run(f) })
				requireWriteFaultOutcome(t, fault, c.atomic, err, before, pipelineSnapshot(t, f.db))
			})
		})
	}
}

// PinReplacementStorageIdentity verifies through the reader: a failed read
// after the enrolment must not be taken as a matching pin.
func TestReplacementStoreVerificationReadFailureIsNotAMatch(t *testing.T) {
	f := newReplacementStoreFixture(t)
	if err := f.db.PinReplacementStorageIdentity(t.Context(), "/library", "nfs:lib", "obs-ready"); err != nil {
		t.Fatal(err)
	}
	err := withFaultyConn(t, f.db, false, &importFaultDriver{at: 1}, func() error {
		return f.db.PinReplacementStorageIdentity(t.Context(), "/library", "nfs:lib", "obs-ready")
	})
	if !errors.Is(err, errImportStorageFault) {
		t.Fatalf("verification read failure = %v", err)
	}
}

func TestReplacementStoreReadFailuresNeverBecomeEmptySuccess(t *testing.T) {
	ctx := context.Background()
	f := newReplacementStoreFixture(t)
	db := f.db
	found := func(ok bool) int {
		if ok {
			return 1
		}
		return 0
	}
	download := f.op
	download.Scope, download.EpisodeIDs = "download", nil
	episodes := f.op
	episodes.EpisodeIDs = []int64{882}
	calls := map[string]func() (int, error){
		"current-placements": func() (int, error) { v, e := db.CurrentReplacementPlacements(ctx, f.op.ID); return len(v), e },
		"operation":          func() (int, error) { op, e := db.GetReplacementOperation(ctx, f.op.ID); return len(op.ID), e },
		"by-request": func() (int, error) {
			op, e := db.GetReplacementOperationByRequestID(ctx, f.op.RequestID)
			return len(op.ID), e
		},
		"files":             func() (int, error) { v, e := db.ListReplacementFiles(ctx, f.op.ID); return len(v), e },
		"coverage-download": func() (int, error) { ok, e := db.ReplacementHasCommittedCoverage(ctx, download); return found(ok), e },
		"coverage-episodes": func() (int, error) { ok, e := db.ReplacementHasCommittedCoverage(ctx, episodes); return found(ok), e },
		"open-flag": func() (int, error) {
			ok, e := db.HasOpenMediaFileReviewFlag(ctx, 880, f.generation)
			return found(ok), e
		},
		"flags":           func() (int, error) { v, e := db.ListOpenMediaFileReviewFlags(ctx, 88); return len(v), e },
		"for-item":        func() (int, error) { v, e := db.ListReplacementOperationsForItem(ctx, 88, 0); return len(v), e },
		"open-operations": func() (int, error) { v, e := db.ListOpenReplacementOperations(ctx, 0); return len(v), e },
		"downloads":       func() (int, error) { v, e := db.ListReplacementDownloads(ctx, f.op.ID); return len(v), e },
		"observation":     func() (int, error) { o, e := db.GetReplacementFileObservation(ctx, "obs-ready"); return len(o.ID), e },
		"pending-observations": func() (int, error) {
			v, e := db.ListPendingReplacementFileObservations(ctx, 0)
			return len(v), e
		},
		"prior-storage": func() (int, error) {
			id, _, e := db.PriorVerifiedReplacementStorageIdentity(ctx, 880, f.generation)
			return len(id), e
		},
		"exclusions": func() (int, error) { v, e := db.ReplacementSourceExclusions(ctx, 88, 0); return len(v), e },
	}
	before := pipelineSnapshot(t, db)
	for name, call := range calls {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", name, malformed), func(t *testing.T) {
				completed := false
				for point := 1; point <= 32 && !completed; point++ {
					fault := &importFaultDriver{at: point, malformed: malformed}
					var count int
					err := withFaultyConn(t, db, false, fault, func() error {
						var e error
						count, e = call()
						return e
					})
					if !fault.fired {
						if err != nil || count == 0 {
							t.Fatalf("unfaulted populated read returned %d, %v", count, err)
						}
						completed = true
						continue
					}
					if err == nil {
						t.Fatalf("read failure %d returned success with %d rows", point, count)
					}
				}
				if !completed {
					t.Fatal("read never completed without injection")
				}
			})
		}
	}
	if after := pipelineSnapshot(t, db); after != before {
		t.Fatal("a read changed durable state")
	}
}
