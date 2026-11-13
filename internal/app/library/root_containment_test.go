package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func TestAdoptionAndManualEntryRejectSymlinkOutsideRoot(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "adopt", true: "manual"}[manual], func(t *testing.T) {
			svc, db, _ := newService(t)
			ctx := context.Background()
			root := t.TempDir()
			outside := t.TempDir()
			if _, err := svc.AddRootFolder(ctx, root, domain.KindMixed); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "Escape (2020)")
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(outside, "keep.txt")
			if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if manual {
				_, err = svc.AddManual(ctx, ManualRequest{Kind: domain.KindMovie, Title: "Escape", Year: 2020, Path: path})
			} else {
				err = svc.AdoptOne(ctx, path, movie(550, "Fight Club", 1999), false)
			}
			if err == nil {
				t.Fatal("accepted a symlink to an unregistered outside directory")
			}
			items, e := db.ListMediaItems(ctx, "")
			if e != nil || len(items) != 0 {
				t.Fatalf("refused input changed library: %v %v", items, e)
			}
			if data, e := os.ReadFile(marker); e != nil || string(data) != "retained" {
				t.Fatalf("outside bytes changed: %q %v", data, e)
			}
		})
	}
}

func TestRootContainmentPreservesRootAliasesAndInternalLinks(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	if err := os.MkdirAll(filepath.Join(realRoot, "Nested", "Movie"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, alias, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(alias, "Internal")
	if err := os.Symlink(filepath.Join(realRoot, "Nested", "Movie"), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(alias, "Nested"), filepath.Join(realRoot, "Nested"), link} {
		got, e := svc.rootFolderFor(ctx, path)
		if e != nil || got.ID != rf.ID {
			t.Errorf("legitimate root alias/internal link %q rejected: %v", path, e)
		}
	}
	if _, err := svc.rootFolderFor(ctx, realRoot); err == nil {
		t.Fatal("root itself accepted as item folder")
	}
	sibling := realRoot + "-sibling"
	if err := os.Mkdir(sibling, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.rootFolderFor(ctx, sibling); err == nil {
		t.Fatal("prefix sibling accepted")
	}
}

func TestQueuedAdoptionRevalidatesRootContainment(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Queued")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{RootFolderID: rf.ID, Path: path, Confidence: ConfidenceExact, Candidates: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	result := svc.Adopt(ctx, []Proposal{proposal}, false)
	if len(result.Failures) != 1 || len(result.Adopted) != 0 {
		t.Fatalf("stale proposal adopted: %+v", result)
	}
	items, err := db.ListMediaItems(ctx, "")
	if err != nil || len(items) != 0 {
		t.Fatalf("library changed: %v %v", items, err)
	}
}
