package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/filename"
	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/infra/probe"
)

// JobProbe measures one library file (ADR 0013 §4). One job per file, deduped
// on the file id, priority 80 — below scan (50) and adopt (60), because a
// probe backlog must never delay the reconcile that discovers new files.
//
// The backfill for an existing library is not a special mechanism: it is the
// first scan after upgrade enqueueing one of these per file.
const JobProbe = "library.probe"

// FileProbed is published after a file's measurement lands. The acquisition
// service listens for it, because a probe can change whether an item is wanted
// at all — which is the entire point of ADR 0013.
type FileProbed struct {
	FileID      int64  `json:"fileId"`
	MediaItemID int64  `json:"mediaItemId"`
	Path        string `json:"path"`
	Provenance  string `json:"provenance"`
	Quality     string `json:"quality"`
}

// EventType implements bus.Event.
func (FileProbed) EventType() string { return "library.file.probed" }

type probePayload struct {
	FileID int64 `json:"fileId"`
}

// EnqueueProbe asks for one file to be measured. Deduped per file, so a scan
// that runs twice before the queue drains does not double the work.
//
// Without a queue configured the probe runs inline — the same degradation
// EnqueueScan documents, and the reason the queue stays an addition rather
// than a dependency.
func (s *Service) EnqueueProbe(ctx context.Context, fileID int64) error {
	if s.queue == nil {
		return s.ProbeFile(ctx, fileID)
	}
	payload, err := json.Marshal(probePayload{FileID: fileID})
	if err != nil {
		return err
	}
	if _, err := s.queue.EnqueueUnique(ctx, domain.Job{
		Kind:      JobProbe,
		Payload:   string(payload),
		DedupeKey: fmt.Sprintf("probe:%d", fileID),
		Priority:  80,
	}); err != nil {
		return fmt.Errorf("enqueue probe: %w", err)
	}
	return nil
}

// ProbeFile measures one file and records what it found.
//
// It returns nil for everything that is a *result* rather than a failure: a
// vanished row, an unreadable file, a container we do not deep-parse. Those
// are facts about the library, and a corrupt file is not made readable by
// trying again on a timer. Only storage errors come back as errors, because
// those are the ones a retry can fix.
func (s *Service) ProbeFile(ctx context.Context, fileID int64) error {
	rec, err := s.db.GetFileQuality(ctx, fileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // the row went away between enqueue and run
		}
		return err
	}
	// Book files are graded by their extension — the format IS the quality
	// (ADR 0006). There is no container to walk and nothing to learn.
	if !filename.IsVideo(rec.Path) {
		return nil
	}

	now := time.Now()
	info, probeErr := probe.File(rec.Path)

	if !info.Measured() {
		// Unreadable, or a container we do not parse. Record that we tried,
		// so the next scan does not queue the same doomed read again, and
		// leave the file in the "on disk, quality unverified" state — which is
		// emphatically not "missing" (ADR 0013 §5).
		prov := mediainfo.ProvenanceFailed
		if rec.Known {
			prov = mediainfo.ProvenanceFilename // a name told us something
		}
		s.log.Debug("probe: nothing measured", "path", rec.Path, "err", probeErr)
		if err := s.db.SetFileMediaInfo(ctx, fileID, info, prov, mediainfo.ConfidenceNone, now); err != nil {
			return err
		}
		s.publishProbed(rec.FileID, rec.MediaItemID, rec.Path, prov, rec.Quality.String())
		return nil
	}

	hint := parser.Parse(filepath.Base(rec.Path)).Quality
	q, prov, conf := mediainfo.Resolve(info, hint, mediainfo.ProvenanceFilename)
	if err := s.db.SetFileMediaInfo(ctx, fileID, info, prov, conf, now); err != nil {
		return err
	}
	if err := s.db.SetFileQualityFrom(ctx, fileID, q, prov, conf); err != nil {
		return err
	}
	s.log.Debug("probe: measured", "file", filepath.Base(rec.Path),
		"quality", q.String(), "provenance", string(prov), "facts", info.Summary())
	s.publishProbed(rec.FileID, rec.MediaItemID, rec.Path, prov, q.String())
	return nil
}

func (s *Service) publishProbed(fileID, itemID int64, path string, prov mediainfo.Provenance, q string) {
	s.publish(FileProbed{
		FileID: fileID, MediaItemID: itemID, Path: path,
		Provenance: string(prov), Quality: q,
	})
}
