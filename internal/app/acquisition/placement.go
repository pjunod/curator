package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/probe"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type targetHeldKey struct{}
type recoveryPlacementKey struct{}
type expectedSourceDigestsKey struct{}
type reviewedSourceSnapshotsKey struct{}
type acceptedPublicationGenerationKey struct{}

// withAcceptedPublicationGeneration carries the Runner publication identity
// separately from per-file stat generations and the Curator download key.
func withAcceptedPublicationGeneration(ctx context.Context, generation string) context.Context {
	if strings.TrimSpace(generation) == "" {
		return ctx
	}
	return context.WithValue(ctx, acceptedPublicationGenerationKey{}, generation)
}

func acceptedPublicationGeneration(ctx context.Context) string {
	generation, _ := ctx.Value(acceptedPublicationGenerationKey{}).(string)
	return generation
}

// withExpectedSourceDigests binds accepted manifest content to the existing
// placement commit path. commitPlacement checks the digest it reads before it
// creates a placement journal or changes any library bytes.
func withExpectedSourceDigests(ctx context.Context, digests map[string]string) context.Context {
	canonical := make(map[string]string, len(digests))
	for path, digest := range digests {
		canonical[filepath.Clean(path)] = digest
	}
	return context.WithValue(ctx, expectedSourceDigestsKey{}, canonical)
}

func withReviewedSourceSnapshots(ctx context.Context, snapshots map[string]string) context.Context {
	canonical := make(map[string]string, len(snapshots))
	for path, identity := range snapshots {
		canonical[filepath.Clean(path)] = identity
	}
	return context.WithValue(ctx, reviewedSourceSnapshotsKey{}, canonical)
}

func validateReviewedSourceSnapshot(ctx context.Context, path string) error {
	snapshots, ok := ctx.Value(reviewedSourceSnapshotsKey{}).(map[string]string)
	if !ok {
		return nil
	}
	expected, ok := snapshots[filepath.Clean(path)]
	if !ok {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || completedReviewFileIdentity(path, info) != expected {
		return fmt.Errorf("source file changed after legacy review; review the file again: %s", path)
	}
	return nil
}

type recoveryPlacementContext struct {
	Destinations   map[string]string
	Progress       func(string, int64, int64)
	Validate       func() error
	ObserveFinal   func() (string, error)
	EpisodeTargets map[string]RecoveryEpisodeTarget
	ImportID       string
	Files          map[string]string
}

// rejectSymlinks checks every existing component, including the mount root.
// A missing final path is allowed only for a destination not yet published.
func rejectSymlinks(path string, missing bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("absolute path required: %s", path)
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp"} {
			if path == alias || strings.HasPrefix(path, alias+"/") {
				path = "/private" + path
				break
			}
		}
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, cur), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) && missing {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink requires review: %s", cur)
		}
	}
	return nil
}
func fileDigest(ctx context.Context, path string) (string, int64, error) {
	digest, size, _, err := fileDigestSnapshot(ctx, path)
	return digest, size, err
}

// Hash once (contract section 3.4, field data 2026-11-11). A Runner
// publication's manifest digest is the authority for its bytes: Curator does
// not read the source to learn it, and proves the copy against it in the copy
// stream itself. An import is then one read of the source and one write of
// the stage; a same-filesystem placement is a link and reads nothing. Every
// later decision (resume, reconciliation, cleanup) is made from the recorded
// identity of the file, not by reading it again.

// stageDigest is what the stage writer reports about the bytes it produced:
// the SHA-256 of the stream it wrote, or that the stage is a link to the
// source object (no bytes moved). It is scoped to one placeFile call.
type stageDigestKey struct{}
type stageDigest struct {
	sum    string
	linked bool
}

func withStageDigest(ctx context.Context) (context.Context, *stageDigest) {
	d := &stageDigest{}
	return context.WithValue(ctx, stageDigestKey{}, d), d
}

func recordStageDigest(ctx context.Context, sum string) {
	if d, ok := ctx.Value(stageDigestKey{}).(*stageDigest); ok {
		d.sum, d.linked = sum, false
	}
}

func recordStageLinked(ctx context.Context) {
	if d, ok := ctx.Value(stageDigestKey{}).(*stageDigest); ok {
		d.sum, d.linked = "", true
	}
}

// errPayloadMismatch is a publication file that is not what Runner's
// manifest says it is (its size, or its digest as copied). The publication
// cannot be imported; it ends and its want is grabbed again.
var errPayloadMismatch = errors.New("publication file does not match its manifest")

// errPlacementConflict is a library that moved under a placement between its
// preview and its publication (the target or the stage changed): the attempt
// failed on what it was placing, is counted, and is retried on the ladder.
var errPlacementConflict = errors.New("the library changed under this placement")

type expectedSourceSizesKey struct{}

// withExpectedSourceSizes carries the manifest's byte counts, checked by
// stat before anything is linked or copied.
func withExpectedSourceSizes(ctx context.Context, sizes map[string]int64) context.Context {
	canonical := make(map[string]int64, len(sizes))
	for path, n := range sizes {
		canonical[filepath.Clean(path)] = n
	}
	return context.WithValue(ctx, expectedSourceSizesKey{}, canonical)
}

// placementSourceDigest is the digest a placement records for its source:
// the accepted manifest's, without reading the file, when the import carries
// one; otherwise (another download client, a manual import) one read of the
// source, because the placement journal is keyed by content.
func placementSourceDigest(ctx context.Context, src string) (string, int64, error) {
	if expected, ok := ctx.Value(expectedSourceDigestsKey{}).(map[string]string); ok {
		if digest := expected[filepath.Clean(src)]; digest != "" {
			if err := rejectSymlinks(src, false); err != nil {
				return "", 0, err
			}
			info, err := os.Lstat(src)
			if err != nil {
				return "", 0, err
			}
			if !info.Mode().IsRegular() {
				return "", 0, fmt.Errorf("not a regular file: %s", src)
			}
			if sizes, ok := ctx.Value(expectedSourceSizesKey{}).(map[string]int64); ok {
				if want, known := sizes[filepath.Clean(src)]; known && want != info.Size() {
					return "", 0, fmt.Errorf("%w: %s is %d bytes, the manifest says %d", errPayloadMismatch, src, info.Size(), want)
				}
			}
			return digest, info.Size(), nil
		}
	}
	return fileDigest(ctx, src)
}

// crossVolumePublicationKey marks an import of a publication Runner copied
// across volumes: its bytes are verified once against the manifest. A copy
// does that in its stream; a same-filesystem link reads the file once.
type crossVolumePublicationKey struct{}

func withCrossVolumePublication(ctx context.Context) context.Context {
	return context.WithValue(ctx, crossVolumePublicationKey{}, true)
}

func crossVolumePublication(ctx context.Context) bool {
	v, _ := ctx.Value(crossVolumePublicationKey{}).(bool)
	return v
}

// manifestConstrained reports whether src's digest came from an accepted
// manifest (a mismatch is then the payload's, not the copy's).
func manifestConstrained(ctx context.Context, src string) bool {
	expected, ok := ctx.Value(expectedSourceDigestsKey{}).(map[string]string)
	return ok && expected[filepath.Clean(src)] != ""
}

// sameContentGeneration reports that a recorded file generation still names
// the same bytes: the same inode, size and mtime. ctime is not content — a
// link, a chmod or an NFS silly-rename moves it — so it is not compared
// (design principle 5: identity is digest + inode, size, mtime).
func sameContentGeneration(recorded string, info os.FileInfo) bool {
	if recorded == "" || info == nil {
		return false
	}
	current := deliveryFileGeneration(info)
	if current == "" {
		return false
	}
	return contentPart(recorded) == contentPart(current)
}

// contentPart drops the trailing ctime field of a generation
// ("ino:size:mtime_s:mtime_ns:ctime").
func contentPart(generation string) string {
	if i := strings.LastIndexByte(generation, ':'); i > 0 {
		return generation[:i]
	}
	return generation
}

// placementPublishedByIdentity reports that info is this placement's own
// published object, decided by the identity the journal recorded: the
// published generation, or the staged object (inode and the content
// generation recorded after the copy was proven; a rename moves only ctime).
// Legacy journals that recorded neither are not matched here.
func placementPublishedByIdentity(p sqlite.Placement, info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != p.Size {
		return false
	}
	if p.PublishedFileGeneration != "" {
		return sameContentGeneration(p.PublishedFileGeneration, info)
	}
	return p.TemporaryObjectIdentity != "" && p.TemporaryFileGeneration != "" &&
		deliveryFileStableIdentity(info) == p.TemporaryObjectIdentity && sameContentGeneration(p.TemporaryFileGeneration, info)
}

// committedPlacementIntact reports whether a committed placement's library
// file is still the object it published, by stat. A journal from before
// published generations were recorded is checked by its digest instead.
// A missing target is (false, nil).
func committedPlacementIntact(ctx context.Context, p sqlite.Placement) (bool, error) {
	if p.PublishedFileGeneration == "" {
		current, _, err := fileDigest(ctx, p.Target)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil && current == p.SHA256, err
	}
	if err := rejectSymlinks(p.Target, true); err != nil {
		return false, err
	}
	info, err := os.Lstat(p.Target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return placementPublishedByIdentity(p, info), nil
}

func fileDigestSnapshot(ctx context.Context, path string) (string, int64, string, error) {
	if err := rejectSymlinks(path, false); err != nil {
		return "", 0, "", err
	}
	// Concurrent digests of one unchanged file share a single read (see
	// verify_reads.go). The key is the generation this caller sees now; the
	// shared result must end at that same generation, as a private read would.
	pre, err := os.Lstat(path)
	if err != nil || !pre.Mode().IsRegular() || deliveryFileGeneration(pre) == "" {
		return readFileDigestSnapshot(ctx, path)
	}
	want := deliveryFileGeneration(pre)
	// Foreground and background readers share only among themselves: a
	// lock holder joining a background read would wait at background pace.
	class := "bg"
	if foregroundVerification(ctx) {
		class = "fg"
	}
	digest, n, generation, err := joinDigest(ctx, class+"\x00"+path+"\x00"+want, func() (string, int64, string, error) {
		return readFileDigestSnapshot(ctx, path)
	})
	if err != nil {
		return "", 0, "", err
	}
	if generation != want {
		return "", 0, "", fmt.Errorf("file changed during verification: %s", path)
	}
	return digest, n, generation, nil
}

func readFileDigestSnapshot(ctx context.Context, path string) (string, int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	if !before.Mode().IsRegular() {
		return "", 0, "", fmt.Errorf("not a regular file: %s", path)
	}
	beforeGeneration := deliveryFileGeneration(before)
	h := sha256.New()
	n, err := io.Copy(h, newVerifyReader(ctx, f))
	if err != nil {
		return "", 0, "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", 0, "", err
	}
	afterGeneration := deliveryFileGeneration(after)
	currentGeneration := deliveryFileGeneration(current)
	if !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		(beforeGeneration != "" && (beforeGeneration != afterGeneration || afterGeneration != currentGeneration)) || n != before.Size() {
		return "", 0, "", fmt.Errorf("file changed during verification: %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, currentGeneration, nil
}
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// manualPlacementDestination keeps a same-quality manual import beside existing
// bytes. The digest suffix is stable across retries and never authorizes overwrite.
func manualPlacementDestination(ctx context.Context, src, dest string, manual, replace bool) (string, error) {
	if !manual || replace {
		return dest, nil
	}
	incoming, _, err := fileDigest(ctx, src)
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for n := 0; n < 1000; n++ {
		current, _, err := fileDigest(ctx, dest)
		if errors.Is(err, os.ErrNotExist) || (err == nil && current == incoming) {
			return dest, nil
		}
		if err != nil {
			return "", err
		}
		dest = fmt.Sprintf("%s [manual-%s-%d]%s", base, incoming[:12], n, ext)
	}
	return "", fmt.Errorf("manual destination collision limit reached")
}

// replacementPlacementDestination keeps a selected source beside any current
// library path. Replacement cleanup later removes only the operation's exact
// observed generations; writing beside the target also protects shared or
// newer files that happen to render to the same name.
func replacementPlacementDestination(ctx context.Context, src, dest string, scope importScope) (string, error) {
	if scope.ReplacementOperationID == "" {
		return dest, nil
	}
	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		return dest, nil
	} else if err != nil {
		return "", err
	}
	if err := rejectSymlinks(dest, true); err != nil {
		return "", err
	}
	incoming, _, err := fileDigest(ctx, src)
	if err != nil {
		return "", err
	}
	shortOperation := scope.ReplacementOperationID
	if len(shortOperation) > 8 {
		shortOperation = shortOperation[:8]
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	base = fmt.Sprintf("%s [replacement-%s-%s]", base, shortOperation, incoming[:12])
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := base + ext
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d%s", base, attempt, ext)
		}
		if err := rejectSymlinks(filepath.Dir(candidate), false); err != nil {
			return "", err
		}
		if err := rejectSymlinks(candidate, true); err != nil {
			return "", err
		}
		current, _, statErr := fileDigest(ctx, candidate)
		if errors.Is(statErr, os.ErrNotExist) {
			return candidate, nil
		}
		if statErr != nil {
			return "", statErr
		}
		if current == incoming {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("replacement destination collision limit reached")
}

func (s *Service) commitPlacement(ctx context.Context, item domain.MediaItem, scope importScope, src, dest string, q quality.Quality, eps []int64, replace bool) (int64, error) {
	if err := validateReviewedSourceSnapshot(ctx, src); err != nil {
		return 0, err
	}
	hash, size, err := placementSourceDigest(ctx, src)
	if err != nil {
		return 0, err
	}
	if err := validateReviewedSourceSnapshot(ctx, src); err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%d:%d:%s:%s:%s", item.ID, scope.CopyID, dest, hash, scope.Attempt)
	recovery, _ := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext)
	if recovery != nil {
		key += "/" + recovery.ImportID
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	p, err := s.db.GetPlacement(ctx, id)
	// A committed ordinary attempt is idempotent while its library result
	// exists. A deliberate reimport after removal gets a new durable generation.
	for generation := 0; recovery == nil && err == nil && (p.State == "committed" || p.State == "cleaned"); generation++ {
		intact, e := committedPlacementIntact(ctx, p)
		var present int
		dbErr := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=? AND media_item_id=?`, p.Target, p.ItemID).Scan(&present)
		if dbErr != nil {
			return 0, dbErr
		}
		if e == nil && intact && present > 0 {
			break
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return 0, e
		}
		if generation >= 1000 {
			return 0, fmt.Errorf("placement generation limit reached")
		}
		id = fmt.Sprintf("%x", sha256.Sum256([]byte(key+"/"+id)))
		p, err = s.db.GetPlacement(ctx, id)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err = rejectSymlinks(dest, true); err != nil {
			return 0, err
		}
		previous, _, targetGeneration, e := fileDigestSnapshot(ctx, dest)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return 0, e
		}
		targetObserved := e == nil
		if e = os.MkdirAll(filepath.Dir(dest), dirMode()); e != nil {
			return 0, e
		}
		storageIdentity, parentIdentity, e := placementParentIdentity(dest)
		if e != nil {
			return 0, e
		}
		info := mediainfo.Info{}
		if item.Kind != domain.KindBook {
			if recovery != nil {
				info, _ = probe.NativeFile(src)
			} else {
				info, _ = probe.File(src)
			}
		}
		measured, prov, conf := q, mediainfo.ProvenanceRelease, mediainfo.ConfidenceNone
		if item.Kind == domain.KindBook {
			prov = mediainfo.ProvenanceFilename
		}
		if info.Measured() {
			measured, prov, conf = mediainfo.Resolve(info, q, mediainfo.ProvenanceRelease)
			if _, bad := mediainfo.DurationImplausible(info, item.Runtime); bad {
				measured = quality.Quality{Source: quality.SourceUnknown, Resolution: info.ResolutionTier()}
				prov = mediainfo.ProvenanceImplausible
				conf = mediainfo.ConfidenceNone
			}
		}
		p = sqlite.Placement{DownloadID: scope.DownloadID, ReplacementOperationID: scope.ReplacementOperationID,
			SourceGeneration: scope.SourceGeneration, SourceTransfer: scope.Transfer,
			SourcePayloadGeneration: acceptedPublicationGeneration(ctx), SourceGUID: scope.SourceGUID, SourceIndexerID: scope.SourceIndexerID,
			ID: id, Source: src, Target: dest,
			Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id), Backup: filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
			SHA256: hash, PreviousSHA256: previous, TargetObserved: targetObserved, TargetFileGeneration: targetGeneration,
			StorageRoot: filepath.Dir(dest), StorageIdentity: storageIdentity, StorageRootIdentity: parentIdentity,
			TargetParentIdentity: parentIdentity, Size: size, ItemID: item.ID, CopyID: scope.CopyID,
			EpisodeIDs: eps, Quality: measured, Info: info, Provenance: prov, Confidence: conf,
			Release: scope.Release, Indexer: scope.Indexer, State: "prepared"}
		if key, ok := importAttemptFrom(ctx); ok {
			epoch := key.Epoch
			p.ImportEpoch = &epoch
		}
		p.CleanupSuperseded = replace && item.Kind == domain.KindBook && !scope.DeferCleanup && scope.ReplacementOperationID == ""
		p.SupersededDigests = map[string]string{}
		if replace {
			files, e := s.db.ListFilesForItem(ctx, item.ID)
			if e != nil {
				return 0, e
			}
			var approvedReplacementFiles map[int64]string
			if scope.ReplacementOperationID != "" {
				approvedReplacementFiles = map[int64]string{}
				snapshots, snapshotErr := s.db.ListReplacementFiles(ctx, scope.ReplacementOperationID)
				if snapshotErr != nil {
					return 0, snapshotErr
				}
				for _, snapshot := range snapshots {
					if snapshot.Disposition == "selected" {
						approvedReplacementFiles[snapshot.MediaFileID] = snapshot.FileGeneration
					}
				}
			}
			for _, old := range files {
				if old.CopyID != scope.CopyID {
					continue
				}
				if approvedReplacementFiles != nil && approvedReplacementFiles[old.ID] != old.FileGeneration {
					continue
				}
				matched := len(eps) == 0
				for _, ep := range old.EpisodeIDs {
					for _, wanted := range eps {
						if ep == wanted {
							matched = true
						}
					}
				}
				if matched {
					if p.CleanupSuperseded && old.Path != dest {
						digest, _, e := fileDigest(ctx, old.Path)
						if e != nil && !errors.Is(e, os.ErrNotExist) {
							return 0, e
						}
						p.SupersededDigests[old.Path] = digest
					}
					p.Superseded = append(p.Superseded, old)
				}
			}
		}
		if recovery != nil {
			p.RecoveryImport = recovery.ImportID
			p.RecoveryFile = recovery.Files[src]
			if p.RecoveryFile == "" {
				return 0, fmt.Errorf("file is outside the recovery selection")
			}
		}
		// A deterministic policy refusal has no publication custody. Measure
		// and validate before creating the journal; persist before any rename.
		if err = s.validateMeasuredPlacement(ctx, item, scope, p, replace); err != nil {
			return 0, err
		}
		if err = s.db.PreparePlacement(ctx, p); err != nil {
			return 0, err
		}
	}
	// Hashing, backup creation, and payload staging happen outside the global
	// storage authority lock. The item's import target lock still reserves it.
	if err = s.preparePlacementPublication(ctx, &p); err != nil {
		return 0, err
	}
	if p.State == "prepared" {
		if err = s.validateMeasuredPlacement(ctx, item, scope, p, replace); err != nil {
			return 0, err
		}
	}
	var recoveryObservation string
	if recovery != nil && recovery.ObserveFinal != nil {
		recoveryObservation, err = recovery.ObserveFinal()
		if err != nil {
			return 0, err
		}
	}
	if recovery != nil && recovery.Validate != nil {
		if err = recovery.Validate(); err != nil {
			return 0, err
		}
	}
	// Serialize only final identity checks, publication, and the metadata commit.
	decisionCtx, releaseDecision := s.lockStorageDecision(ctx)
	ctx = decisionCtx
	if recovery != nil && recovery.ObserveFinal != nil {
		currentObservation, observeErr := recovery.ObserveFinal()
		if observeErr != nil || currentObservation != recoveryObservation {
			releaseDecision()
			if observeErr != nil {
				return 0, observeErr
			}
			return 0, fmt.Errorf("recovery target changed before placement publication")
		}
	}
	cleanupCtx := ctx
	if err = s.db.AssertPublicationTargetAvailable(ctx, p.Target); err != nil {
		releaseDecision()
		return 0, importControlError(err)
	}
	if p.ImportEpoch != nil && p.State == "prepared" {
		if err = s.db.AuthorizeImportPlacement(ctx, p); err != nil {
			releaseDecision()
			return 0, importControlError(err)
		}
		// Publication won the W serialization boundary. Finish its rename/sync and
		// durable metadata while retaining worker ownership, then honor cancellation.
		ctx = context.WithoutCancel(ctx)
	}
	if err = s.publishPlacement(ctx, &p); err != nil {
		releaseDecision()
		return 0, err
	}
	local, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	fid, err := s.db.CommitPlacement(local, p)
	cancelCommit()
	releaseDecision()
	if err != nil {
		return 0, err
	}
	if p.State != "committed" {
		if why, bad := mediainfo.DurationImplausible(p.Info, item.Runtime); bad {
			s.recordImplausible(ctx, item.ID, dest, scope.Release, p.Info, why)
		} else if p.Provenance == mediainfo.ProvenanceImplausible {
			why, _ := mediainfo.Implausible(p.Info)
			s.recordImplausible(ctx, item.ID, dest, scope.Release, p.Info, why)
		}
		if q.Resolution != 0 && p.Quality.Resolution != 0 && q.Resolution != p.Quality.Resolution {
			_ = s.db.AddHistory(ctx, HistoryQualityMismatch, item.ID, scope.Release, map[string]any{"claimed": q.String(), "measured": p.Quality.String(), "file": filepath.Base(dest), "facts": p.Info.Summary()})
		}
		if profile, perr := s.db.GetProfile(ctx, scope.ProfileID); perr == nil {
			s.recordLanguageMismatch(ctx, item.ID, dest, scope.Release, scope.Indexer, profile, p.Info)
		}
	}
	if e := s.cleanupPlacement(cleanupCtx, p); e != nil {
		s.log.Warn("placement cleanup remains pending", "id", p.ID, "err", e)
	}
	return fid, nil
}
func (s *Service) cleanupPlacement(ctx context.Context, p sqlite.Placement) error {
	if p.CleanupSuperseded {
		for _, old := range p.Superseded {
			if old.Path == p.Target {
				continue
			}
			got, _, err := fileDigest(ctx, old.Path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				if got != p.SupersededDigests[old.Path] {
					return fmt.Errorf("superseded book changed: %s", old.Path)
				}
				if err = os.Remove(old.Path); err != nil {
					return err
				}
			}
			if err = syncPath(filepath.Dir(old.Path)); err != nil {
				return err
			}
			if err = s.db.DeleteFile(ctx, old.ID); err != nil {
				return err
			}
		}
	}
	for path, expected := range map[string]string{p.Backup: p.PreviousSHA256, p.Temporary: p.SHA256} {
		if expected == "" {
			continue
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// The rollback link and the stage are this journal's own names; the
		// identity it recorded for them says which object they are. Only an
		// object it cannot name is hashed before it is removed.
		known := info.Mode().IsRegular() && ((path == p.Temporary && p.TemporaryObjectIdentity != "" && deliveryFileStableIdentity(info) == p.TemporaryObjectIdentity) ||
			(path == p.Backup && (sameContentGeneration(p.TargetAfterBackupGen, info) || sameContentGeneration(p.TargetFileGeneration, info))))
		if !known {
			got, _, err := fileDigest(ctx, path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if got != expected {
				return fmt.Errorf("cleanup identity mismatch: %s", path)
			}
		}
		if err = os.Remove(path); err != nil {
			return err
		}
		if err = syncPath(filepath.Dir(path)); err != nil {
			return err
		}
	}
	_, err := s.db.W.ExecContext(ctx, `UPDATE import_placements SET state='cleaned',updated_at=? WHERE id=? AND state='committed'`, time.Now().UnixMilli(), p.ID)
	return err
}

func placementParentIdentity(target string) (string, string, error) {
	parent := filepath.Dir(target)
	if err := rejectSymlinks(parent, false); err != nil {
		return "", "", err
	}
	info, err := os.Stat(parent)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("placement parent is not a directory: %s", parent)
	}
	storage, err := expectedStorageIdentity(parent)
	if err != nil {
		return "", "", err
	}
	identity := deliveryDirectoryObjectIdentity(info)
	if identity == "" {
		return "", "", fmt.Errorf("placement parent identity is unavailable: %s", parent)
	}
	return storage, identity, nil
}

func (s *Service) preparePlacementPublication(ctx context.Context, p *sqlite.Placement) error {
	if p.ImportEpoch != nil {
		a, err := s.db.GetImportAttempt(ctx, p.DownloadID, p.SourceGeneration)
		if err != nil {
			return err
		}
		if a.Epoch != *p.ImportEpoch || a.Outcome == "cancelled" {
			return importControlError(sqlite.ErrImportCancelled)
		}
	}
	legacyIdentitySnapshot := legacyPlacementIdentitySnapshot(*p)
	if err := rejectSymlinks(filepath.Dir(p.Target), false); err != nil {
		return err
	}
	storage, parentIdentity, err := placementParentIdentity(p.Target)
	if err != nil {
		return err
	}
	if p.StorageIdentity != "" && p.StorageIdentity != storage {
		return fmt.Errorf("placement storage identity changed; review required")
	}
	if p.TargetParentIdentity != "" && p.TargetParentIdentity != parentIdentity {
		return fmt.Errorf("placement parent identity changed; review required")
	}
	if p.StorageIdentity == "" {
		p.StorageIdentity = storage
	}
	if p.StorageRoot == "" {
		p.StorageRoot = filepath.Dir(p.Target)
	}
	if p.StorageRootIdentity == "" {
		p.StorageRootIdentity = parentIdentity
	}
	if p.TargetParentIdentity == "" {
		p.TargetParentIdentity = parentIdentity
	}
	if p.State != "prepared" {
		intact, err := committedPlacementIntact(ctx, *p)
		if err != nil {
			return err
		}
		if !intact {
			return fmt.Errorf("%w: "+"committed placement bytes or generation changed; a new placement is required", errPlacementConflict)
		}
		info, err := os.Lstat(p.Target)
		if err != nil {
			return err
		}
		if err = syncPath(p.Target); err != nil {
			return err
		}
		p.PublishedFileGeneration = deliveryFileGeneration(info)
		return nil
	}

	// A crash after rename but before the DB transaction: the target is this
	// intent's own staged object (its recorded inode and content generation),
	// so its bytes are the ones the copy proved. Only a legacy journal with no
	// recorded identity needs a digest to say so.
	if info, e := os.Lstat(p.Target); e == nil {
		generation := deliveryFileGeneration(info)
		published := placementPublishedByIdentity(*p, info)
		if !published && legacyIdentitySnapshot && info.Mode().IsRegular() {
			digest, _, g, de := fileDigestSnapshot(ctx, p.Target)
			if de != nil {
				return de
			}
			published = digest == p.SHA256 && publishedPlacementMatchesIntent(*p, info, g, true)
			generation = g
		}
		if published {
			if e = syncPath(p.Target); e != nil {
				return e
			}
			p.PublishedFileGeneration = generation
			return s.db.UpdatePreparedPlacement(ctx, *p)
		}
		if p.PublishedFileGeneration != "" {
			return fmt.Errorf("%w: "+"published placement generation changed", errPlacementConflict)
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}

	// The target as the preview recorded it is proven by its generation; it
	// is hashed only when that generation moved (or was never recorded).
	var currentDigest, currentGeneration string
	var targetErr error
	if info, e := os.Lstat(p.Target); e == nil && info.Mode().IsRegular() && p.TargetFileGeneration != "" &&
		(deliveryFileGeneration(info) == p.TargetFileGeneration || (p.TargetAfterBackupGen != "" && deliveryFileGeneration(info) == p.TargetAfterBackupGen)) {
		currentDigest, currentGeneration = p.PreviousSHA256, deliveryFileGeneration(info)
	} else {
		currentDigest, _, currentGeneration, targetErr = fileDigestSnapshot(ctx, p.Target)
	}
	if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return targetErr
	}
	if targetErr == nil {
		if currentDigest != p.PreviousSHA256 {
			return fmt.Errorf("%w: target changed after placement preview: %s", errPlacementConflict, p.Target)
		}
		if p.TargetFileGeneration == "" {
			// Old journals can be upgraded only after the digest proves the
			// exact previous bytes they recorded.
			p.TargetObserved = true
			p.TargetFileGeneration = currentGeneration
		} else if currentGeneration != p.TargetFileGeneration && currentGeneration != p.TargetAfterBackupGen {
			// An interrupted hard-link backup changes ctime on the target. It is
			// accepted only when the backup hashes to the reviewed bytes, is the
			// same inode, and its post-hash generation still matches the target.
			backupDigest, _, backupGeneration, backupErr := fileDigestSnapshot(ctx, p.Backup)
			if backupErr != nil || backupDigest != p.PreviousSHA256 {
				return fmt.Errorf("%w: target generation changed after placement preview: %s", errPlacementConflict, p.Target)
			}
			backupInfo, backupErr := os.Stat(p.Backup)
			targetInfo, targetErr := os.Stat(p.Target)
			if backupErr != nil || targetErr != nil || !os.SameFile(backupInfo, targetInfo) ||
				deliveryFileGeneration(targetInfo) != backupGeneration || deliveryFileGeneration(targetInfo) != currentGeneration {
				return fmt.Errorf("interrupted rollback link does not prove the reviewed target")
			}
			p.TargetAfterBackupGen = backupGeneration
		}
	} else if p.TargetObserved || p.PreviousSHA256 != "" {
		return fmt.Errorf("%w: "+"reviewed placement target disappeared before publication", errPlacementConflict)
	}

	// The stage is scratch (design principle 6: every state has an automatic
	// exit). It is reused only when this journal recorded it as fully copied
	// and proven — its inode and content generation — and is otherwise
	// deleted and written again. The name is this placement's own
	// (.monarr-stage-<id>, the id binding the import epoch), and the import
	// that wrote it has returned: one import runs per download, and a
	// restart ended the process that held it.
	stageReady := false
	if stagedInfo, e := os.Lstat(p.Temporary); e == nil {
		stageReady = stagedInfo.Mode().IsRegular() && stagedInfo.Mode()&os.ModeSymlink == 0 && stagedInfo.Size() == p.Size &&
			p.TemporaryObjectIdentity != "" && deliveryFileStableIdentity(stagedInfo) == p.TemporaryObjectIdentity &&
			p.TemporaryFileGeneration != "" && sameContentGeneration(p.TemporaryFileGeneration, stagedInfo)
		if stageReady {
			p.TemporaryFileGeneration = deliveryFileGeneration(stagedInfo)
		} else {
			if err = discardStage(p.Temporary); err != nil {
				return err
			}
			p.TemporaryFileGeneration, p.TemporaryObjectIdentity = "", ""
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if !stageReady {
		// Nothing recorded about an earlier stage describes the one about to
		// be written.
		p.TemporaryFileGeneration = ""
		progress := transfers.Progress(ctx)
		if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok && recovery.Progress != nil {
			previous := progress
			progress = func(done, total int64) {
				if previous != nil {
					previous(done, total)
				}
				recovery.Progress(p.Source, done, total)
			}
		}
		if err = validateReviewedSourceSnapshot(ctx, p.Source); err != nil {
			return err
		}
		stagingCtx, written := withStageDigest(ctx)
		if p.ImportEpoch != nil {
			stagingCtx = context.WithValue(stagingCtx, namedPlacementStageKey{}, namedPlacementStage{source: p.Source, target: p.Temporary, write: func(c context.Context, on func(int64, int64)) error { return s.stageImportPlacement(c, p, on) }})
		}
		err = placeFile(stagingCtx, p.Source, p.Temporary, progress)
		if err != nil {
			return err
		}
		if err = validateReviewedSourceSnapshot(ctx, p.Source); err != nil {
			return err
		}
		stagedInfo, statErr := os.Lstat(p.Temporary)
		if statErr != nil {
			return statErr
		}
		// The bytes were proven as they were written: the stream's digest
		// against the recorded one (the manifest's, for a publication). A
		// link is the source object itself.
		switch {
		case written.linked:
			if stagedInfo.Size() != p.Size {
				_ = discardStage(p.Temporary)
				return fmt.Errorf("linked stage size %d differs from the recorded %d: %s", stagedInfo.Size(), p.Size, p.Source)
			}
			if crossVolumePublication(ctx) && manifestConstrained(ctx, p.Source) {
				sum, _, e := fileDigest(ctx, p.Temporary)
				if e != nil {
					_ = discardStage(p.Temporary)
					return e
				}
				if sum != p.SHA256 {
					_ = discardStage(p.Temporary)
					s.abandonUnpublishedPlacement(ctx, p.ID)
					return fmt.Errorf("%w: source file no longer matches its accepted delivery manifest: %s", errPayloadMismatch, p.Source)
				}
			}
		case written.sum == "":
			_ = discardStage(p.Temporary)
			return fmt.Errorf("the stage was written without a digest: %s", p.Temporary)
		case written.sum != p.SHA256:
			_ = discardStage(p.Temporary)
			s.abandonUnpublishedPlacement(ctx, p.ID)
			if manifestConstrained(ctx, p.Source) {
				return fmt.Errorf("%w: source file no longer matches its accepted delivery manifest: %s", errPayloadMismatch, p.Source)
			}
			return fmt.Errorf("copy digest mismatch: %s", p.Source)
		}
		p.TemporaryFileGeneration = deliveryFileGeneration(stagedInfo)
		namedIdentity := deliveryFileStableIdentity(stagedInfo)
		if p.ImportEpoch != nil && (p.TemporaryObjectIdentity == "" || p.TemporaryObjectIdentity != namedIdentity) {
			// Something replaced the stage under its name; it is scratch, and
			// the next attempt writes it again.
			_ = discardStage(p.Temporary)
			return fmt.Errorf("%w: "+"stage pathname no longer identifies the opened object; it is discarded and written again on retry", errPlacementConflict)
		}
		if p.ImportEpoch == nil {
			p.TemporaryObjectIdentity = namedIdentity
		}
	}
	if p.TemporaryFileGeneration == "" || p.TemporaryObjectIdentity == "" {
		return fmt.Errorf("staged file identity is unavailable")
	}
	if err = syncPath(p.Temporary); err != nil {
		return err
	}
	// Persist the staged directory entry and any newly created ancestors before
	// the later short, anchored publication section.
	if err = syncPublicationDirectory(p.Temporary); err != nil {
		return err
	}

	if p.TargetObserved {
		// The rollback link recorded its generation when it was made; an
		// unchanged one is that link, unread.
		var backupDigest, backupGeneration string
		var backupErr error
		if info, e := os.Lstat(p.Backup); e == nil && info.Mode().IsRegular() && p.TargetAfterBackupGen != "" && deliveryFileGeneration(info) == p.TargetAfterBackupGen {
			backupDigest, backupGeneration = p.PreviousSHA256, deliveryFileGeneration(info)
		} else {
			backupDigest, _, backupGeneration, backupErr = fileDigestSnapshot(ctx, p.Backup)
		}
		if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
			return backupErr
		}
		if backupErr == nil {
			if backupDigest != p.PreviousSHA256 {
				return fmt.Errorf("rollback identity mismatch")
			}
			backupInfo, e := os.Stat(p.Backup)
			if e != nil {
				return e
			}
			targetInfo, e := os.Stat(p.Target)
			if e != nil {
				return e
			}
			if !os.SameFile(backupInfo, targetInfo) {
				return fmt.Errorf("rollback link no longer identifies the reviewed target")
			}
			targetAfterHash := deliveryFileGeneration(targetInfo)
			if targetAfterHash == "" || backupGeneration == "" || targetAfterHash != backupGeneration {
				return fmt.Errorf("placement target generation is unavailable")
			}
			if p.TargetAfterBackupGen != "" && p.TargetAfterBackupGen != targetAfterHash {
				return fmt.Errorf("rollback target changed after backup verification")
			}
			p.TargetAfterBackupGen = targetAfterHash
		}
	}
	if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok && recovery.Validate != nil {
		if err = recovery.Validate(); err != nil {
			return err
		}
	}
	return s.db.UpdatePreparedPlacement(ctx, *p)
}

// abandonUnpublishedPlacement closes the journal of a placement whose bytes
// were refused before anything was published (the copy did not match the
// recorded digest): nothing of it is in the library, so it is rolled back
// rather than left prepared for reconciliation to revisit.
func (s *Service) abandonUnpublishedPlacement(ctx context.Context, id string) {
	_, _ = s.db.W.ExecContext(context.WithoutCancel(ctx), `UPDATE import_placements SET state='rolled_back',updated_at=? WHERE id=? AND state='prepared'`, time.Now().UnixMilli(), id)
}

// discardStage removes a placement's scratch stage and makes the removal
// durable. A stage that is already gone is fine.
func discardStage(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return syncPath(dir)
		}
	}
	return nil
}

// Legacy journals predate the per-object publication identity fields. They may
// be upgraded from a digest-proven published target only when none of those
// identity fields were ever recorded. Newer journals must bind publication to
// the staged object or to the exact reviewed target generation.
func legacyPlacementIdentitySnapshot(p sqlite.Placement) bool {
	return p.StorageIdentity == "" && p.TargetParentIdentity == "" &&
		p.TargetFileGeneration == "" && p.TemporaryFileGeneration == "" && p.TemporaryObjectIdentity == ""
}

func publishedPlacementMatchesIntent(p sqlite.Placement, info os.FileInfo, generation string, allowLegacy bool) bool {
	if info == nil || generation == "" || deliveryFileGeneration(info) != generation {
		return false
	}
	if p.PublishedFileGeneration != "" {
		return generation == p.PublishedFileGeneration
	}
	if p.TemporaryObjectIdentity != "" {
		return deliveryFileStableIdentity(info) == p.TemporaryObjectIdentity
	}
	if p.TemporaryFileGeneration != "" {
		return false
	}
	if p.TargetObserved {
		return generation == p.TargetFileGeneration || (p.TargetAfterBackupGen != "" && generation == p.TargetAfterBackupGen)
	}
	return allowLegacy
}

func (s *Service) publishPlacement(ctx context.Context, p *sqlite.Placement) error {
	storage, parentIdentity, err := placementParentIdentity(p.Target)
	if err != nil {
		return err
	}
	if storage != p.StorageIdentity || parentIdentity != p.TargetParentIdentity || parentIdentity != p.StorageRootIdentity {
		return fmt.Errorf("placement storage or parent identity changed; review required")
	}
	root, err := os.OpenRoot(filepath.Dir(p.Target))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || deliveryDirectoryObjectIdentity(rootInfo) != p.TargetParentIdentity {
		return fmt.Errorf("placement parent changed before publication")
	}
	pathParentInfo, err := os.Stat(filepath.Dir(p.Target))
	if err != nil || !os.SameFile(rootInfo, pathParentInfo) {
		return fmt.Errorf("placement parent path changed before publication")
	}
	targetName := filepath.Base(p.Target)
	temporaryName := filepath.Base(p.Temporary)
	backupName := filepath.Base(p.Backup)
	if p.PublishedFileGeneration != "" {
		info, e := root.Lstat(targetName)
		if e != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || deliveryFileGeneration(info) != p.PublishedFileGeneration {
			return fmt.Errorf("%w: "+"published placement generation changed", errPlacementConflict)
		}
		return syncOpenedDirectory(root)
	}
	if p.State == "committed" {
		return fmt.Errorf("%w: "+"committed target changed; a new preview is required", errPlacementConflict)
	}
	stagedInfo, err := root.Lstat(temporaryName)
	if err != nil || stagedInfo.Mode()&os.ModeSymlink != 0 || !stagedInfo.Mode().IsRegular() ||
		deliveryFileGeneration(stagedInfo) != p.TemporaryFileGeneration || deliveryFileStableIdentity(stagedInfo) != p.TemporaryObjectIdentity {
		return fmt.Errorf("%w: "+"staging generation changed before publication", errPlacementConflict)
	}
	if p.TargetObserved {
		current, e := root.Lstat(targetName)
		if e != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
			(deliveryFileGeneration(current) != p.TargetAfterBackupGen && deliveryFileGeneration(current) != p.TargetFileGeneration) {
			return fmt.Errorf("%w: "+"target generation changed before publication", errPlacementConflict)
		}
		backup, backupErr := root.Lstat(backupName)
		if errors.Is(backupErr, os.ErrNotExist) {
			if deliveryFileGeneration(current) != p.TargetFileGeneration {
				return fmt.Errorf("original target changed before rollback link")
			}
			preLink := current
			if err = root.Link(targetName, backupName); err != nil {
				return err
			}
			backup, backupErr = root.Lstat(backupName)
			current, e = root.Lstat(targetName)
			if backupErr != nil || e != nil || !os.SameFile(current, backup) || !samePlacementContentObservation(preLink, current) || !samePlacementContentObservation(preLink, backup) {
				return fmt.Errorf("rollback link could not be bound to the reviewed target")
			}
			p.TargetAfterBackupGen = deliveryFileGeneration(current)
			if p.TargetAfterBackupGen == "" {
				return fmt.Errorf("placement target generation is unavailable")
			}
			if err = syncOpenedDirectory(root); err != nil {
				return err
			}
			if err = s.db.UpdatePreparedPlacement(ctx, *p); err != nil {
				return err
			}
		} else if backupErr != nil || !os.SameFile(current, backup) {
			return fmt.Errorf("rollback link changed before publication")
		}
	} else if _, e := root.Lstat(targetName); e == nil || !errors.Is(e, os.ErrNotExist) {
		return fmt.Errorf("%w: "+"unreviewed target appeared before publication", errPlacementConflict)
	}
	if err = root.Rename(temporaryName, targetName); err != nil {
		return err
	}
	if err = syncOpenedDirectory(root); err != nil {
		return err
	}
	published, err := root.Lstat(targetName)
	if err != nil || published.Mode()&os.ModeSymlink != 0 || !published.Mode().IsRegular() {
		return fmt.Errorf("published placement cannot be identified")
	}
	pathParentInfo, err = os.Stat(filepath.Dir(p.Target))
	if err != nil || !os.SameFile(rootInfo, pathParentInfo) {
		return fmt.Errorf("placement parent path changed during publication")
	}
	if !samePlacementContentObservation(stagedInfo, published) {
		return fmt.Errorf("published file differs from the staged content observation")
	}
	p.PublishedFileGeneration = deliveryFileGeneration(published)
	if p.PublishedFileGeneration == "" {
		return fmt.Errorf("published placement generation is unavailable")
	}
	if deliveryFileStableIdentity(published) != p.TemporaryObjectIdentity {
		return fmt.Errorf("published object differs from the staged object")
	}
	return s.db.UpdatePreparedPlacement(ctx, *p)
}

// Publication changes directory entries and therefore may change ctime, but it
// must preserve the exact inode and content observation captured before the
// link/rename. The content digest was verified before entering storage authority.
func samePlacementContentObservation(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func syncOpenedDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// reconcilePlacements finishes the placement journal a previous process left
// behind. It runs in the background after the workers start, so imports begin
// at once: each row is taken under its own title's import target (the same
// lock an import of that title holds), never the whole library, and decided by
// stat against the identity the journal recorded. A row whose target is not
// provably its published object is left prepared; the title's next import
// writes the stage again and publishes it (redoing the copy is the answer to
// any doubt).
func (s *Service) reconcilePlacements(ctx context.Context) {
	ctx, releaseWork, admissionErr := ports.EnterWork(ctx, s.admission, "reconcilePlacements")
	if admissionErr != nil {
		return
	}
	defer releaseWork()
	rows, err := s.db.PendingPlacements(ctx)
	if err != nil {
		s.log.Error("placement reconciliation unavailable", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		// Rotate incomplete work so one damaged placement cannot starve later rows.
		_, _ = s.db.W.ExecContext(ctx, `UPDATE import_placements SET updated_at=? WHERE id=?`, time.Now().UnixMilli(), row.ID)
		targetCtx, releaseTarget := s.lockImportTarget(ctx, row.ItemID)
		// Re-read under the lock: an import of this title may have finished
		// (or moved) the row while this pass waited for it.
		p, e := s.db.GetPlacement(targetCtx, row.ID)
		if e == nil && (p.State == "prepared" || p.State == "committed") {
			s.reconcilePlacement(targetCtx, p)
		}
		releaseTarget()
	}
}

// reconcilePlacement settles one journal row; the caller holds its title.
func (s *Service) reconcilePlacement(ctx context.Context, p sqlite.Placement) {
	if p.State == "committed" {
		if e := s.cleanupPlacement(ctx, p); e != nil {
			s.log.Warn("placement cleanup pending", "id", p.ID, "err", e)
		}
		return
	}
	var recovery RecoveryImport
	var recoveryObservation string
	if p.RecoveryImport != "" {
		r, e := s.RecoveryImport(ctx, p.RecoveryImport)
		if e != nil {
			return
		}
		recovery = r
		if r.State == "cancel_pending" || r.State == "cancelled" {
			if e = s.rollbackPlacement(ctx, p); e != nil {
				s.log.Warn("cancelled placement needs reconciliation", "id", p.ID, "err", e)
			}
			return
		}
		recoveryObservation, e = s.recoveryFinalObservation(ctx, r)
		if e != nil {
			return
		}
		if e = s.validateRecoveryTarget(ctx, r); e != nil {
			return
		}
	}
	// Reconcile only already published bytes. An old unplaced intention cannot
	// replace a newer library file merely because the daemon restarted.
	info, statErr := os.Lstat(p.Target)
	if statErr != nil {
		return
	}
	generation := deliveryFileGeneration(info)
	if !placementPublishedByIdentity(p, info) {
		if !legacyPlacementIdentitySnapshot(p) || !info.Mode().IsRegular() {
			return
		}
		// A journal from before stage identities were recorded can only be
		// matched by its digest.
		got, _, g, e := fileDigestSnapshot(ctx, p.Target)
		if e != nil || got != p.SHA256 || !publishedPlacementMatchesIntent(p, info, g, true) {
			return
		}
		generation = g
	}
	if e := syncPath(p.Target); e != nil {
		return
	}
	p.PublishedFileGeneration = generation
	storage, parentIdentity, identityErr := placementParentIdentity(p.Target)
	if identityErr != nil {
		return
	}
	if p.StorageIdentity == "" {
		p.StorageIdentity = storage
	}
	if p.StorageRoot == "" {
		p.StorageRoot = filepath.Dir(p.Target)
	}
	if p.StorageRootIdentity == "" {
		p.StorageRootIdentity = parentIdentity
	}
	if p.TargetParentIdentity == "" {
		p.TargetParentIdentity = parentIdentity
	}
	if e := s.db.UpdatePreparedPlacement(ctx, p); e != nil {
		return
	}
	decisionCtx, release := s.lockStorageDecision(ctx)
	if p.RecoveryImport != "" {
		currentObservation, observeErr := s.recoveryFinalObservation(decisionCtx, recovery)
		if observeErr != nil || currentObservation != recoveryObservation {
			release()
			return
		}
	}
	current, statErr := os.Lstat(p.Target)
	storage, parentIdentity, identityErr = placementParentIdentity(p.Target)
	if statErr != nil || identityErr != nil || !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 ||
		deliveryFileGeneration(current) != p.PublishedFileGeneration || storage != p.StorageIdentity ||
		parentIdentity != p.TargetParentIdentity || parentIdentity != p.StorageRootIdentity {
		release()
		return
	}
	if e := syncPublicationDirectory(p.Target); e != nil {
		release()
		return
	}
	_, e := s.db.CommitPlacement(decisionCtx, p)
	release()
	if e != nil {
		s.log.Error("placement commit remains pending", "id", p.ID, "err", e)
	} else if e = s.cleanupPlacement(ctx, p); e != nil {
		s.log.Warn("placement cleanup pending", "id", p.ID, "err", e)
	}
}

// A previous rename is not evidence that its fsync succeeded. Recovery repeats
// both file and directory synchronization, including newly created ancestors.
func syncPublication(path string) error {
	if err := syncPath(path); err != nil {
		return err
	}
	return syncPublicationDirectory(path)
}

func syncPublicationDirectory(path string) error {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		if err := syncPath(parent); err != nil {
			return err
		}
		if filepath.Dir(parent) == parent {
			return nil
		}
	}
}
func (s *Service) rollbackPlacement(ctx context.Context, p sqlite.Placement) error {
	got, _, err := fileDigest(ctx, p.Target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if got == p.SHA256 || (errors.Is(err, os.ErrNotExist) && p.PreviousSHA256 != "") {
		if p.PreviousSHA256 == "" {
			if err = os.Remove(p.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			backup, _, e := fileDigest(ctx, p.Backup)
			if e != nil {
				return e
			}
			if backup != p.PreviousSHA256 {
				return fmt.Errorf("rollback backup changed: %s", p.Backup)
			}
			if e = os.Rename(p.Backup, p.Target); e != nil {
				return e
			}
			if e = syncPublication(p.Target); e != nil {
				return e
			}
		}
	} else if got != p.PreviousSHA256 {
		return fmt.Errorf("cancelled target changed: %s", p.Target)
	}
	if _, err = os.Stat(filepath.Dir(p.Target)); err == nil {
		if err = syncPath(filepath.Dir(p.Target)); err != nil {
			return err
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = s.cleanupPlacement(ctx, sqlite.Placement{ID: p.ID, Target: p.Target, Backup: p.Backup, Temporary: p.Temporary, SHA256: p.SHA256, PreviousSHA256: p.PreviousSHA256}); err != nil {
		return err
	}
	_, err = s.db.W.ExecContext(ctx, `UPDATE import_placements SET state='rolled_back',updated_at=? WHERE id=? AND state='prepared'`, time.Now().UnixMilli(), p.ID)
	return err
}
func (s *Service) rollbackRecoveryPlacements(ctx context.Context, id string) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE state='prepared' AND json_extract(data,'$.recovery_import')=?`, id)
	if err != nil {
		return err
	}
	var placements []sqlite.Placement
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var p sqlite.Placement
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			break
		}
		placements = append(placements, p)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, p := range placements {
		if err = s.rollbackPlacement(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) verifyRecoveryPublications(ctx context.Context, r RecoveryImport) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE state IN ('committed','cleaned') AND json_extract(data,'$.recovery_import')=?`, r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	seen, files := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var p sqlite.Placement
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		path := strings.ToLower(filepath.Clean(p.Target))
		if seen[path] || files[p.RecoveryFile] {
			return fmt.Errorf("recovery placements overlap")
		}
		seen[path], files[p.RecoveryFile] = true, true
		got, size, e := fileDigest(ctx, p.Target)
		if e != nil {
			return e
		}
		if got != p.SHA256 || size != p.Size {
			return fmt.Errorf("published recovery changed before receipt: %s", p.Target)
		}
		if e = syncPublication(p.Target); e != nil {
			return e
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(files) != len(r.Recovery.Files) {
		return fmt.Errorf("not every staged file has a surviving publication")
	}
	return nil
}
