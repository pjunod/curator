package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Per-file cleanup of a publication (pipeline contract v2, section 3.4;
// plan M4.1). This is the one way Curator deletes bytes from a Runner
// publication: after an import (on the cleanup lane), from the hourly sweep,
// and when a person discards a file in Resolve or Storage. Every file of a
// finished publication has a final outcome, and none waits for a person:
//
//   - placed: the file leaves once its library copy is proven — the same
//     inode, or the placement journal's recorded size and inode generation
//     (a library file with no such record is hashed once). A library that
//     no longer holds it because something newer replaced or removed it
//     makes it discarded{superseded};
//   - discarded (superseded, duplicate, rejected): it leaves (a job's
//     directory is a closed scope);
//   - pending (left by an earlier build): decided first
//     (redecideLeftoverSidecars).
//
// Before each unlink the publication generation (the manifest in the
// directory), an unchanged inode generation since the file was checked and
// the storage identity of the completed root are checked again; a root whose
// identity changed is an unmounted mount point and nothing under it is
// deleted. A library file whose record remains but whose bytes are missing or
// differ keeps the publication copy (it may be the only good one) and is
// reported on the publication; the next pass looks again. The directory goes
// when nothing is left in it but the manifest.

// publicationSweepBatch bounds one hourly pass.
const publicationSweepBatch = 50

// publicationCleanupResult says what one pass did, for logs and tests.
type publicationCleanupResult struct {
	Removed          int      // files unlinked by this pass
	Kept             []string // "<path>: <why>" for files whose evidence did not hold
	DirectoryRemoved bool
	Skipped          string // why the pass did not run at all
}

// cleanupPublication runs the per-file cleanup of one publication, holding
// its download row's lock (the importer holds the same lock while it places).
func (s *Service) cleanupPublication(ctx context.Context, publicationID int64) (publicationCleanupResult, error) {
	pub, err := s.db.GetPublication(ctx, publicationID)
	if err != nil {
		return publicationCleanupResult{}, err
	}
	if pub.DownloadID != 0 {
		unlock := s.lockDownload(pub.DownloadID)
		defer unlock()
	}
	return s.cleanupPublicationLocked(ctx, publicationID)
}

// afterPublicationImported starts the cleanup an import has just made due.
// The caller holds the download lock, so the inline fallback runs here.
func (s *Service) afterPublicationImported(ctx context.Context, publicationID int64) {
	if s.queuePublicationCleanup(publicationID, "after its import") {
		return
	}
	ctx = withCleanupReason(context.WithoutCancel(ctx), "after its import")
	if _, err := s.cleanupPublicationLocked(ctx, publicationID); err != nil {
		s.log.Info("publication cleanup incomplete; the hourly sweep retries", "publication", publicationID, "err", err)
	}
}

// sweepPublications is the hourly pass: the same rule, for publications
// whose import finished and that still have evidenced files on disk (or an
// empty directory left behind). Each pass goes onto the cleanup lane, where
// it is tracked (System → In flight, the publication in Activity) and an
// operator can pause it.
func (s *Service) sweepPublications(ctx context.Context) error {
	if !perFileCleanupSupported {
		unsupportedCleanupNotice.Do(func() {
			s.log.Warn("per-file publication cleanup is unavailable on this platform (no inode identity to bind a verified file to its unlink); publications are kept")
		})
		return nil
	}
	pubs, err := s.db.PublicationsToSweep(ctx, publicationSweepBatch)
	if err != nil {
		return err
	}
	removed, dirs, queued := 0, 0, 0
	for _, pub := range pubs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.queuePublicationCleanup(pub.ID, "hourly sweep") {
			queued++
			continue
		}
		res, err := s.cleanupPublication(withCleanupReason(ctx, "hourly sweep"), pub.ID)
		if err != nil {
			s.log.Info("publication cleanup stopped", "publication", pub.ID, "path", pub.Path, "err", err)
		}
		removed += res.Removed
		if res.DirectoryRemoved {
			dirs++
		}
	}
	if removed > 0 || dirs > 0 || queued > 0 {
		s.log.Info("cleanup: swept publications", "queued", queued, "files_removed", removed, "directories_removed", dirs, "of", len(pubs))
	}
	return nil
}

var unsupportedCleanupNotice sync.Once

// errPerFileCleanupUnsupported stops a pass where a verified file cannot be
// bound to its unlink (no inode generation on this platform).
var errPerFileCleanupUnsupported = errors.New("per-file cleanup is unavailable on this platform (no inode identity); nothing deleted")

// cleanupReasonKey says why a pass runs (shown while it is in flight), and
// operatorCleanupKey that a person asked for it: an operator's explicit
// action passes the storage-change review fence and a pause; automatic
// passes do not.
type cleanupReasonKey struct{}
type operatorCleanupKey struct{}

func withCleanupReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, cleanupReasonKey{}, reason)
}

func withOperatorCleanup(ctx context.Context, reason string) context.Context {
	return context.WithValue(withCleanupReason(withForegroundVerification(ctx), reason), operatorCleanupKey{}, true)
}

func cleanupReason(ctx context.Context) string {
	if r, ok := ctx.Value(cleanupReasonKey{}).(string); ok && r != "" {
		return r
	}
	return "cleanup"
}

func operatorCleanup(ctx context.Context) bool {
	v, _ := ctx.Value(operatorCleanupKey{}).(bool)
	return v
}

// cleanupGate is checked at the start of a pass, before every unlink and
// before the rmdir: the root's live storage identity against the recorded
// one, then the storage-change review fences — an accepted replacement
// volume records the identity it now has, so the first check passes there,
// and the fence is what keeps automatic deletion off what was on it.
func (s *Service) cleanupGate(ctx context.Context, pub sqlite.Publication) error {
	if !perFileCleanupSupported {
		return errPerFileCleanupUnsupported
	}
	if err := s.checkStorageIdentity(ctx, pub.Path); err != nil {
		return fmt.Errorf("storage identity check failed; nothing deleted: %w", err)
	}
	held, err := s.publicationStorageReview(ctx, pub)
	if err != nil {
		return fmt.Errorf("storage review lookup failed; nothing deleted: %w", err)
	}
	if held && !operatorCleanup(ctx) {
		return fmt.Errorf("the storage under this publication was replaced and it is held for review; nothing is deleted automatically (discarding its files in Resolve or Storage is the review)")
	}
	return nil
}

// publicationStorageReview reports a storage-change fence on the
// publication: its row's generation fenced in the changed root, or an orphan
// path fence around (or inside) its directory.
func (s *Service) publicationStorageReview(ctx context.Context, pub sqlite.Publication) (bool, error) {
	if pub.DownloadID != 0 {
		dl, err := s.db.GetDownload(ctx, pub.DownloadID)
		if err != nil {
			return true, err
		}
		return s.db.StorageChangeReview(ctx, dl.GenerationKey, pub.Path)
	}
	return s.db.StorageReviewPath(ctx, pub.Path)
}

// publicationCleanupRun is one queued or running pass, so it is visible and
// can be stopped.
type publicationCleanupRun struct {
	state  string // queued | running
	reason string
	cancel context.CancelFunc
}

// PublicationCleanupState reports a queued or running pass for Activity.
func (s *Service) PublicationCleanupState(publicationID int64) (state, reason string) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if r := s.publicationCleanups[publicationID]; r != nil {
		return r.state, r.reason
	}
	return "", ""
}

// beginPublicationCleanup registers a running pass: in the in-flight
// registry (what it is and why it runs) and in the run table (so a pause
// cancels it). The returned context ends with the pass.
func (s *Service) beginPublicationCleanup(ctx context.Context, pub sqlite.Publication) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	reason := cleanupReason(ctx)
	s.importMu.Lock()
	if s.publicationCleanups == nil {
		s.publicationCleanups = map[int64]*publicationCleanupRun{}
	}
	if queued := s.publicationCleanups[pub.ID]; queued != nil && queued.cancel != nil {
		// The queued job that started this pass: a pause must reach both.
		outer := queued.cancel
		inner := cancel
		cancel = func() { inner(); outer() }
	}
	run := &publicationCleanupRun{state: "running", reason: reason, cancel: cancel}
	s.publicationCleanups[pub.ID] = run
	s.importMu.Unlock()
	key := pub.DownloadID
	if key == 0 {
		key = -pub.ID // a download-less publication still needs its own row
	}
	h := s.registry.Begin(transfers.Transfer{DownloadID: key, Title: filepath.Base(pub.Path), Stage: transfers.StagePublicationCleanup,
		Detail: "verifying and removing evidenced files (" + reason + ")"})
	return ctx, func() {
		h.End()
		cancel()
		s.importMu.Lock()
		if s.publicationCleanups[pub.ID] == run {
			delete(s.publicationCleanups, pub.ID)
		}
		s.importMu.Unlock()
	}
}

// PausePublicationCleanup stops a publication's automatic cleanup until it
// is resumed: a running or queued pass is cancelled (files already removed
// stay removed; nothing half-done is left, each unlink is one step), and the
// sweep skips it. Resume clears the pause and queues a pass now.
func (s *Service) PausePublicationCleanup(ctx context.Context, publicationID int64, paused bool) error {
	if err := s.db.SetPublicationCleanupPaused(ctx, publicationID, paused); err != nil {
		return err
	}
	if paused {
		s.importMu.Lock()
		if r := s.publicationCleanups[publicationID]; r != nil && r.cancel != nil {
			r.cancel()
		}
		s.importMu.Unlock()
		_ = s.db.AddHistory(ctx, "publication_cleanup_paused", 0, fmt.Sprintf("publication %d", publicationID), map[string]any{"publication": publicationID})
		return nil
	}
	_ = s.db.AddHistory(ctx, "publication_cleanup_resumed", 0, fmt.Sprintf("publication %d", publicationID), map[string]any{"publication": publicationID})
	if !s.queuePublicationCleanup(publicationID, "resumed by an operator") {
		if _, err := s.cleanupPublication(withOperatorCleanup(ctx, "resumed by an operator"), publicationID); err != nil {
			return err
		}
	}
	return nil
}

// cleanupPublicationLocked is the implementation; the caller holds the
// download lock (or the publication has no row).
func (s *Service) cleanupPublicationLocked(ctx context.Context, publicationID int64) (publicationCleanupResult, error) {
	var res publicationCleanupResult
	pub, err := s.db.GetPublication(ctx, publicationID)
	if err != nil {
		return res, err
	}
	switch pub.State {
	case "removed":
		res.Skipped = "already removed"
		return res, nil
	case "discovered", "importing":
		res.Skipped = "the import owns this publication"
		return res, nil
	}
	if pub.Cohort == "historical" && pub.CleanupApprovedAt.IsZero() {
		res.Skipped = ErrHistoricalCohort.Error()
		return res, nil
	}
	if !pub.CleanupPausedAt.IsZero() && !operatorCleanup(ctx) {
		res.Skipped = "cleanup is paused by an operator"
		return res, nil
	}
	root := filepath.Clean(pub.Path)
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return res, s.stopCleanup(ctx, pub, fmt.Errorf("publication path %q is not an absolute directory", pub.Path))
	}
	ctx, end := s.beginPublicationCleanup(ctx, pub)
	defer end()
	// Storage first: an unmounted share is an empty directory, not a payload
	// that is gone, and nothing below a root whose identity changed (or that
	// an accepted storage change fenced) is read.
	if err := s.cleanupGate(ctx, pub); err != nil {
		return res, s.stopCleanup(ctx, pub, err)
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return res, s.finishRemovedPublication(ctx, pub, 0, 0, "the publication directory was already gone")
	}
	if err != nil {
		return res, s.stopCleanup(ctx, pub, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return res, s.stopCleanup(ctx, pub, fmt.Errorf("publication path is not a plain directory"))
	}
	// Sidecars an earlier build left undecided are decided now (subtitles
	// placed beside the publication's video, the rest discarded), so they
	// leave with the rest instead of keeping the directory.
	if err := s.redecideLeftoverSidecars(ctx, pub); err != nil {
		return res, s.stopCleanup(ctx, pub, fmt.Errorf("leftover sidecars not decided yet: %w", err))
	}
	files, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return res, err
	}
	manifest, err := s.cleanupManifest(pub, allFilesRemoved(files))
	if err != nil {
		return res, s.stopCleanup(ctx, pub, err)
	}
	var item int64
	if pub.DownloadID != 0 {
		if dl, err := s.db.GetDownload(ctx, pub.DownloadID); err == nil {
			item = dl.MediaItemID
		}
	}
	var bytes int64
	for _, f := range files {
		if !f.RemovedAt.IsZero() || (f.Outcome != "placed" && f.Outcome != "discarded") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		err := s.cleanupPublicationFile(ctx, pub, manifest, f, item)
		switch {
		case err != nil:
			// Transient (unreadable, changed while checked, storage moved):
			// kept as it is, tried again by the next pass.
			res.Kept = append(res.Kept, f.Path+": "+err.Error())
		default:
			res.Removed++
			bytes += f.Bytes
		}
	}
	if len(res.Kept) > 0 {
		summary := fmt.Sprintf("cleanup kept %d file(s): %s", len(res.Kept), strings.Join(firstN(res.Kept, 3), "; "))
		_ = s.db.SetPublicationError(ctx, pub.ID, summary)
		s.log.Info("publication cleanup kept files", "publication", pub.ID, "kept", res.Kept)
	}
	files, err = s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return res, err
	}
	if !allFilesRemoved(files) {
		// An unresolved, pending or kept file is still here: so is the
		// directory.
		if res.Removed > 0 {
			s.recordPublicationFilesRemoved(ctx, pub, res.Removed, bytes)
		}
		if len(res.Kept) == 0 {
			// Nothing stopped this pass: an earlier stop no longer applies.
			_ = s.db.ClearPublicationCleanupError(ctx, pub.ID)
		}
		return res, nil
	}
	removedDir, err := s.removeEmptyPublicationDirectory(ctx, pub, item)
	if err != nil {
		if res.Removed > 0 {
			s.recordPublicationFilesRemoved(ctx, pub, res.Removed, bytes)
		}
		return res, s.stopCleanup(ctx, pub, err)
	}
	res.DirectoryRemoved = removedDir
	return res, s.finishRemovedPublication(ctx, pub, res.Removed, bytes, "every file resolved and removed")
}

// allFilesRemoved reports that every manifest file of the publication has
// left the directory (vacuously true for a publication with none).
func allFilesRemoved(files []sqlite.PublicationFile) bool {
	for _, f := range files {
		if f.RemovedAt.IsZero() {
			return false
		}
	}
	return true
}

// cleanupManifest re-reads what the directory says it is now and requires the
// publication's generation. A directory whose manifest names another
// generation was replaced; nothing in it is ours to delete. Once every file
// has been removed the manifest may be gone too (an earlier pass removed it
// and the rmdir failed on a stray): the stored manifest is then enough, as
// only the directory is left to remove.
func (s *Service) cleanupManifest(pub sqlite.Publication, allRemoved bool) (map[string]ports.ManifestFile, error) {
	var m ports.Manifest
	raw, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s is unreadable: %w", ports.ManifestFileName, err)
		}
		if pub.Generation != "" && m.Generation != pub.Generation {
			return nil, fmt.Errorf("the directory's manifest names generation %q, not this publication's %q; nothing deleted", m.Generation, pub.Generation)
		}
	case errors.Is(err, os.ErrNotExist):
		if pub.ManifestSource == "file" && !allRemoved {
			return nil, fmt.Errorf("the directory's %s is missing; nothing deleted", ports.ManifestFileName)
		}
		// A v1 delivery record (or no manifest at all, for a directory the
		// cutover froze): the stored manifest is the evidence.
		if pub.ManifestJSON != "" {
			_ = json.Unmarshal([]byte(pub.ManifestJSON), &m)
		}
		if pub.Generation != "" && m.Generation != "" && m.Generation != pub.Generation {
			return nil, fmt.Errorf("the stored manifest names generation %q, not %q; nothing deleted", m.Generation, pub.Generation)
		}
	default:
		return nil, err
	}
	out := make(map[string]ports.ManifestFile, len(m.Files))
	for _, f := range m.Files {
		out[f.Path] = f
	}
	return out, nil
}

// cleanupPublicationFile decides one file from its recorded outcome and
// unlinks it. It never leaves a file for a person: a discarded file leaves
// (the job's directory is a closed scope); a placed file leaves once its
// library copy is proven, and becomes discarded{superseded} when the library
// no longer holds it because something newer replaced or removed it. err is
// transient (storage, a read error, a file changing while checked, a library
// file whose record remains but whose bytes are missing or differ): the file
// stays as it is and the next pass tries again.
func (s *Service) cleanupPublicationFile(ctx context.Context, pub sqlite.Publication, manifest map[string]ports.ManifestFile, f sqlite.PublicationFile, item int64) error {
	if _, ok := manifest[f.Path]; !ok || !safeDeliveryRelativePath(f.Path) {
		return fmt.Errorf("the publication's manifest does not list this file")
	}
	base, err := os.OpenRoot(pub.Path)
	if err != nil {
		return err
	}
	defer func() { _ = base.Close() }()
	rel := filepath.FromSlash(f.Path)
	parent, file, named, err := openDeliveryFileNoSymlinks(base, rel)
	if errors.Is(err, os.ErrNotExist) {
		// The storage under the root is verified and the file is not there:
		// whoever removed it, it is gone.
		return s.db.MarkPublicationFileRemoved(ctx, pub.ID, f.Path, f.Outcome)
	}
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	// The publication file is not read: it is leaving. An unchanged
	// generation up to the unlink binds this decision to this object.
	generation := deliveryFileGeneration(named)
	_ = file.Close()
	var libGeneration string
	if f.Outcome == "placed" {
		superseded, why, lib, err := s.placedLibraryState(ctx, f, named)
		if err != nil {
			return err
		}
		if superseded {
			f.Outcome, f.DiscardReason, f.Reason = "discarded", "superseded", why
			if err := s.db.SetPublicationFileOutcome(ctx, f); err != nil {
				return err
			}
		} else if lib != nil {
			libGeneration = deliveryFileGeneration(lib)
		}
	}
	// The destructive step: serialized with imports into the same item and
	// with storage decisions, every identity re-checked, then one unlink.
	ctx, releaseTarget := s.lockImportTarget(ctx, item)
	defer releaseTarget()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	publicationUnlinkHook(pub, f.Path)
	if err := s.cleanupGate(ctx, pub); err != nil {
		return err
	}
	if _, err := s.cleanupManifest(pub, false); err != nil {
		return err
	}
	name := filepath.Base(rel)
	now, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if now.Mode()&os.ModeSymlink != 0 || !now.Mode().IsRegular() || deliveryFileGeneration(now) != generation || generation == "" {
		return fmt.Errorf("the file changed after it was verified")
	}
	if libGeneration != "" {
		lib, err := os.Lstat(f.LibraryPath)
		if err != nil || deliveryFileGeneration(lib) != libGeneration {
			return fmt.Errorf("the library file changed after it was verified")
		}
	}
	if err := parent.Remove(name); err != nil {
		return err
	}
	_ = syncRootDirectory(parent, ".")
	// The download lock held by every caller keeps Resolve from changing the
	// outcome between the verification and this record.
	return s.db.MarkPublicationFileRemoved(ctx, pub.ID, f.Path, f.Outcome)
}

// placedLibraryState decides a placed file's library copy. It holds (lib is
// returned, unless it is the same inode as the publication file): the
// publication copy may leave. The library no longer holds it because
// something newer replaced or removed it — no library row at that path, a
// newer placement of different bytes there, or a subtitle that is no longer
// the one Curator placed — superseded: the publication copy is garbage. A
// library file whose record remains but whose bytes are missing or differ is
// an error: the publication copy may be the only good one, so it stays and
// the next pass looks again.
func (s *Service) placedLibraryState(ctx context.Context, f sqlite.PublicationFile, named os.FileInfo) (superseded bool, why string, lib os.FileInfo, err error) {
	if f.LibraryPath == "" || !filepath.IsAbs(f.LibraryPath) {
		return true, "the placement recorded no library path", nil, nil
	}
	lib, err = os.Lstat(f.LibraryPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "", nil, err
	}
	if err == nil && os.SameFile(lib, named) {
		return false, "", nil, nil
	}
	if err == nil {
		holds := false
		if f.SHA256 == "" {
			// A historical row whose manifest carried no digest: its review
			// was approved, and the library file of the recorded size is it.
			holds = lib.Mode().IsRegular() && lib.Size() == f.Bytes
		} else if f.PlacedSHA256 == f.SHA256 {
			if holds, err = s.libraryCopyHolds(ctx, f, lib); err != nil {
				return false, "", nil, err
			}
		}
		if holds {
			return false, "", lib, nil
		}
	}
	if isSubtitleFile(f.LibraryPath) {
		return true, "the subtitle Curator placed at " + f.LibraryPath + " was replaced or removed", nil, nil
	}
	var rows int
	if qerr := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, f.LibraryPath).Scan(&rows); qerr != nil {
		return false, "", nil, qerr
	}
	if rows == 0 {
		return true, "the library no longer holds " + f.LibraryPath + " (replaced or removed)", nil, nil
	}
	if p, ok, perr := s.db.CommittedPlacementForTarget(ctx, f.LibraryPath, ""); perr == nil && ok && p.SHA256 != f.SHA256 {
		return true, "a newer placement replaced " + f.LibraryPath, nil, nil
	}
	return false, "", nil, fmt.Errorf("the library file at %s is missing or differs from what was placed; the publication copy is kept until it is back", f.LibraryPath)
}

// libraryCopyHolds decides that the library file a publication file was
// placed as still holds the manifest's bytes. The placement journal recorded
// the object it published (digest, size, inode generation); a library file
// that is still that object is proven by stat. Only a library file with no
// such record (a historical or v1 import, a subtitle) or whose generation
// moved is hashed, once.
func (s *Service) libraryCopyHolds(ctx context.Context, f sqlite.PublicationFile, lib os.FileInfo) (bool, error) {
	if !lib.Mode().IsRegular() || lib.Mode()&os.ModeSymlink != 0 || lib.Size() != f.Bytes {
		return false, nil
	}
	p, ok, err := s.db.CommittedPlacementForTarget(ctx, f.LibraryPath, f.SHA256)
	if err != nil {
		return false, err
	}
	if ok && p.Size == f.Bytes && sameContentGeneration(p.PublishedFileGeneration, lib) {
		return true, nil
	}
	digest, size, generation, err := fileDigestSnapshot(ctx, f.LibraryPath)
	if err != nil {
		return false, err
	}
	return size == f.Bytes && digest == f.SHA256 && generation == deliveryFileGeneration(lib), nil
}

// removeEmptyPublicationDirectory removes the publication's empty
// subdirectories, then its manifest and the directory itself, when nothing
// else is left in it. Anything Curator cannot account for keeps it.
func (s *Service) removeEmptyPublicationDirectory(ctx context.Context, pub sqlite.Publication, item int64) (bool, error) {
	base, err := os.OpenRoot(pub.Path)
	if err != nil {
		return false, err
	}
	defer func() { _ = base.Close() }()
	var others []string
	var walk func(dir string) (bool, error)
	walk = func(dir string) (bool, error) {
		f, err := base.Open(dir)
		if err != nil {
			return false, err
		}
		entries, err := f.ReadDir(-1)
		_ = f.Close()
		if err != nil {
			return false, err
		}
		empty := true
		for _, e := range entries {
			rel := filepath.Join(dir, e.Name())
			if dir == "." && e.Name() == ports.ManifestFileName && e.Type().IsRegular() {
				continue
			}
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				sub, err := walk(rel)
				if err != nil {
					return false, err
				}
				if sub {
					if err := base.Remove(rel); err != nil {
						return false, err
					}
					continue
				}
			} else {
				others = append(others, rel)
			}
			empty = false
		}
		return empty, nil
	}
	empty, err := walk(".")
	if err != nil {
		return false, err
	}
	if !empty {
		return false, fmt.Errorf("the directory holds %d entr(ies) no manifest file accounts for (%s); kept", len(others), strings.Join(firstN(others, 3), ", "))
	}
	ctx, releaseTarget := s.lockImportTarget(ctx, item)
	defer releaseTarget()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	if err := s.cleanupGate(ctx, pub); err != nil {
		return false, err
	}
	if _, err := s.cleanupManifest(pub, true); err != nil {
		return false, err
	}
	if err := base.Remove(ports.ManifestFileName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// rmdir, never a recursive delete: a file that appeared since the walk
	// keeps the directory.
	if err := os.Remove(pub.Path); err != nil {
		return false, err
	}
	if parent, err := os.OpenRoot(filepath.Dir(pub.Path)); err == nil {
		_ = syncRootDirectory(parent, ".")
		_ = parent.Close()
	}
	return true, nil
}

// publicationUnlinkHook runs inside the destructive step, before its
// re-checks; tests use it to change the world between hash and unlink.
var publicationUnlinkHook = func(sqlite.Publication, string) {}

// stopCleanup records why a pass deleted nothing more and returns the error.
func (s *Service) stopCleanup(ctx context.Context, pub sqlite.Publication, err error) error {
	_ = s.db.SetPublicationError(ctx, pub.ID, "cleanup stopped: "+err.Error())
	return err
}

func (s *Service) recordPublicationFilesRemoved(ctx context.Context, pub sqlite.Publication, files int, bytes int64) {
	mediaItemID, title := s.publicationOwner(ctx, pub)
	_ = s.db.AddHistory(ctx, HistoryPayloadRemoved, mediaItemID, title,
		map[string]any{"publication": pub.Path, "files": files, "bytes": bytes, "by": "per-file cleanup"})
}

// finishRemovedPublication marks a publication removed once its directory is
// gone, with one history row for what this pass removed.
func (s *Service) finishRemovedPublication(ctx context.Context, pub sqlite.Publication, files int, bytes int64, why string) error {
	if err := s.db.MarkPublicationRemoved(ctx, pub.ID); err != nil {
		return err
	}
	mediaItemID, title := s.publicationOwner(ctx, pub)
	_ = s.db.AddHistory(ctx, HistoryPayloadRemoved, mediaItemID, title,
		map[string]any{"publication": pub.Path, "files": files, "bytes": bytes, "by": "per-file cleanup", "directory": "removed", "why": why})
	s.log.Info("cleanup: publication removed", "publication", pub.ID, "path", pub.Path, "files", files, "bytes", bytes, "why", why)
	return nil
}

func (s *Service) publicationOwner(ctx context.Context, pub sqlite.Publication) (int64, string) {
	if pub.DownloadID != 0 {
		if dl, err := s.db.GetDownload(ctx, pub.DownloadID); err == nil {
			return dl.MediaItemID, dl.ReleaseTitle
		}
	}
	return 0, filepath.Base(pub.Path)
}

func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// ApprovePublicationCleanup is the M7 approval of a review batch: the named
// historical publications become subject to the per-file rule, and each is
// cleaned up now. Live publications need no approval and are not changed.
func (s *Service) ApprovePublicationCleanup(ctx context.Context, ids []int64) (int64, error) {
	n, err := s.db.ApprovePublicationCleanup(ctx, ids)
	if err != nil {
		return 0, err
	}
	_ = s.db.AddHistory(ctx, "publication_cleanup_approved", 0, fmt.Sprintf("%d publication(s)", n), map[string]any{"ids": ids, "approved": n})
	for _, id := range ids {
		if !s.queuePublicationCleanup(id, "review batch approved") {
			if _, err := s.cleanupPublication(withCleanupReason(ctx, "review batch approved"), id); err != nil {
				s.log.Info("approved publication cleanup incomplete; the hourly sweep retries", "publication", id, "err", err)
			}
		}
	}
	return n, nil
}

// DiscardPublicationFiles is the operator's "delete this leftover" on a
// publication-managed directory in Storage: each selected file must be an
// unresolved (or never imported) file of the publication claiming the
// directory; it becomes discarded{rejected} and leaves through the same
// per-file cleanup, with the same checks. A historical publication waits for
// its review batch, as automatic deletion does.
func (s *Service) DiscardPublicationFiles(ctx context.Context, dir string, selections []CompletedFileSelection) error {
	ctx = withOperatorCleanup(ctx, "operator discard in Storage")
	if len(selections) == 0 {
		return fmt.Errorf("select one or more files to delete")
	}
	pub, ok, err := s.db.PublicationByPath(ctx, filepath.Clean(dir))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no publication claims %s", dir)
	}
	if pub.Cohort == "historical" && pub.CleanupApprovedAt.IsZero() {
		return ErrHistoricalCohort
	}
	if pub.DownloadID != 0 {
		unlock := s.lockDownload(pub.DownloadID)
		defer unlock()
	}
	files, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	byPath := map[string]sqlite.PublicationFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	var chosen []sqlite.PublicationFile
	for _, sel := range selections {
		rel, err := filepath.Rel(pub.Path, filepath.Clean(sel.Path))
		if err != nil || !filepath.IsLocal(rel) {
			return fmt.Errorf("%s is not inside the publication", sel.Path)
		}
		f, ok := byPath[filepath.ToSlash(rel)]
		if !ok {
			return fmt.Errorf("%s is not a manifest file of this publication; nothing deleted", rel)
		}
		if f.Outcome != "unresolved" && f.Outcome != "pending" && f.Outcome != "discarded" {
			return fmt.Errorf("%s is %s; only unresolved files can be discarded", rel, f.Outcome)
		}
		chosen = append(chosen, f)
	}
	for _, f := range chosen {
		if f.Outcome == "discarded" {
			continue
		}
		f.Outcome, f.DiscardReason, f.Reason = "discarded", "rejected", "discarded by an operator in Storage"
		if err := s.db.SetPublicationFileOutcome(ctx, f); err != nil {
			return err
		}
	}
	mediaItemID, title := s.publicationOwner(ctx, pub)
	_ = s.db.AddHistory(ctx, "publication_file_resolved", mediaItemID, title,
		map[string]any{"publication": pub.Path, "files": len(chosen), "action": "discard"})
	res, err := s.cleanupPublicationLocked(ctx, pub.ID)
	if err != nil {
		return err
	}
	if len(res.Kept) > 0 {
		return fmt.Errorf("discarded, but kept: %s", strings.Join(res.Kept, "; "))
	}
	if res.Skipped != "" {
		return fmt.Errorf("discarded; deletion waits: %s", res.Skipped)
	}
	return s.ScanCompleted(ctx)
}
