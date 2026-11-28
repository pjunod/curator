package notify

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// refreshScanDelivery keeps a delayed scan from sending an old title's
// identity for files that moved, changed or now belong to a different title.
func (d *Dispatcher) refreshScanDelivery(ctx context.Context, del *sqlite.Delivery, notice *ports.Notification) error {
	obsolete := fmt.Errorf("%w: these files are no longer linked to this title at the observed paths and versions", ports.ErrNotifyPermanent)
	if notice.Import == nil || len(del.ExpectedFiles) == 0 || del.MediaItemID == 0 {
		return fmt.Errorf("%w: scan delivery has no file history", ports.ErrNotifyPermanent)
	}
	item, err := d.db.GetMediaItemFull(ctx, del.MediaItemID)
	if errors.Is(err, sqlite.ErrNotFound) || (err == nil &&
		(string(item.Kind) != notice.Import.Kind || item.IDs.TMDB != notice.Import.TmdbID || item.IDs.IMDB != notice.Import.ImdbID)) {
		return obsolete
	}
	if err != nil {
		return err
	}
	folders := map[int64]string{0: item.Path}
	for _, copy := range item.Copies {
		folders[copy.ID] = copy.Path
		if copy.Path == "" {
			folders[copy.ID] = item.Path
		}
	}
	sizes := make(map[string]int64)
	for _, file := range item.Files {
		folder := folders[file.CopyID]
		if folder == "" {
			continue
		}
		if rel, err := filepath.Rel(folder, file.Path); err == nil && filepath.IsLocal(rel) && rel != "." {
			sizes[file.Path] = file.Size
		}
	}

	var current []ports.ExpectedFile
	var paths []string
	var unavailable error
	for _, file := range del.ExpectedFiles {
		if size, ok := sizes[file.Path]; !ok || size != file.Bytes {
			continue
		}
		info, err := os.Lstat(file.Path)
		if err != nil {
			// The path is still linked. Retry during a mount outage, and let
			// a later library scan decide when the record can be pruned.
			unavailable = err
			continue
		}
		if !info.Mode().IsRegular() || info.Size() != file.Bytes || info.ModTime().UnixNano() != file.MtimeNs {
			continue
		}
		current = append(current, file)
		paths = append(paths, file.Path)
	}
	if len(current) == 0 {
		if unavailable != nil {
			return unavailable
		}
		return obsolete
	}
	if len(current) != len(del.ExpectedFiles) {
		if unavailable != nil {
			// Do not settle only the visible subset if a still-linked file
			// could become readable again after an outage.
			return unavailable
		}
		// A partially obsolete batch has a different request shape. Use
		// a distinct key while retaining retry idempotency for that subset.
		raw, err := json.Marshal(current)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		notice.IdempotencyKey = fmt.Sprintf("%s-%x", del.RequestKey(), sum[:8])
		del.ExpectedFiles = current
		notice.Import.ExpectedFiles = map[string][]ports.ExpectedFile{del.Directory: current}
		notice.Import.Paths = paths
	}
	return nil
}
