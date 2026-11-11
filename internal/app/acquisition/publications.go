package acquisition

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Publications are the import unit (pipeline contract v2, section 3.4): one
// per Runner generation, each with its own import, notify and cleanup
// lifecycle and a per-file outcome. A download row may have several (the
// completed one, a salvage, a late one).

// publicationManifest is the v1 adapter (contract section 6): the manifest
// from the publication's own .runner-manifest.json when it has one, else from
// Runner's v1 delivery record (stored on the publication when the deliveries
// feed carried it, or looked up by its delivery id). A directory with neither
// is not a publication yet: ok=false, and the next poll checks again.
func (s *Service) publicationManifest(ctx context.Context, cfg ports.ClientConfig, pub sqlite.Publication) (ports.Manifest, bool, error) {
	if raw, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName)); err == nil {
		var m ports.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return m, false, fmt.Errorf("%s is not a manifest: %w", ports.ManifestFileName, err)
		}
		m.Source = "file"
		if err := verifyManifestShape(m, pub.Generation); err != nil {
			return m, false, err
		}
		return m, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ports.Manifest{}, false, err
	}
	if pub.ManifestSource == "v1_record" || pub.ManifestSource == "file" {
		var m ports.Manifest
		if err := json.Unmarshal([]byte(pub.ManifestJSON), &m); err == nil && len(m.Files) > 0 {
			m.Source = "v1_record"
			if err := verifyManifestShape(m, pub.Generation); err != nil {
				return m, false, err
			}
			return m, true, nil
		}
	}
	// The record may exist on Runner without having reached the feed cache:
	// look it up by the delivery id the stored manifest names, when it does.
	var stored ports.Manifest
	_ = json.Unmarshal([]byte(pub.ManifestJSON), &stored)
	if stored.DeliveryID != "" {
		if lookup, ok := s.newClient(cfg).(ports.DeliveryLookupProtocol); ok {
			d, err := lookup.GetDelivery(ctx, stored.DeliveryID)
			if err != nil {
				return ports.Manifest{}, false, err
			}
			m := ports.ManifestFromRunnerFiles(d.PublicationGeneration, d.ManifestDigest, d.DeliveryID, d.Files)
			if err := verifyManifestShape(m, pub.Generation); err != nil {
				return m, false, err
			}
			return m, true, nil
		}
	}
	return ports.Manifest{}, false, nil
}

// verifyManifestShape checks what a manifest says about itself, cheaply: a
// generation that matches the publication, safe relative paths, and a digest
// equal to the deployed algorithm over its own file list. It reads no bytes.
func verifyManifestShape(m ports.Manifest, generation string) error {
	if len(m.Files) == 0 {
		return fmt.Errorf("manifest lists no files")
	}
	if generation != "" && m.Generation != "" && m.Generation != generation {
		return fmt.Errorf("manifest generation %s does not match publication generation %s", m.Generation, generation)
	}
	for _, f := range m.Files {
		if !safeDeliveryRelativePath(f.Path) || f.Bytes < 0 {
			return fmt.Errorf("manifest file %q is not a safe relative path", f.Path)
		}
	}
	if m.Digest != "" {
		digest, err := ports.ManifestDigest(m.Files)
		if err != nil {
			return err
		}
		if digest != m.Digest {
			return fmt.Errorf("manifest digest %s does not match its file list (%s)", m.Digest, digest)
		}
	}
	return nil
}

// A copied (cross-volume) publication is verified against its manifest once,
// at import (contract section 2.5), and only the files Curator places: the
// placement copy hashes its stream against the manifest digest, and a
// same-filesystem link of a cross-volume file is hashed once
// (withCrossVolumePublication). A file Curator does not place is discarded or
// left unresolved without being read.

// enqueuePublicationImport offers one publication to the import workers.
func (s *Service) enqueuePublicationImport(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, pubID int64) bool {
	return s.enqueuePublicationImportApproved(ctx, dl, cfg, pubID, false)
}

func (s *Service) enqueuePublicationImportApproved(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, pubID int64, approved bool) bool {
	if s.ImportRunning(dl.ID) {
		return false
	}
	if a, err := s.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey); err == nil && (a.Outcome == "completed" || approved) {
		// A row that already imported one publication (a salvage arriving
		// after the completed one, or the reverse) imports the next in a
		// fresh attempt epoch. So does a person's Resolve selection: it is an
		// explicit retry, and the epoch that left the file unresolved may
		// still hold that file's stage.
		if _, err := s.db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); err != nil {
			s.log.Debug("publication import could not open a new attempt", "download", dl.ID, "err", err)
			return false
		}
	}
	return s.enqueueImportJobFor(ctx, dl, cfg, importJob{publicationID: pubID, approved: approved}, true)
}

// publicationImportKey marks an import run for a publication: the caller,
// not importDownloadFiles, writes its history and its notification, in the
// publication's completing transaction.
type publicationImportKey struct{}

func publicationImport(ctx context.Context) bool {
	v, _ := ctx.Value(publicationImportKey{}).(bool)
	return v
}

// manifestNotice rate-limits the "no manifest yet" log line per publication.
var manifestNotice sync.Map // publication id -> time.Time

// runPublicationImport imports one publication file by file and records the
// outcome of every file (contract section 3.4).
func (s *Service) runPublicationImport(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, pubID int64, approved bool) (err error) {
	pub, err := s.db.GetPublication(ctx, pubID)
	if err != nil {
		return err
	}
	if pub.DownloadID != dl.ID || (pub.State != "discovered" && pub.State != "importing" && (!approved || pub.State != "import_failed")) {
		return nil
	}
	m, ok, err := s.publicationManifest(ctx, cfg, pub)
	if err != nil {
		s.failPublication(ctx, &dl, pub, "manifest refused: "+err.Error())
		return err
	}
	if !ok {
		if last, seen := manifestNotice.Load(pub.ID); !seen || time.Since(last.(time.Time)) > time.Hour {
			manifestNotice.Store(pub.ID, time.Now())
			s.log.Info("publication has no manifest yet; checking again on the next poll", "publication", pub.ID, "path", pub.Path)
		}
		return nil
	}
	// The publication must sit under a configured completed root on the
	// storage that root was recorded on: an unmounted share is an empty
	// directory, not a missing payload.
	if roots, err := s.completedRoots(ctx, nil); err == nil {
		if _, _, err := deliveryRootAndRelative(pub.Path, roots); err != nil {
			s.failPublication(ctx, &dl, pub, err.Error())
			return err
		}
	}
	if err := s.checkStorageIdentity(ctx, pub.Path); err != nil {
		_ = s.db.SetPublicationState(ctx, pub.ID, "discovered", err.Error())
		return err
	}
	files := make([]sqlite.PublicationFile, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, sqlite.PublicationFile{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256})
	}
	if err := s.db.SeedPublicationFiles(ctx, pub.ID, files); err != nil {
		return err
	}
	// ManualApproval is honoured on this path (contract section 6): the row
	// waits for a person, and approving it imports this publication.
	if cfg.ManualApproval && !approved {
		if pub.Kind == "completed" && dl.State != "awaiting_import" {
			s.advance(ctx, &dl, "awaiting_import", 1, "", stepAwaiting, "held for your approval before import")
		}
		return nil
	}
	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return err
	}
	if pub.Kind == "completed" {
		s.startRunnerDoneTiming(ctx, dl)
	}
	if err := s.db.StartPublicationImport(ctx, pub.ID); err != nil {
		return err
	}
	// Any error that leaves the publication in importing (a persistence
	// outage after files were placed, say) puts it back to discovered with
	// the error, so the poll offers it again; placements already committed
	// are reused, and its six-hour deadline (from this import's start) is
	// what ends it.
	defer func() {
		if err == nil {
			return
		}
		bg := context.WithoutCancel(ctx)
		if cur, gerr := s.db.GetPublication(bg, pub.ID); gerr == nil && cur.State == "importing" {
			_ = s.db.SetPublicationState(bg, pub.ID, "discovered", err.Error())
		}
	}()
	if pub.Kind == "completed" && dl.State != "importing" {
		dl.ImportPath, dl.SavePath = pub.Path, pub.RemotePath
		s.advance(ctx, &dl, "importing", 1, "", stepImporting, "importing publication "+filepath.Base(pub.Path))
	}
	// What to place: the media of the item's kind, not samples; or exactly
	// what a person selected in Resolve.
	// A file an earlier attempt (or a migrated v1 receipt) already resolved is
	// not offered again: only the rest is imported (plan M2 test (n)).
	resolved := map[string]bool{}
	if rows, err := s.db.ListPublicationFiles(ctx, pub.ID); err == nil {
		for _, r := range rows {
			if r.Outcome == "placed" || r.Outcome == "discarded" {
				resolved[r.Path] = true
			}
		}
	}
	var selected []string
	byAbs := map[string]ports.ManifestFile{}
	for _, f := range m.Files {
		abs := filepath.Clean(filepath.Join(pub.Path, filepath.FromSlash(f.Path)))
		byAbs[abs] = f
		if deliveryImportablePath(item.Kind, f.Path) && !isReleaseSample(f.Path) && !resolved[f.Path] {
			selected = append(selected, abs)
		}
	}
	manual := dl.Intent == GrabIntentManualAdditional
	if approved {
		for i := len(dl.Handoff) - 1; i >= 0; i-- {
			if dl.Handoff[i].Step == stepManualQueued && len(dl.Handoff[i].Paths) > 0 {
				selected, manual = nil, true
				for _, p := range dl.Handoff[i].Paths {
					if _, ok := byAbs[filepath.Clean(p)]; ok {
						selected = append(selected, filepath.Clean(p))
					}
				}
				break
			}
		}
	}
	if len(selected) == 0 {
		if len(resolved) > 0 {
			// Everything importable is already resolved: settle it.
			sidecars, err := s.publicationSidecars(ctx, item, pub, m, ImportResult{})
			if err != nil {
				_ = s.db.SetPublicationState(ctx, pub.ID, "discovered", err.Error())
				return err
			}
			return s.completePublication(ctx, dl, cfg, item, pub, m, nil, ImportResult{}, &precedence{dl: dl, decided: map[string]precedenceVerdict{}}, sidecars)
		}
		s.failPublication(ctx, &dl, pub, "the publication holds no importable media (samples and sidecars only)")
		return ErrNoFilesImported
	}
	ictx, err := s.ensureImportContext(ctx, dl)
	if err != nil {
		return err
	}
	ictx = context.WithValue(ictx, publicationImportKey{}, true)
	// Placements record which Runner generation they came from, so a resumed
	// import reuses only its own publication's committed placements.
	ictx = withAcceptedPublicationGeneration(ictx, pub.Generation)
	// Hash once: the manifest digest is the authority for every file's bytes;
	// the placement proves its copy against it as it writes.
	digests := make(map[string]string, len(m.Files))
	for abs, f := range byAbs {
		if f.SHA256 != "" {
			digests[abs] = f.SHA256
		}
	}
	ictx = withExpectedSourceDigests(ictx, digests)
	if pub.CrossVolume || m.CrossVolume {
		ictx = withCrossVolumePublication(ictx)
	}
	prec := &precedence{dl: dl, decided: map[string]precedenceVerdict{}}
	if !manual {
		ictx = context.WithValue(ictx, precedenceKey{}, prec)
	} else {
		ictx = context.WithValue(ictx, precedenceKey{}, &precedence{dl: dl, decided: map[string]precedenceVerdict{}, recordOnly: true})
		prec = ictx.Value(precedenceKey{}).(*precedence)
	}
	result, importErr := s.importDownloadFiles(ictx, dl, pub.Path, selected, manual)
	if importErr != nil && (errors.Is(importErr, context.Canceled) || errors.Is(importErr, ErrImportControl)) {
		_ = s.db.SetPublicationState(context.WithoutCancel(ctx), pub.ID, "discovered", importErr.Error())
		return importErr
	}
	if importErr != nil && errors.Is(importErr, ErrImportBlocked) {
		// The environment, not the payload: leave it discovered so the poll
		// offers it again; its six-hour deadline is what ends it.
		_ = s.db.SetPublicationState(ctx, pub.ID, "discovered", importErr.Error())
		s.advance(ctx, &dl, dlStateOr(dl, pub, "downloaded"), dl.Progress, importErr.Error(), stepFailed, importErr.Error())
		return importErr
	}
	if importErr != nil && !errors.Is(importErr, ErrNoFilesImported) && len(result.Files) == 0 {
		s.failPublication(ctx, &dl, pub, importErr.Error())
		return importErr
	}
	// Subtitles go in with their video, in this import, before Cinema is
	// told (the notification is written by completePublication). A library
	// that cannot take one leaves the publication discovered; the next poll
	// retries, reusing the committed media placements.
	sidecars, err := s.publicationSidecars(ictx, item, pub, m, result)
	if err != nil {
		_ = s.db.SetPublicationState(context.WithoutCancel(ctx), pub.ID, "discovered", err.Error())
		return err
	}
	return s.completePublication(ictx, dl, cfg, item, pub, m, selected, result, prec, sidecars)
}

// publicationSidecars decides the publication's non-media files: subtitles
// placed beside the video this import (or an earlier attempt) placed, the
// rest discarded. Files an earlier attempt resolved keep their outcome.
func (s *Service) publicationSidecars(ctx context.Context, item domain.MediaItem, pub sqlite.Publication, m ports.Manifest, result ImportResult) (map[string]sidecarDecision, error) {
	done, placed := map[string]bool{}, map[string]string{}
	if rows, err := s.db.ListPublicationFiles(ctx, pub.ID); err == nil {
		for _, r := range rows {
			if r.Outcome == "placed" || r.Outcome == "discarded" {
				done[r.Path] = true
			}
			if r.Outcome == "placed" {
				placed[r.Path] = r.LibraryPath
			}
		}
	} else {
		return nil, err
	}
	fresh := map[string]bool{}
	for _, o := range result.Files {
		if !o.Imported {
			continue
		}
		if rel, err := filepath.Rel(pub.Path, o.SourcePath); err == nil && filepath.IsLocal(rel) {
			placed[filepath.ToSlash(rel)] = o.Path
			fresh[filepath.ToSlash(rel)] = true
		}
	}
	return s.decidePublicationSidecars(ctx, item, pub, m, placed, done, fresh)
}

// dlStateOr keeps a salvage's row state; a completed publication's row uses
// the given state.
func dlStateOr(dl sqlite.Download, pub sqlite.Publication, state string) string {
	if pub.Kind != "completed" {
		return dl.State
	}
	return state
}

// failPublication ends a publication as import_failed: a Resolve action,
// never a re-grab trigger.
func (s *Service) failPublication(ctx context.Context, dl *sqlite.Download, pub sqlite.Publication, reason string) {
	_ = s.db.SetPublicationState(ctx, pub.ID, "import_failed", reason)
	if pub.Kind == "completed" {
		s.failImport(ctx, dl, "publication import_failed: "+reason)
		return
	}
	_ = s.db.AddHistory(ctx, "import_failed", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"reason": reason, "publication": pub.Path})
}

// completePublication turns the importer's per-file result into the
// publication's per-file outcomes and records them, the publication's state,
// the row's advance, the history row and the notification in one write.
func (s *Service) completePublication(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, item domain.MediaItem,
	pub sqlite.Publication, m ports.Manifest, selected []string, result ImportResult, prec *precedence, sidecars map[string]sidecarDecision) error {
	outcomes := map[string]FileOutcome{}
	for _, f := range result.Files {
		outcomes[filepath.Clean(f.SourcePath)] = f
	}
	// A file this attempt did not select keeps the outcome an earlier
	// attempt (or a person in Resolve) already recorded for it.
	prior := map[string]sqlite.PublicationFile{}
	if rows, err := s.db.ListPublicationFiles(ctx, pub.ID); err == nil {
		for _, r := range rows {
			prior[r.Path] = r
		}
	}
	selectedSet := map[string]bool{}
	for _, p := range selected {
		selectedSet[p] = true
	}
	var files []sqlite.PublicationFile
	var supersede []sqlite.SupersedeWrite
	placed, unresolved := 0, 0
	var reasons []string
	for _, f := range m.Files {
		abs := filepath.Clean(filepath.Join(pub.Path, filepath.FromSlash(f.Path)))
		pf := sqlite.PublicationFile{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256}
		verdict := prec.decided[abs]
		pf.WantableID = verdict.wantable
		o, attempted := outcomes[abs]
		before, known := prior[f.Path]
		side, isSidecar := sidecars[f.Path]
		switch {
		case !selectedSet[abs] && isSidecar:
			// Decided by this import: a subtitle placed with its video, or a
			// file Curator does not keep (never left for Resolve).
			pf.Outcome, pf.DiscardReason, pf.Reason, pf.LibraryPath = side.outcome, side.discardReason, side.reason, side.libraryPath
			if side.outcome == "placed" {
				pf.PlacedSHA256 = f.SHA256
			}
		case !selectedSet[abs] && known && before.Outcome != "pending":
			pf = before
			if pf.Outcome == "unresolved" && deliveryImportablePath(item.Kind, f.Path) && !isReleaseSample(f.Path) {
				unresolved++
			}
		case !selectedSet[abs]:
			switch {
			case isReleaseSample(f.Path):
				pf.Outcome, pf.DiscardReason, pf.Reason = "discarded", "rejected", "release sample"
			case deliveryImportablePath(item.Kind, f.Path):
				pf.Outcome, pf.Reason = "unresolved", "not selected for import; place or discard it in Resolve"
			default:
				pf.Outcome, pf.DiscardReason, pf.Reason = "discarded", "rejected", "not media of this item's kind; not kept"
			}
		case !attempted:
			pf.Outcome, pf.Reason = "unresolved", "the importer did not reach this file"
			unresolved++
		case o.Imported:
			pf.Outcome, pf.LibraryPath, pf.PlacedSHA256 = "placed", o.Path, f.SHA256
			placed++
			if verdict.wantable != "" {
				supersede = append(supersede, sqlite.SupersedeWrite{WantableID: verdict.wantable, LibraryPath: o.Path})
			}
		case verdict.kind == "superseded":
			pf.Outcome, pf.DiscardReason, pf.Reason = "discarded", "superseded", verdict.reason
		case verdict.kind == "unresolved":
			pf.Outcome, pf.Reason = "unresolved", verdict.reason
			unresolved++
		case o.Retryable:
			pf.Outcome, pf.Reason = "unresolved", o.Reason
			unresolved++
		case duplicateRefusal(o.Reason):
			pf.Outcome, pf.DiscardReason, pf.Reason = "discarded", "duplicate", o.Reason
		default:
			// The importer could not say where it belongs: an obfuscated name
			// with no episode mapping, an unknown episode, an allowlist miss.
			pf.Outcome, pf.Reason = "unresolved", o.Reason
			unresolved++
		}
		if pf.Outcome == "unresolved" && selectedSet[abs] && len(reasons) < 3 {
			reasons = append(reasons, f.Path+": "+pf.Reason)
		}
		files = append(files, pf)
	}
	w := sqlite.PublicationImportWrite{PublicationID: pub.ID, Files: files, Supersede: supersede, DownloadID: dl.ID}
	now := time.Now().UnixMilli()
	detail := fmt.Sprintf("imported %d file(s) from publication %s", placed, filepath.Base(pub.Path))
	if skipped := result.Skipped(); len(skipped) > 0 {
		detail += fmt.Sprintf("; %d not placed — %s", len(skipped), result.reasons(3))
	}
	if unresolved > 0 {
		w.State = "import_failed"
		w.Error = fmt.Sprintf("%d file(s) unresolved: %s", unresolved, strings.Join(reasons, "; "))
		if pub.Kind == "completed" {
			w.DownloadState, w.DownloadError = "failed", "publication import_failed: "+w.Error
			w.Handoff = &sqlite.HandoffEntry{Step: stepFailed, At: now, Detail: detail + "; " + w.Error}
		}
		w.History = append(w.History, sqlite.HistoryWrite{Type: "import_failed", MediaItemID: dl.MediaItemID, ReleaseTitle: dl.ReleaseTitle,
			Data: map[string]any{"reason": w.Error, "publication": pub.Path, "placed": placed}})
	} else {
		w.State = "imported"
		if pub.Kind == "completed" {
			w.DownloadState = "imported"
			if placed == 0 {
				detail = "nothing placed: every file is already present at equal or better quality, or superseded"
			}
			w.Handoff = &sqlite.HandoffEntry{Step: stepImported, At: now, Detail: detail}
			w.TimingEnds = append(w.TimingEnds, sqlite.TimingStart{Kind: pipeline.RunnerDoneToImport, Transfer: pipeline.Key(dl.Transfer, dl.ID)})
		}
	}
	var event *ImportCompleted
	if placed > 0 {
		e := importedEvent(item, dl, result)
		e.PublicationID = pub.ID
		event = &e
		w.History = append(w.History, sqlite.HistoryWrite{Type: "imported", MediaItemID: item.ID, ReleaseTitle: dl.ReleaseTitle,
			Data: map[string]any{"files": result.Imported, "upgrade": result.Upgraded, "publication": pub.Path, "kind": pub.Kind}})
		generation := pub.Generation
		if generation == "" {
			generation = fmt.Sprintf("publication-%d", pub.ID)
		}
		outbox, starts, err := s.importOutbox(ctx, e, generation)
		if err != nil {
			return err
		}
		w.Outbox, w.TimingStarts = outbox, starts
	}
	if err := s.db.CompletePublicationImport(ctx, w); err != nil {
		return err
	}
	if w.State == "imported" {
		// Only a completed publication decides the row: a salvage imports
		// under a row that stays failed (contract section 3.1).
		closeAttempt := func() error { return s.completeBoundImport(ctx, detail) }
		if pub.Kind != "completed" {
			closeAttempt = func() error { return s.settleBoundImport(ctx) }
		}
		if err := closeAttempt(); err != nil {
			s.log.Warn("import attempt could not be closed", "download", dl.ID, "err", err)
		}
		// Every file has an outcome: the evidenced ones can leave now
		// (contract section 3.4), the rest stay for Resolve.
		s.afterPublicationImported(ctx, pub.ID)
	}
	if event != nil {
		s.publish(*event)
		s.InvalidateWanted()
	}
	if w.State == "import_failed" {
		s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: w.Error})
	}
	s.log.Info("publication imported", "publication", pub.ID, "kind", pub.Kind, "placed", placed, "unresolved", unresolved, "state", w.State)
	return nil
}

// duplicateRefusal reports whether the importer declined a file because the
// library already holds as good or better: discarded{duplicate}.
func duplicateRefusal(reason string) bool {
	return strings.Contains(reason, "does not improve") || strings.Contains(reason, "protected by current policy") ||
		strings.Contains(reason, "already present")
}

// importOutbox builds every notification an import sends, as outbox rows the
// caller writes in the import's own transaction (contract section 4.1): there
// is no bus hop to drop one.
//
// A notifier whose message is the work (Cinema) gets one sub-delivery per
// directory, keyed <transfer>/<generation>/<dir-hash> and carrying the media
// files placed under that directory; each is delivered, retried and read back
// on its own. Every other notifier gets one row, attempted once. The
// import_to_cinema_visible timing starts here when there is a sub-delivery to
// end it.
func (s *Service) importOutbox(ctx context.Context, e ImportCompleted, generation string) ([]sqlite.OutboxWrite, []sqlite.TimingStart, error) {
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return nil, nil, err
	}
	return ImportOutbox(notifiers, e, generation)
}

// ImportOutbox is importOutbox over a given notifier list: the rows an
// import with this generation writes.
func ImportOutbox(notifiers []ports.NotifierConfig, e ImportCompleted, generation string) ([]sqlite.OutboxWrite, []sqlite.TimingStart, error) {
	whole := ImportNotification(e)
	wholePayload, err := json.Marshal(whole)
	if err != nil {
		return nil, nil, err
	}
	transfer := pipeline.Key(e.Transfer, e.DownloadID)
	var out []sqlite.OutboxWrite
	subs := false
	for _, cfg := range notifiers {
		if !cfg.Enabled || !cfg.OnImport {
			continue
		}
		if !ports.QueuedNotifier(cfg) {
			out = append(out, sqlite.OutboxWrite{NotifierID: cfg.ID, DownloadID: e.DownloadID, PublicationID: e.PublicationID,
				Event: "import", Payload: string(wholePayload)})
			continue
		}
		for _, dir := range e.Dirs {
			expected := CapExpected(e.Expected[dir])
			n := subDeliveryNotification(whole, dir, expected)
			payload, err := json.Marshal(n)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, sqlite.OutboxWrite{NotifierID: cfg.ID, DownloadID: e.DownloadID, PublicationID: e.PublicationID,
				Directory: dir, IdempotencyKey: SubDeliveryKey(transfer, generation, dir), ExpectedFiles: expected,
				Event: "import", Payload: string(payload)})
			subs = true
		}
	}
	var starts []sqlite.TimingStart
	if subs && e.DownloadID != 0 {
		starts = append(starts, sqlite.TimingStart{Kind: pipeline.ImportToCinemaVisible, Transfer: transfer, DownloadID: e.DownloadID})
	}
	return out, starts, nil
}

// subDeliveryNotification narrows an import's notification to one directory.
func subDeliveryNotification(whole ports.Notification, dir string, expected []ports.ExpectedFile) ports.Notification {
	n := whole
	info := *whole.Import
	info.Dirs = []string{dir}
	info.Paths = nil
	for _, p := range whole.Import.Paths {
		if filepath.Dir(p) == dir {
			info.Paths = append(info.Paths, p)
		}
	}
	info.ExpectedFiles = nil
	if len(expected) > 0 {
		info.ExpectedFiles = map[string][]ports.ExpectedFile{dir: expected}
	}
	n.Import = &info
	return n
}

// CapExpected keeps the expected files Cinema will accept in one request:
// at most ports.MaxExpectedFiles, within ports.MaxExpectedFileBytes as JSON.
// Cinema refuses a request over either with a 400, which retrying cannot fix,
// so a directory never scanned would be the price of naming every file. Over
// the limit, the largest files win (the episodes, not the extras), then the
// newest; the delivery log says how many of the directory's files that
// verified. Order is by path, as the files were given.
func CapExpected(files []ports.ExpectedFile) []ports.ExpectedFile {
	if len(files) <= ports.MaxExpectedFiles && expectedJSONSize(files) <= ports.MaxExpectedFileBytes {
		return files
	}
	ranked := append([]ports.ExpectedFile(nil), files...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Bytes != ranked[j].Bytes {
			return ranked[i].Bytes > ranked[j].Bytes
		}
		if ranked[i].MtimeNs != ranked[j].MtimeNs {
			return ranked[i].MtimeNs > ranked[j].MtimeNs
		}
		return ranked[i].Path < ranked[j].Path
	})
	var out []ports.ExpectedFile
	size := 2 // the brackets
	for _, f := range ranked {
		if len(out) == ports.MaxExpectedFiles {
			break
		}
		n := expectedJSONSize([]ports.ExpectedFile{f}) - 2
		if len(out) > 0 {
			n++ // the comma
		}
		if size+n > ports.MaxExpectedFileBytes {
			continue
		}
		out, size = append(out, f), size+n
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func expectedJSONSize(files []ports.ExpectedFile) int {
	if files == nil {
		files = []ports.ExpectedFile{}
	}
	raw, _ := json.Marshal(files)
	return len(raw)
}

// SubDeliveryKey is a sub-delivery's idempotency key (contract section 4.1):
// <transfer>/<generation>/<first 16 hex of sha256(directory)>. Cinema sees it
// with the attempt appended.
func SubDeliveryKey(transfer, generation, dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("%s/%s/%s", transfer, generation, hex.EncodeToString(sum[:])[:16])
}

// legacyGeneration names an import that did not come from a publication: a
// legacy client's payload or a manual import has no generation, and the same
// row can be imported twice, so each import gets its own.
func legacyGeneration() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "import-" + hex.EncodeToString(b[:])
}

// ImportNotification is the notification an import sends: one shape for the
// dispatcher's bus path and for the outbox written with the import.
func ImportNotification(e ImportCompleted) ports.Notification {
	var book *ports.BookImportInfo
	if e.MediaItemKind == "book" && e.BookMedium != "" && e.BookWorkID != "" && e.BookEditionID != "" {
		book = &ports.BookImportInfo{
			Title: e.BookTitle, Author: e.BookAuthor, Medium: e.BookMedium,
			WorkID: e.BookWorkID, EditionID: e.BookEditionID, CoverURL: e.BookCoverURL,
		}
	}
	return ports.Notification{
		Event: "import", Title: "Import completed", Body: e.Release,
		Fields: map[string]string{
			"files":   fmt.Sprintf("%d", e.Files),
			"upgrade": fmt.Sprintf("%v", e.Upgrade),
		},
		Import: &ports.ImportInfo{
			MediaItemID: e.MediaItemID, DownloadID: e.DownloadID, Paths: e.Paths, Dirs: e.Dirs,
			Kind: e.MediaItemKind, Title: e.Title, Book: book, TmdbID: e.TmdbID, ImdbID: e.ImdbID, Transfer: e.Transfer,
			PublicationID: e.PublicationID, ExpectedFiles: e.Expected,
		},
	}
}

// ---- precedence: the destination-conflict policy (contract section 3.4) ----

// precedenceKey carries the policy into importDownloadFiles for publication
// imports. Other imports (legacy clients, manual imports) carry none.
type precedenceKey struct{}

type precedence struct {
	dl         sqlite.Download
	recordOnly bool // manual: record the wantable, decide nothing
	mu         sync.Mutex
	decided    map[string]precedenceVerdict
}

type precedenceVerdict struct {
	wantable string
	kind     string // "" (current) | superseded | unresolved
	reason   string
}

// precedenceRefusal declines a file that is not the current attempt's.
type precedenceRefusal struct{ verdict precedenceVerdict }

func (e precedenceRefusal) Error() string { return e.verdict.reason }

// checkPrecedence decides, before a file is placed, whether it is the
// current attempt's for its wantable — by Curator's own acquisition record,
// never by generation ids or publication time. A file from another attempt
// is discarded{superseded} when the library already holds the current
// attempt's file, else unresolved; no record at all is unresolved.
func (s *Service) checkPrecedence(ctx context.Context, item domain.MediaItem, copyID int64, src string, season int, episodes []int, episodeIDs []int64) error {
	p, ok := ctx.Value(precedenceKey{}).(*precedence)
	if !ok || p == nil {
		return nil
	}
	keys, primary := wantableKeys(item, copyID, season, episodes)
	verdict := precedenceVerdict{wantable: primary}
	defer func() {
		p.mu.Lock()
		p.decided[filepath.Clean(src)] = verdict
		p.mu.Unlock()
	}()
	if p.recordOnly {
		return nil
	}
	attempts, err := s.db.WantableAttempts(ctx, keys)
	if err != nil {
		return err
	}
	var cur sqlite.WantableAttempt
	for _, k := range keys {
		a, ok := attempts[k]
		if ok && (cur.DownloadID == 0 || a.DecidedAt.After(cur.DecidedAt) || (a.DecidedAt.Equal(cur.DecidedAt) && a.DownloadID > cur.DownloadID)) {
			cur = a
		}
	}
	switch cur.DownloadID {
	case p.dl.ID:
		return nil
	case 0:
		verdict.kind, verdict.reason = "unresolved", "no acquisition record for "+primary
	default:
		other, err := s.db.GetDownload(ctx, cur.DownloadID)
		holds := false
		if err == nil {
			holds, err = s.db.LibraryHoldsDownloadFile(ctx, item.ID, copyID, episodeIDs, other.GenerationKey)
			if err != nil {
				return err
			}
		}
		if holds {
			verdict.kind, verdict.reason = "superseded", fmt.Sprintf("superseded by download %d, whose file is in the library", cur.DownloadID)
		} else {
			verdict.kind, verdict.reason = "unresolved", fmt.Sprintf("superseded by download %d which has not imported yet", cur.DownloadID)
		}
	}
	return importPolicyRefusal{precedenceRefusal{verdict}}
}

// wantableKeys are the wantable ids a file answers to, most specific first:
// the episode, then its season pack; the movie or book. primary names the
// file's own wantable.
func wantableKeys(item domain.MediaItem, copyID int64, season int, episodes []int) ([]string, string) {
	suffix := ""
	if copyID != 0 {
		suffix = fmt.Sprintf(":c%d", copyID)
	}
	switch item.Kind {
	case domain.KindMovie:
		k := fmt.Sprintf("movie:%d%s", item.ID, suffix)
		return []string{k}, k
	case domain.KindBook:
		k := fmt.Sprintf("book:%d%s", item.ID, suffix)
		return []string{k}, k
	}
	var keys []string
	for _, e := range episodes {
		keys = append(keys, fmt.Sprintf("episode:%d:%d:%d%s", item.ID, season, e, suffix))
	}
	primary := ""
	if len(keys) > 0 {
		primary = keys[0]
	}
	keys = append(keys, fmt.Sprintf("season:%d:%d%s", item.ID, season, suffix))
	return keys, primary
}

// ---- the historical cohort (contract section 3.4) --------------------------

// historicalMinAge keeps a directory Runner is publishing right now out of
// the backlog.
const historicalMinAge = 10 * time.Minute

// historicalNoManifestReason explains a frozen directory with no manifest and
// no download row (the same text migration 0073 writes).
const historicalNoManifestReason = "frozen at the pipeline v2 cutover with no manifest and no download row: Curator cannot import or clean it file by file; the M7 review decides it, and once approved it can be deleted whole from Storage"

// MarkHistoricalCohort records, once, at the first M2 start, every directory
// under the completed roots that no publication claims, as a historical
// publication no automatic deletion touches until the M7 review approves it.
// It returns how many it recorded; a second call does nothing.
func (s *Service) MarkHistoricalCohort(ctx context.Context) (int, error) {
	if v, err := s.db.GetMeta(ctx, sqlite.PipelineCutoverSetting); err == nil && v != "" {
		return 0, nil
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return 0, err
	}
	claimed, err := s.db.ExistingPublicationPaths(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			s.log.Warn("historical cohort: completed root unreadable; it is not marked", "root", root, "err", err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root, e.Name())
			if claimed[path] {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < historicalMinAge {
				continue
			}
			id, err := s.db.InsertPublication(ctx, sqlite.PublicationWrite{Kind: "completed", Path: path, Cohort: "historical", Source: "none"})
			if err != nil {
				return n, err
			}
			// No manifest and no row: nothing can import it, so it is not
			// "discovered" (M4 review finding 3); the M7 review decides it.
			if err := s.db.SetPublicationState(ctx, id, "import_failed", historicalNoManifestReason); err != nil {
				return n, err
			}
			n++
		}
	}
	if err := s.db.SetMeta(ctx, sqlite.PipelineCutoverSetting, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return n, err
	}
	total, _ := s.db.CountHistoricalPublications(ctx)
	s.log.Info("pipeline v2 cutover: historical cohort frozen", "directories_marked", n, "historical_publications", total)
	return n, nil
}

// publicationManaged reports whether a row or a path belongs to a
// publication. Whole-directory legacy cleanup never touches those: a
// publication's files leave only by their recorded per-file outcome
// (contract section 3.4; M4), and the historical cohort not at all until
// approved.
func (s *Service) publicationManaged(ctx context.Context, downloadID int64, path string) bool {
	if downloadID > 0 {
		pubs, err := s.db.ListPublicationsForDownload(ctx, downloadID)
		if err != nil || len(pubs) > 0 {
			return true
		}
	}
	if path != "" {
		_, ok, err := s.publicationClaiming(ctx, path)
		if err != nil || ok {
			return true
		}
	}
	return false
}

// publicationClaiming finds the publication that claims path: its own
// directory, a directory containing it, or one inside it (compared on
// storage paths, so aliases of the completed roots match).
func (s *Service) publicationClaiming(ctx context.Context, path string) (sqlite.Publication, bool, error) {
	path = filepath.Clean(path)
	if p, ok, err := s.db.PublicationByPath(ctx, path); err != nil || (ok && p.State != "removed") {
		return p, ok, err
	}
	pubs, err := s.db.ListPublicationPaths(ctx)
	if err != nil {
		return sqlite.Publication{}, false, err
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return sqlite.Publication{}, false, err
	}
	target := storagePath(path, roots)
	for _, p := range pubs {
		dir := storagePath(filepath.Clean(p.Path), roots)
		if dir == target || within(dir, target) || within(target, dir) {
			return p, true, nil
		}
	}
	return sqlite.Publication{}, false, nil
}

// ResolvePublicationFile is a person's decision on one unresolved file:
// place it (import exactly that file), discard it (discarded{rejected}; the
// bytes leave only through the per-file cleanup), or keep it as it is.
func (s *Service) ResolvePublicationFile(ctx context.Context, downloadID, publicationID int64, relPath, action string) error {
	pub, err := s.db.GetPublication(ctx, publicationID)
	if err != nil {
		return err
	}
	if pub.DownloadID != downloadID {
		return ErrNotFound
	}
	files, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	var file *sqlite.PublicationFile
	for i := range files {
		if files[i].Path == relPath {
			file = &files[i]
		}
	}
	if file == nil || (file.Outcome != "unresolved" && file.Outcome != "pending") {
		return fmt.Errorf("%w: %q is not an unresolved file of this publication", ErrInvalidResolution, relPath)
	}
	unlock := s.lockDownload(downloadID)
	defer unlock()
	dl, err := s.db.GetDownload(ctx, downloadID)
	if err != nil {
		return err
	}
	switch action {
	case "place":
		item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
		if err != nil {
			return err
		}
		if !deliveryImportablePath(item.Kind, relPath) {
			return fmt.Errorf("%w: only media of this item's kind can be placed; keep or discard %q", ErrInvalidResolution, relPath)
		}
		abs := filepath.Clean(filepath.Join(pub.Path, filepath.FromSlash(relPath)))
		dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepManualQueued, At: time.Now().UnixMilli(),
			Detail: "operator placed " + relPath + " from Resolve", Paths: []string{abs}})
		if err := s.db.UpdateDownloadHandoff(ctx, dl); err != nil {
			return err
		}
		if err := s.db.SetPublicationState(ctx, pub.ID, "discovered", ""); err != nil {
			return err
		}
		var cfg ports.ClientConfig
		if dl.ClientID != 0 {
			if cfg, err = s.db.GetDownloadClient(ctx, dl.ClientID); err != nil {
				return err
			}
		}
		if !s.enqueuePublicationImportApproved(ctx, dl, cfg, pub.ID, true) {
			return fmt.Errorf("import worker is unavailable or full; retry")
		}
		return nil
	case "discard":
		file.Outcome, file.DiscardReason, file.Reason = "discarded", "rejected", "discarded by an operator in Resolve"
	case "keep":
		file.Reason = "kept by an operator in Resolve"
	default:
		return fmt.Errorf("%w: unknown action %q", ErrInvalidResolution, action)
	}
	if err := s.db.SetPublicationFileOutcome(ctx, *file); err != nil {
		return err
	}
	_ = s.db.AddHistory(ctx, "publication_file_resolved", dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"publication": pub.Path, "file": relPath, "action": action})
	if err := s.settleResolvedPublication(ctx, dl, pub); err != nil {
		return err
	}
	if action == "discard" {
		// The discarded file leaves through the per-file cleanup, with its
		// checks; a historical publication waits for its review batch. Say
		// so when it stayed: "discarded" must not read as "deleted".
		res, err := s.cleanupPublicationLocked(withOperatorCleanup(ctx, "operator discard in Resolve"), pub.ID)
		if err != nil {
			return fmt.Errorf("%w; the file stays for now: %w", ErrDiscardedButKept, err)
		}
		for _, k := range res.Kept {
			if strings.HasPrefix(k, relPath+": ") {
				return fmt.Errorf("%w: %s", ErrDiscardedButKept, k)
			}
		}
		if res.Skipped != "" {
			return fmt.Errorf("%w; deletion waits: %s", ErrDiscardedButKept, res.Skipped)
		}
	}
	return nil
}

// ErrDiscardedButKept says a Discard was recorded but the bytes stayed (the
// per-file evidence did not hold, or deletion has to wait): the file is not
// deleted, and the reason says why.
var ErrDiscardedButKept = errors.New("discarded, but kept")

// ErrInvalidResolution refuses a Resolve decision that does not apply.
var ErrInvalidResolution = errors.New("invalid resolution")

// settleResolvedPublication marks an import_failed publication imported once
// no media file of it is left unresolved, and its completed row with it.
func (s *Service) settleResolvedPublication(ctx context.Context, dl sqlite.Download, pub sqlite.Publication) error {
	if pub.State != "import_failed" {
		return nil
	}
	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return err
	}
	files, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	for _, f := range files {
		if (f.Outcome == "unresolved" && f.Reason != "kept by an operator in Resolve" || f.Outcome == "pending") &&
			deliveryImportablePath(item.Kind, f.Path) && !isReleaseSample(f.Path) {
			return nil
		}
	}
	if err := s.db.SetPublicationState(ctx, pub.ID, "imported", ""); err != nil {
		return err
	}
	if pub.Kind == "completed" && dl.State == "failed" && dl.RunnerOutcome == ports.OutcomeCompleted {
		s.advance(ctx, &dl, "imported", 1, "", stepImported, "every file of publication "+filepath.Base(pub.Path)+" is resolved")
	}
	return nil
}
