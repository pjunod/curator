package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type activityDispositionContext struct{}

// reconcileDispositionPublication uses the same import completion transaction
// as a normal/partly successful import. A manifest can arrive before any file
// rows exist, or placement can commit just before cancellation. Both must be
// accounted for before cleanup; an installed file still gets its Cinema outbox.
func (s *Service) reconcileDispositionPublication(ctx context.Context, dl sqlite.Download, pub sqlite.Publication) error {
	m, ok, err := s.publicationManifest(pub)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the publication manifest is unavailable; reconnect its original storage and retry")
	}
	// An old digestless row must not acquire new deletion authority from a
	// live marker observed after the user requested deletion.
	previous, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	for _, before := range previous {
		if before.SHA256 != "" || !before.RemovedAt.IsZero() {
			continue
		}
		for _, live := range m.Files {
			if live.Path == before.Path && live.SHA256 != "" && !recordedPublicationFileMatches(pub, live) {
				return fmt.Errorf("this legacy file has no recorded digest or object identity; its ownership cannot be verified for deletion")
			}
		}
	}
	if err = s.recordFileManifest(ctx, pub, m); err != nil {
		return err
	}
	return s.reconcileDispositionManifest(ctx, dl, pub, m)
}

func (s *Service) finishAbsentDispositionPublication(ctx context.Context, dl sqlite.Download, pub sqlite.Publication) error {
	// The caller has already verified the mount. Sync the observed absence
	// before settling any file outcomes against the missing publication.
	if err := syncRemovedPublicationParent(pub.Path); err != nil {
		return err
	}
	if pub.State == "discovered" || pub.State == "importing" {
		if pub.ManifestSource == "file" || pub.ManifestSource == "v1_record" {
			var manifest ports.Manifest
			if err := json.Unmarshal([]byte(pub.ManifestJSON), &manifest); err != nil {
				return fmt.Errorf("the recorded publication manifest is invalid: %w", err)
			}
			manifest.Source = pub.ManifestSource
			if err := verifyManifestShape(manifest, pub.Generation); err != nil {
				return fmt.Errorf("the recorded publication manifest is invalid: %w", err)
			}
			if err := s.reconcileDispositionManifest(ctx, dl, pub, manifest); err != nil {
				return err
			}
		} else {
			// Without the manifest, a placement may still need its file outcome
			// and notification. Preserve that work for a retry with the manifest.
			var hasPlacement bool
			if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM import_placements WHERE state IN ('committed','cleaned')
				AND json_extract(data,'$.download_id')=? AND json_extract(data,'$.source_generation')=?
				AND (json_extract(data,'$.source_payload_generation')=?
					OR (COALESCE(json_extract(data,'$.source_payload_generation'),'')=''
						AND instr(json_extract(data,'$.source'),?)=1)))`,
				dl.ID, dl.GenerationKey, pub.Generation, filepath.Clean(pub.Path)+string(filepath.Separator)).Scan(&hasPlacement); err != nil {
				return err
			}
			if hasPlacement {
				return fmt.Errorf("the recorded publication manifest is needed to finish accounting for installed Library files")
			}
		}
	}
	result, err := s.db.W.ExecContext(ctx, `UPDATE publications SET
		state=CASE WHEN state IN ('discovered','importing') THEN 'import_failed' ELSE state END,
		cleanup_approved_at=CASE WHEN cohort='historical' AND cleanup_approved_at=0 THEN ? ELSE cleanup_approved_at END,
		cleanup_paused_at=0 WHERE id=? AND download_id=? AND generation=?`,
		time.Now().UnixMilli(), pub.ID, dl.ID, pub.Generation)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("publication identity changed before cleanup")
	}
	cleanupCtx := context.WithValue(withOperatorCleanup(ctx, "verifying prior job deletion"), activityDispositionContext{}, true)
	cleanup, err := s.cleanupPublicationLocked(cleanupCtx, pub.ID)
	if err != nil {
		return err
	}
	if cleanup.Skipped != "" && cleanup.Skipped != "already removed" {
		return fmt.Errorf("cleanup stopped: %s", cleanup.Skipped)
	}
	if len(cleanup.Kept) > 0 {
		return fmt.Errorf("cleanup could not verify every job file: %s", cleanup.Kept[0])
	}
	return nil
}

func (s *Service) reconcileDispositionManifest(ctx context.Context, dl sqlite.Download, pub sqlite.Publication, m ports.Manifest) error {
	files := make([]sqlite.PublicationFile, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, sqlite.PublicationFile{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256})
	}
	if err := s.db.SeedPublicationFiles(ctx, pub.ID, files); err != nil {
		return err
	}
	previous, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	resolved := make(map[string]sqlite.PublicationFile, len(previous))
	for _, f := range previous {
		if f.Outcome == "placed" || f.Outcome == "discarded" {
			resolved[f.Path] = f
		}
	}
	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return err
	}
	var result ImportResult
	var selected []string
	for _, f := range m.Files {
		// The aggregate already recorded these outcomes and their outbox.
		// A later Offload or replacement does not make them uncommitted.
		source := filepath.Clean(filepath.Join(pub.Path, filepath.FromSlash(f.Path)))
		if before, settled := resolved[f.Path]; settled {
			if before.Outcome == "placed" && deliveryImportablePath(item.Kind, f.Path) && !isReleaseSample(f.Path) {
				superseded, _, _, err := s.placedLibraryState(ctx, before, nil)
				if err != nil {
					return err
				}
				if !superseded {
					// Include still-current prior placements in the notification,
					// while leaving their already committed file outcomes alone.
					result.Files = append(result.Files, FileOutcome{Name: filepath.Base(source), SourcePath: source, Imported: true, Path: before.LibraryPath})
					result.Imported++
				}
			}
			continue
		}
		if !deliveryImportablePath(item.Kind, f.Path) || isReleaseSample(f.Path) {
			continue
		}
		selected = append(selected, source)
		placements, err := s.db.CommittedImportPlacements(ctx, dl.ID, dl.GenerationKey, source)
		if err != nil {
			return err
		}
		for _, p := range placements {
			if p.SourcePayloadGeneration != pub.Generation {
				continue
			}
			intact, err := committedPlacementIntact(ctx, p)
			if err != nil {
				return err
			}
			if !intact {
				return fmt.Errorf("an installed Library file changed; restore its original storage before deleting the only download copy")
			}
			result.Files = append(result.Files, FileOutcome{Name: filepath.Base(source), SourcePath: source, Imported: true, Path: p.Target, PlacementID: p.ID})
			result.Imported++
			break
		}
	}
	if _, err = s.db.W.ExecContext(ctx, `UPDATE publications SET state='importing' WHERE id=? AND generation=? AND state!='removed'`, pub.ID, pub.Generation); err != nil {
		return err
	}
	// The normal coordinator preserves already recorded sidecars. It never
	// starts a new sidecar writer in this explicit disposition context.
	return s.completePublication(context.WithValue(ctx, activityDispositionContext{}, true), dl, ports.ClientConfig{}, item, pub, m, selected, result, &precedence{dl: dl, decided: map[string]precedenceVerdict{}}, nil, "deleted by operator")
}

// Final file outcomes and the Cinema outbox are committed together. Only
// unfinished outcomes need reconstruction; the cleaner separately checks the
// current Library state and every owned source before unlinking it.
func publicationNeedsResolution(pub sqlite.Publication, files []sqlite.PublicationFile) bool {
	if len(files) == 0 || (pub.State != "imported" && pub.State != "notified" && pub.State != "import_failed") {
		return true
	}
	for _, f := range files {
		if f.RemovedAt.IsZero() && f.Outcome != "placed" && f.Outcome != "discarded" {
			return true
		}
	}
	return false
}
