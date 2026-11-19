package acquisition

import (
	"context"
	"fmt"
	"path/filepath"

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
	if err = s.recordFileManifest(ctx, pub, m); err != nil {
		return err
	}
	files := make([]sqlite.PublicationFile, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, sqlite.PublicationFile{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256})
	}
	if err = s.db.SeedPublicationFiles(ctx, pub.ID, files); err != nil {
		return err
	}
	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return err
	}
	var result ImportResult
	var selected []string
	for _, f := range m.Files {
		source := filepath.Clean(filepath.Join(pub.Path, filepath.FromSlash(f.Path)))
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
