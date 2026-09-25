package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// Two different films share a title and a year — there are two called
// Leviticus from 2022. Both belong in the library, and each needs a folder
// of its own, or the scan links one film's files to whichever it saw last.
func TestSameTitleAndYearGetSeparateFolders(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1001, "Leviticus", 2022), movie(1002, "Leviticus", 2022),
	}}
	ctx := context.Background()
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 1001, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 1002, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Leviticus (2022)"); first.Path != want {
		t.Errorf("first path = %q, want the plain naming-rule folder %q", first.Path, want)
	}
	if want := filepath.Join(root, "Leviticus (2022) {tmdb-1002}"); second.Path != want {
		t.Errorf("second path = %q, want %q", second.Path, want)
	}

	// The placement suggestion for the second item must not steer the
	// repair form back onto the first item's folder.
	sug, err := svc.SuggestPlacement(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sug.Path != second.Path {
		t.Errorf("suggestion = %q, want the item's own folder %q", sug.Path, second.Path)
	}
	// And the first item's own suggestion is still its plain folder: an item
	// does not collide with itself.
	if sug, err := svc.SuggestPlacement(ctx, first.ID); err != nil || sug.Path != first.Path {
		t.Errorf("first suggestion = %q, %v; want %q", sug.Path, err, first.Path)
	}

	// Moving the second item to the same root again (a root-folder edit)
	// keeps it off the first item's folder.
	rootID := rf.ID
	moved, err := svc.UpdateItem(ctx, second.ID, UpdateRequest{RootFolderID: &rootID})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Path == first.Path {
		t.Fatalf("root-folder edit moved the second film onto %s", first.Path)
	}
}

// A folder a person chooses by hand is refused, not silently shared.
func TestExplicitPathHeldByAnotherItemIsRefused(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1001, "Leviticus", 2022), movie(2002, "Other", 2020),
	}}
	ctx := context.Background()
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	held, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 1001, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 2002, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{held.Path, held.Path + "/"} {
		p := p
		_, err = svc.UpdateItem(ctx, other.ID, UpdateRequest{Path: &p})
		if !errors.Is(err, ErrFolderConflict) {
			t.Fatalf("path %q: err = %v, want ErrFolderConflict", p, err)
		}
		if !strings.Contains(err.Error(), "Leviticus") {
			t.Errorf("error should name the holder: %v", err)
		}
	}
	// Re-saving an item's own folder is not a conflict.
	own := held.Path
	if _, err := svc.UpdateItem(ctx, held.ID, UpdateRequest{Path: &own}); err != nil {
		t.Fatalf("re-saving own folder: %v", err)
	}
	// Nor is clearing a folder.
	empty := ""
	if _, err := svc.UpdateItem(ctx, other.ID, UpdateRequest{Path: &empty}); err != nil {
		t.Fatalf("clearing folder: %v", err)
	}
}

// A manual entry cannot be created on a folder a copy already holds.
func TestManualEntryRefusesACopyFolder(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Copy Folder")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_copies
		(media_item_id, name, quality_profile_id, monitored, root_folder_id, path, added_at)
		VALUES (?, 'copy', ?, 1, ?, ?, 0)`, item.ID, item.QualityProfileID, rf.ID, dir); err != nil {
		t.Fatal(err)
	}
	_, err = svc.AddManual(ctx, ManualRequest{Kind: domain.KindMovie, Title: "Home Video", Path: dir})
	if !errors.Is(err, ErrFolderConflict) {
		t.Fatalf("err = %v, want ErrFolderConflict", err)
	}
}

// A folder Monarr disambiguated names its own identity; adoption must use
// it rather than asking which of two identical titles it is.
func TestAdoptionUsesTheFolderIDHint(t *testing.T) {
	prov := adoptProvider{movies: []ports.SearchResult{
		movie(1001, "Leviticus", 2022), movie(1002, "Leviticus", 2022),
	}}
	if got := propose(t, prov, domain.RootKindOf(domain.KindMovie), "Leviticus (2022)"); got.Confidence == ConfidenceExact {
		t.Fatalf("without a hint two identical titles must ask, got %s", got.Confidence)
	}
	for _, folder := range []string{"Leviticus (2022) {tmdb-1002}", "Leviticus (2022) [tmdbid-1002]"} {
		got := propose(t, prov, domain.RootKindOf(domain.KindMovie), folder)
		if got.Confidence != ConfidenceExact || got.Candidates[0].TMDBID != 1002 {
			t.Errorf("%s: confidence %s, first %+v; want exact on 1002", folder, got.Confidence, got.Candidates[0])
		}
		if got.ParsedTitle != "Leviticus" || got.ParsedYear != 2022 {
			t.Errorf("%s: parsed %q (%d); the hint must not leak into the title", folder, got.ParsedTitle, got.ParsedYear)
		}
	}
	// A hint naming a record the provider did not return changes nothing.
	if got := propose(t, prov, domain.RootKindOf(domain.KindMovie), "Leviticus (2022) {tmdb-9}"); got.Confidence == ConfidenceExact {
		t.Errorf("an unknown id must not manufacture confidence, got %s", got.Confidence)
	}
}

// A copy follows the same rule as the item: in a root where another item
// holds the plain name it takes the tagged one, rather than being refused
// with no way round it (a copy request carries no path).
func TestCopyInARootWhereThePlainNameIsHeld(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1001, "Leviticus", 2022), movie(1002, "Leviticus", 2022),
	}}
	ctx := context.Background()
	r1, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	r2root := t.TempDir()
	r2, err := svc.AddRootFolder(ctx, r2root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 1001, RootFolderID: r2.ID}); err != nil {
		t.Fatal(err)
	}
	b, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 1002, RootFolderID: r1.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.AddCopy(ctx, b.ID, CopyRequest{QualityProfileID: b.QualityProfileID, RootFolderID: r2.ID, Monitored: true})
	if err != nil {
		t.Fatalf("AddCopy: %v", err)
	}
	want := filepath.Join(r2root, "Leviticus (2022) {tmdb-1002}")
	if len(got.Copies) != 1 || got.Copies[0].Path != want {
		t.Fatalf("copies = %+v, want one at %q", got.Copies, want)
	}
	// In the item's own root the copy would land on the item's folder, and
	// that is still refused.
	if _, err := svc.AddCopy(ctx, b.ID, CopyRequest{QualityProfileID: b.QualityProfileID, RootFolderID: r1.ID, Monitored: true}); err == nil {
		t.Fatal("a copy onto the item's own folder must be refused")
	}
}

// Two same-named films added at the same moment (an import list and a
// person) both succeed, in different folders. The index settles the race;
// Add re-picks instead of failing a valid add.
func TestConcurrentSameNameAddsBothSucceed(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1001, "Leviticus", 2022), movie(1002, "Leviticus", 2022),
	}}
	ctx := context.Background()
	rf, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		items, err := svc.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if err := svc.Delete(ctx, it.ID); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		got := make([]domain.MediaItem, 2)
		errs := make([]error, 2)
		for j, id := range []int64{1001, 1002} {
			wg.Add(1)
			go func(j int, id int64) {
				defer wg.Done()
				got[j], errs[j] = svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: id, RootFolderID: rf.ID})
			}(j, id)
		}
		wg.Wait()
		for j, e := range errs {
			if e != nil {
				t.Fatalf("iteration %d: add %d failed: %v", i, j, e)
			}
		}
		if got[0].Path == got[1].Path {
			t.Fatalf("iteration %d: both films given %s", i, got[0].Path)
		}
	}
}

// Adoption compares folders the same way the rest of the rule does: a
// holder stored with a trailing slash still holds the folder, so adopting
// the second film there adds nothing — no stray item left behind.
func TestAdoptionSeesAnUncleanedHolder(t *testing.T) {
	svc, db, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1001, "Leviticus", 2022), movie(1002, "Leviticus", 2022),
	}}
	ctx := context.Background()
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 1001, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET path = ? WHERE id = ?`, a.Path+"/", a.ID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Leviticus (2022)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptOne(ctx, dir, movie(1002, "Leviticus", 2022), false); err != nil {
		t.Fatalf("AdoptOne: %v", err)
	}
	items, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("library has %d items, want only the holder", len(items))
	}
}

// When pointing a freshly added item at its folder fails, the item is
// removed again rather than left as a stray entry at its naming-rule path.
func TestFailedAdoptionLeavesNoStrayItem(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(1002, "Leviticus", 2022)}}
	ctx := context.Background()
	rf, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	// A relative path gets past the holder check and is refused by
	// UpdateItem — the point being that the follow-up write fails after Add
	// has already created the item.
	err = svc.adoptOne(ctx, Proposal{
		RootFolderID: rf.ID, Path: "not/absolute",
		Candidates: []ports.SearchResult{movie(1002, "Leviticus", 2022)},
	})
	if err == nil {
		t.Fatal("adoptOne onto a relative path should fail")
	}
	items, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("failed adoption left %d item(s) behind: %+v", len(items), items)
	}
}
