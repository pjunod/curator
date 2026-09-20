package library

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestUncreatedDestinationsAreNotFolderProblems(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	rf, err := svc.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []Report{
		{MissingPaths: []string{item.Path}},
		{MissingPaths: []string{item.Path}, MissingItems: []MissingItem{{ID: item.ID, Path: item.Path}}},
	} {
		raw, _ := json.Marshal(legacy)
		if err := db.SetMeta(ctx, scanReportKey, string(raw)); err != nil {
			t.Fatal(err)
		}
		report, ok, err := svc.LastScanReport(ctx)
		if err != nil || !ok || len(report.MissingPaths) != 0 || len(report.MissingItems) != 0 {
			t.Fatalf("old false alarm survived upgrade: %+v, %v", report, err)
		}
	}
	// A separate acquired copy does not make the primary folder overdue.
	copyRoot, err := svc.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := svc.AddCopy(ctx, item.ID, CopyRequest{QualityProfileID: item.QualityProfileID, RootFolderID: copyRoot.ID, Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	cp := copy.Copies[0]
	if _, err := db.UpsertFile(ctx, item.ID, cp.ID, filepath.Join(cp.Path, "movie.mkv"), 42); err != nil {
		t.Fatal(err)
	}
	report, err := svc.Scan(ctx)
	if err != nil || len(report.MissingItems) != 0 {
		t.Fatalf("undownloaded primary reported: %+v, %v", report, err)
	}
	// Once the primary arrives, scanning it must not discard the unavailable
	// separate copy's records. Folder repair uses the same scanner.
	if err := os.Mkdir(item.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, "primary.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, item.ID)
	if err != nil || len(files) != 2 {
		t.Fatalf("scan lost the unavailable copy: %+v, %v", files, err)
	}
}

func TestPreviouslyScannedFolderCanBeRepaired(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(item.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "Test.Show.S01E01.mkv"
	if err := os.WriteFile(filepath.Join(item.Path, name), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "Renamed show")
	if err := os.Rename(item.Path, moved); err != nil {
		t.Fatal(err)
	}
	// Repeat scans must retain the evidence needed to report and repair.
	for range 2 {
		report, err := svc.Scan(ctx)
		if err != nil || len(report.MissingItems) != 1 || report.MissingItems[0].Reason != "missing" {
			t.Fatalf("missing folder report: %+v, %v", report, err)
		}
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{empty, root, filepath.Join(root, "nonexistent"), filepath.Join(moved, name), "relative"} {
		if err := svc.RepairFolder(ctx, item.ID, item.Path, path); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid repair %q: %v", path, err)
		}
		unchanged, err := svc.Get(ctx, item.ID)
		if err != nil || unchanged.Path != item.Path || len(unchanged.Files) != 1 {
			t.Fatalf("invalid repair changed entry: %+v, %v", unchanged, err)
		}
	}
	if err := svc.RepairFolder(ctx, item.ID, "stale", moved); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("stale repair: %v", err)
	}
	if err := svc.RepairFolder(ctx, item.ID, item.Path, moved); err != nil {
		t.Fatal(err)
	}
	fixed, err := svc.Get(ctx, item.ID)
	resolved := moved
	if err != nil || fixed.Path != resolved || len(fixed.Files) != 1 || fixed.Files[0].Path != filepath.Join(resolved, name) || !fixed.Seasons[0].Episodes[0].HasFile {
		t.Fatalf("repair did not relink files and episodes: %+v, %v", fixed, err)
	}
	report, _, err := svc.LastScanReport(ctx)
	if err != nil || len(report.MissingItems) != 0 {
		t.Fatalf("repair still reported: %+v, %v", report, err)
	}
	if _, err := os.Stat(item.Path); !os.IsNotExist(err) {
		t.Fatalf("repair created the old folder: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(moved, name)); err != nil || string(contents) != "video" {
		t.Fatalf("repair altered media: %q, %v", contents, err)
	}
	// A repaired folder must not reappear as an unclaimed adoption candidate,
	// including on hosts whose temporary directory is reached via a symlink.
	report, err = svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range report.UnmatchedDirs {
		if candidate.Path == moved {
			t.Fatal("repaired folder was offered for adoption again")
		}
	}
}

func TestFolderProblemReasons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "folder")
	if folderProblem(path) != "missing" {
		t.Fatal("absent path was not missing")
	}
	if err := os.WriteFile(path, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if folderProblem(path) != "not_directory" {
		t.Fatal("plain file was not distinguished from a directory")
	}
	unreadable := t.TempDir()
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	if os.Geteuid() != 0 && folderProblem(unreadable) != "inaccessible" {
		t.Fatal("unreadable folder was not distinguished from missing")
	}
}
