package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A replacement is the operator's authorization to remove a library file and
// grab another. A storage failure while it is admitted, reported or observed
// must be visible, and the operator's retry must land exactly one operation:
// never a second one for the same request, never a lost one.

type replacementFaultFixture struct {
	svc        *Service
	db         *sqlite.DB
	movieID    int64
	fileID     int64
	generation string
	request    ReplacementRequest
}

func newReplacementFaultFixture(t *testing.T) replacementFaultFixture {
	t.Helper()
	ctx := context.Background()
	svc, db, movieID := autoSetupOn(t, migratedTestDB(t), []ports.Release{rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 20)}, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "reviewed.mkv", "Test.Movie.2024.720p.WEB-DL.x264-OLD")
	payload := []byte("reviewed bytes")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=?`, fileID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-generation' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_file_placements(placement_id,media_file_id,file_generation,source_generation,path,sha256,committed_at) VALUES('old-placement',?,?,'old-generation',?,?,?)`, fileID, generation, path, hex.EncodeToString(digest[:]), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	observation := observeReplacementFile(t, svc, movieID, fileID, generation, "fault-observe")
	blacklist := false
	return replacementFaultFixture{svc: svc, db: db, movieID: movieID, fileID: fileID, generation: generation, request: ReplacementRequest{
		RequestID: "fault-replacement", SourceGeneration: "old-generation", Scope: "download", MediaItemID: movieID, SourceFileID: fileID,
		Intent: "replace_quality", BlacklistSourceRelease: &blacklist, ReasonCode: "upgrade", Reason: "reviewed",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}},
	}}
}

func TestReplacementAdmissionStorageFaultsLandExactlyOneOperation(t *testing.T) {
	ctx := context.Background()
	ops := map[string]func(f replacementFaultFixture) error{
		"create": func(f replacementFaultFixture) error {
			_, _, err := f.svc.CreateReplacement(ctx, f.request)
			return err
		},
		"report": func(f replacementFaultFixture) error {
			return f.svc.ReportLibraryFile(ctx, f.movieID, f.fileID, f.generation, "bad_media", "reported", nil)
		},
		"observe": func(f replacementFaultFixture) error {
			_, _, err := f.svc.StartReplacementFileObservation(ctx, f.movieID, f.fileID, f.generation, "fault-observe-2")
			return err
		},
	}
	for name, op := range ops {
		for _, write := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/write=%t", name, write), func(t *testing.T) {
				eachStorageFault(t, func(t *testing.T, f *storageFault) {
					fx := newReplacementFaultFixture(t)
					var faulted error
					withStorageFault(t, fx.db, write, f, func() { faulted = op(fx) })
					if f.fired.Load() && faulted != nil && !errors.Is(faulted, errStorageFault) {
						t.Fatalf("storage failure surfaced as %v", faulted)
					}
					if !f.fired.Load() && faulted != nil {
						t.Fatalf("unfaulted %s failed: %v", name, faulted)
					}
					// The operator retries the same request.
					if err := op(fx); err != nil {
						t.Fatalf("retry after failure (err=%v): %v", faulted, err)
					}
					var operations, flags, observations int
					if err := fx.db.R.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM replacement_operations), (SELECT count(*) FROM library_review_flags WHERE state='open'), (SELECT count(*) FROM replacement_file_observations WHERE request_id='fault-observe-2')`).Scan(&operations, &flags, &observations); err != nil {
						t.Fatal(err)
					}
					switch name {
					case "create":
						if operations != 1 {
							t.Fatalf("retry after failure (err=%v) left %d operations", faulted, operations)
						}
						op, err := fx.db.GetReplacementOperationByRequestID(ctx, fx.request.RequestID)
						if err != nil || op.MediaItemID != fx.movieID {
							t.Fatalf("operation after retry = %+v, %v", op, err)
						}
						files, err := fx.db.ListReplacementFiles(ctx, op.ID)
						if err != nil || len(files) != 1 || files[0].MediaFileID != fx.fileID {
							t.Fatalf("operation file snapshot after retry = %+v, %v", files, err)
						}
					case "report":
						if flags != 1 {
							t.Fatalf("retry left %d open review flags", flags)
						}
					case "observe":
						if observations != 1 {
							t.Fatalf("retry left %d observations for one request", observations)
						}
					}
				})
			})
		}
	}
}
