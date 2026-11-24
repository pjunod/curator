package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestPresenceRejectsReplacementVolumeAndRequiresCurrentAcknowledgement(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	rootPath := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	root, err := svc.AddRootFolder(ctx, rootPath, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootPath, "Movie")
	if err = os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	movie, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Movie", RootFolderID: root.ID, Path: path, IDs: domain.ExternalIDs{TMDB: 42}})
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(path, "Movie.mkv")
	if err = os.WriteFile(media, []byte("media"), 0644); err != nil {
		t.Fatal(err)
	}
	file, err := db.UpsertFile(ctx, movie, 0, media, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyPresence(ctx, movie); err != nil {
		t.Fatal(err)
	}
	target := []domain.AvailabilityTarget{{Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 42}}}
	facts, err := db.LookupAvailability(ctx, target)
	if err != nil || facts[0].Presence != "present" {
		t.Fatalf("initial %+v %v", facts, err)
	}
	if err = os.Rename(rootPath, rootPath+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(rootPath, "keep-nonempty"), []byte("fallback"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyPresence(ctx, movie); err == nil {
		t.Fatal("replacement volume trusted")
	}
	facts, err = db.LookupAvailability(ctx, target)
	if err != nil || facts[0].Presence != "unknown" {
		t.Fatalf("replacement %+v %v", facts, err)
	}
	preview, err := svc.PreviewLibraryVolume(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ObservedIdentity == preview.State.ExpectedIdentity {
		t.Fatal("different volume has same identity")
	}
	if err = svc.ConfirmLibraryVolume(ctx, root.ID, preview.Fingerprint, false); err == nil {
		t.Fatal("confirmation without acknowledgement")
	}
	if err = os.Rename(rootPath, rootPath+"-fallback"); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err = svc.ConfirmLibraryVolume(ctx, root.ID, preview.Fingerprint, true); err == nil {
		t.Fatal("stale volume confirmed")
	}
	current, err := svc.PreviewLibraryVolume(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.ConfirmLibraryVolume(ctx, root.ID, current.Fingerprint, true); err != nil {
		t.Fatal(err)
	}
	// Explicit rebinding changes catalogue trust, never files or file records.
	var count int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=?`, file).Scan(&count); err != nil || count != 1 {
		t.Fatalf("record lost %d %v", count, err)
	}
	if _, err = os.Stat(filepath.Join(rootPath+"-original", "Movie", "Movie.mkv")); err != nil {
		t.Fatal("original media changed", err)
	}
}
func TestPresenceRejectsUnindexedManifestAndPreservesProofScope(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	rootPath := t.TempDir()
	root, err := svc.AddRootFolder(ctx, rootPath, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(rootPath, "Show")
	if err = os.Mkdir(scope, 0755); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Movie", RootFolderID: root.ID, Path: scope, IDs: domain.ExternalIDs{TMDB: 17}})
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(scope, "unknown.mkv")
	if err = os.WriteFile(media, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyPresence(ctx, id); err == nil {
		t.Fatal("unindexed media certified absent")
	}
	f, err := db.UpsertFile(ctx, id, 0, media, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyPresence(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(media); err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyPresence(ctx, id); err != nil {
		t.Fatal(err)
	}
	v, err := db.LookupAvailability(ctx, []domain.AvailabilityTarget{{LibraryItemID: id, Kind: domain.KindMovie}})
	if err != nil || v[0].Presence != "missing" {
		t.Fatalf("healthy negative %+v %v", v, err)
	}
	var count int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=?`, f).Scan(&count); err != nil || count != 1 {
		t.Fatal("verifier deleted records", err)
	}
}

func TestScanAndPresenceRejectSymlinkedScopeWithoutPruning(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	base := t.TempDir()
	root, err := svc.AddRootFolder(ctx, base, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(base, "Movie")
	if err = os.Mkdir(scope, 0755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(scope, "movie.mkv")
	if err = os.WriteFile(media, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Movie", RootFolderID: root.ID, Path: scope})
	if err != nil {
		t.Fatal(err)
	}
	file, err := db.UpsertFile(ctx, id, 0, media, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyPresence(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(scope, scope+"-actual"); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(scope+"-actual", scope); err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = svc.scanItem(ctx, item, nil); err == nil {
		t.Fatal("symlink reconciled")
	}
	if err = svc.VerifyPresence(ctx, id); err == nil {
		t.Fatal("symlink certified")
	}
	var count int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=?`, file).Scan(&count); err != nil || count != 1 {
		t.Fatal("symlink pruned records", err)
	}
	v, err := db.LookupAvailability(ctx, []domain.AvailabilityTarget{{Kind: domain.KindMovie, LibraryItemID: id}})
	if err != nil || v[0].Presence != "unknown" {
		t.Fatalf("%+v %v", v, err)
	}
}
