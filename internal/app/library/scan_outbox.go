package library

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// scanOutbox prepares Cinema-only requests for the new paths from the same
// snapshot that ApplyLibraryScan validates. A sweep of unchanged files needs
// no delivery. Each request scans one directory under one title's identity.
func (s *Service) scanOutbox(ctx context.Context, item domain.MediaItem, newFiles map[string]ports.ExpectedFile) ([]sqlite.OutboxWrite, error) {
	if len(newFiles) == 0 {
		return nil, nil
	}
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return nil, err
	}
	var targets []int64
	for _, cfg := range notifiers {
		if cfg.Enabled && cfg.OnImport && ports.QueuedNotifier(cfg) {
			targets = append(targets, cfg.ID)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}

	dirs := make(map[string][]ports.ExpectedFile)
	for _, path := range slices.Sorted(maps.Keys(newFiles)) {
		dir := filepath.Dir(path)
		dirs[dir] = append(dirs[dir], newFiles[path])
	}
	transfer := "library-scan-" + strconv.FormatInt(item.ID, 10)
	// A file that leaves and later returns, with the same path and bytes,
	// still needs a fresh request. Retries of this row keep the stored key.
	generation := rand.Text()
	var outbox []sqlite.OutboxWrite
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		for number, files := range s.scanBatches(dirs[dir]) {
			paths := make([]string, 0, len(files))
			for _, file := range files {
				paths = append(paths, file.Path)
			}
			// Book directories can mix editions. Tell Cinema the kind and
			// let it identify each edition from its own file.
			info := ports.ImportInfo{
				MediaItemID: item.ID, Paths: paths, Dirs: []string{dir},
				Kind: string(item.Kind), Title: item.Title, TmdbID: item.IDs.TMDB,
				ImdbID: item.IDs.IMDB, Transfer: transfer,
				ExpectedFiles: map[string][]ports.ExpectedFile{dir: files},
			}
			payload, err := json.Marshal(ports.Notification{
				Event: ports.ScanLinkedEvent, Title: "Library files linked", Body: item.Title, Import: &info,
			})
			if err != nil {
				return nil, err
			}
			key := ports.ScanSubDeliveryKey(transfer, fmt.Sprintf("%s-%d", generation, number), dir)
			for _, id := range targets {
				outbox = append(outbox, sqlite.OutboxWrite{
					NotifierID: id, MediaItemID: item.ID, Directory: dir, IdempotencyKey: key,
					ExpectedFiles: files, Event: ports.ScanLinkedEvent, Payload: string(payload),
				})
			}
		}
	}
	return outbox, nil
}

// Cinema probes the named paths only. Split big directories so every
// representable file is announced under the wire count and byte limits.
func (s *Service) scanBatches(files []ports.ExpectedFile) [][]ports.ExpectedFile {
	var batches [][]ports.ExpectedFile
	var batch []ports.ExpectedFile
	size := 2 // brackets
	for _, file := range files {
		n := ports.ExpectedFilesJSONSize([]ports.ExpectedFile{file}) - 2
		if n+2 > ports.MaxExpectedFileBytes {
			s.log.Warn("scan: path exceeds Cinema's request limit", "path", file.Path)
			continue
		}
		if len(batch) > 0 && (len(batch) == ports.MaxExpectedFiles || size+1+n > ports.MaxExpectedFileBytes) {
			batches = append(batches, batch)
			batch, size = nil, 2
		}
		if len(batch) > 0 {
			size++
		}
		batch = append(batch, file)
		size += n
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}
