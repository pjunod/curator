package acquisition

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Exercise the worker entry point after a crash between creating the
// same-inode quarantine hard link and unlinking the reviewed source name.
func TestReplacementWorkerSweepRecoversQuarantineAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      bool
		wantState   string
		wantErr     string
		wantRemoved bool
	}{
		{name: "valid reviewed bytes", wantState: "replacing", wantRemoved: true},
		{name: "quarantine bytes changed", mutate: true, wantState: "cleanup_blocked", wantErr: "replacement quarantine no longer matches reviewed bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, ctx, op, reviewed, source := reviewedBadReplacementFixture(t)
			client, ok := svc.newClient(ports.ClientConfig{}).(*fakeClient)
			if !ok {
				t.Fatal("replacement fixture did not provide the expected fake client")
			}
			indexer := &mutableWorkerIndexer{releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-QUARANTINE-RECOVERY", 20)}}
			svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
			parent := filepath.Dir(source)
			quarantineName := replacementQuarantinePrefix + op.ID + "-" + itoa(reviewed.MediaFileID)
			quarantine := filepath.Join(parent, quarantineName)
			if err := os.Link(source, quarantine); err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate {
				changed := []byte("xxxxx")
				if err := os.WriteFile(quarantine, changed, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(quarantine, originalInfo.ModTime(), originalInfo.ModTime()); err != nil {
					t.Fatal(err)
				}
			}

			// The replacement operation and exact file review survive process
			// restart; only the OS directory contains the interrupted quarantine.
			reopened := reopenLifecycleDB(t, db)
			restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
			restarted.processReplacementOperations(ctx)

			latest, err := reopened.GetReplacementOperation(ctx, op.ID)
			if err != nil || latest.State != tc.wantState {
				t.Fatalf("worker recovery state=%+v err=%v, want %q", latest, err, tc.wantState)
			}
			fileRecords, err := reopened.ListReplacementFiles(ctx, op.ID)
			if err != nil || len(fileRecords) != 1 {
				t.Fatalf("replacement review records=%+v err=%v", fileRecords, err)
			}
			if tc.wantRemoved {
				if fileRecords[0].RemovalState != "removed" {
					t.Fatalf("recovered review journal=%+v, want removed", fileRecords[0])
				}
				for _, path := range []string{source, quarantine} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("successful recovery left reviewed path %q: %v", path, err)
					}
				}
				if _, err := reopened.GetMediaFileCustodyByPath(ctx, source); !errors.Is(err, sqlite.ErrNotFound) {
					t.Fatalf("retired reviewed source still has a library row: %v", err)
				}
			} else {
				if fileRecords[0].RemovalState == "removed" {
					t.Fatalf("changed quarantine consumed the review obligation: %+v", fileRecords[0])
				}
				wantBytes := []byte("xxxxx")
				for _, path := range []string{source, quarantine} {
					got, readErr := os.ReadFile(path)
					if readErr != nil || !bytes.Equal(got, wantBytes) {
						t.Fatalf("blocked recovery altered evidence at %q: bytes=%q err=%v", path, got, readErr)
					}
				}
				if _, err := reopened.GetMediaFileCustodyByPath(ctx, source); err != nil {
					t.Fatalf("blocked recovery discarded library ownership: %v", err)
				}
				if !strings.Contains(latest.LastError, tc.wantErr) {
					t.Fatalf("blocked recovery error=%q, want %q", latest.LastError, tc.wantErr)
				}
			}
			if tc.wantRemoved {
				if len(client.added) != 1 || len(indexer.releases) != 1 {
					t.Fatalf("verified cleanup did not advance to the available candidate: admissions=%v releases=%+v", client.added, indexer.releases)
				}
				downloads, err := reopened.ListReplacementDownloads(ctx, op.ID)
				if err != nil || len(downloads) != 1 || downloads[0].State != "grabbed" {
					t.Fatalf("replacement admission after verified cleanup=%+v err=%v", downloads, err)
				}
			} else {
				if len(client.added) != 0 {
					t.Fatalf("replacement candidate was admitted before reviewed cleanup was proven: %v", client.added)
				}
				downloads, err := reopened.ListReplacementDownloads(ctx, op.ID)
				if err != nil || len(downloads) != 0 {
					t.Fatalf("blocked cleanup created replacement attempts: downloads=%+v err=%v", downloads, err)
				}
			}
		})
	}
}
