package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
)

// FileQuality is everything the system knows about one library file's quality:
// the canonical (source, resolution) pair, the measured record behind it, and
// the provenance/confidence that say how much to trust the source axis.
//
// Known is deliberately separate from Quality being the zero value. "We looked
// and found nothing" and "we never looked" are different states, and conflating
// them is the bug ADR 0013 exists to fix.
type FileQuality struct {
	FileID      int64
	MediaItemID int64
	CopyID      int64 // 0 = the primary copy
	Path        string
	Size        int64
	Quality     quality.Quality
	Known       bool
	Info        mediainfo.Info
	Probed      bool
	Provenance  mediainfo.Provenance
	Confidence  mediainfo.Confidence
	ProbedAt    time.Time
}

// SourceVerified reports whether this file's SOURCE can be trusted enough to
// justify replacing it (ADR 0013 §5 don't-churn). Resolution is not in
// question — once a probe succeeds it is measured fact.
func (f FileQuality) SourceVerified() bool {
	return f.Provenance.Verified(f.Confidence)
}

// NeedsProbe reports whether this file should be (re)measured: never probed, or
// probed at a different size. Size is the cheap cache key — a file whose bytes
// changed is a different file wearing the same path.
func (f FileQuality) NeedsProbe(sizeOnDisk int64) bool {
	if !f.Probed {
		return true
	}
	return f.Size != sizeOnDisk
}

// CopyDiskState is what one copy of an item actually has on disk.
//
// It replaces BestQualityForItem's single ambiguous ok, which meant BOTH "no
// files" and "files whose quality we could not determine" — and every caller
// read it as the first. That conflation is the whole of Failure B: a 17 GB file
// reported as missing, hunted, replaced, and left behind as a duplicate.
type CopyDiskState struct {
	// HasFiles: files exist for this copy, whatever we know about them.
	HasFiles bool
	// Best is the highest-ranked KNOWN quality among them; nil when files
	// exist but none has a determined quality.
	Best *quality.Quality
	// SourceVerified reports whether Best's source axis is trustworthy. False
	// when the source was inferred at medium/low confidence — at which point
	// a target that is otherwise met counts as met anyway rather than
	// triggering a replacement on a guess.
	SourceVerified bool
}

// FileQualityRecords returns the quality record for every file of an item.
func (d *DB) FileQualityRecords(ctx context.Context, itemID int64) ([]FileQuality, error) {
	rows, err := d.Read.ListFileQualityRecordsForItem(ctx, sql.NullInt64{Int64: itemID, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]FileQuality, 0, len(rows))
	for _, r := range rows {
		fq := FileQuality{
			FileID: r.ID, MediaItemID: itemID, Path: r.Path, Size: r.Size,
			Provenance: mediainfo.Provenance(r.QualityProvenance),
			Confidence: mediainfo.Confidence(r.QualityConfidence),
		}
		if r.CopyID.Valid {
			fq.CopyID = r.CopyID.Int64
		}
		fq.applyQuality(r.Quality, r.MediaInfo, r.ProbedAt)
		out = append(out, fq)
	}
	return out, nil
}

// GetFileQuality returns one file's quality record, or ErrNotFound.
func (d *DB) GetFileQuality(ctx context.Context, fileID int64) (FileQuality, error) {
	r, err := d.Read.GetMediaFile(ctx, fileID)
	if err != nil {
		return FileQuality{}, wrapNotFound(err)
	}
	fq := FileQuality{
		FileID: r.ID, Path: r.Path, Size: r.Size,
		Provenance: mediainfo.Provenance(r.QualityProvenance),
		Confidence: mediainfo.Confidence(r.QualityConfidence),
	}
	if r.MediaItemID.Valid {
		fq.MediaItemID = r.MediaItemID.Int64
	}
	if r.CopyID.Valid {
		fq.CopyID = r.CopyID.Int64
	}
	fq.applyQuality(r.Quality, r.MediaInfo, r.ProbedAt)
	return fq, nil
}

func (f *FileQuality) applyQuality(qualityStr, infoJSON string, probedAt int64) {
	if qualityStr != "" {
		f.Quality, f.Known = quality.FromString(qualityStr), true
	}
	if infoJSON != "" {
		if err := json.Unmarshal([]byte(infoJSON), &f.Info); err == nil {
			f.Probed = true
		}
	}
	if probedAt > 0 {
		f.ProbedAt = time.UnixMilli(probedAt)
		f.Probed = true
	}
}

// DiskStateForItem reports what ONE copy of an item has on disk (copyID 0 =
// primary). Copies never see each other's files — the 4K primary must not
// convince the 720p copy it is satisfied, or the reverse.
func (d *DB) DiskStateForItem(ctx context.Context, itemID, copyID int64) (CopyDiskState, error) {
	records, err := d.FileQualityRecords(ctx, itemID)
	if err != nil {
		return CopyDiskState{}, err
	}
	var state CopyDiskState
	var bestRecord *FileQuality
	for i := range records {
		r := records[i]
		if r.CopyID != copyID {
			continue
		}
		state.HasFiles = true
		if !r.Known {
			continue
		}
		if bestRecord == nil || quality.Better(r.Quality, bestRecord.Quality) {
			rr := r
			bestRecord = &rr
		}
	}
	if bestRecord != nil {
		q := bestRecord.Quality
		state.Best = &q
		state.SourceVerified = bestRecord.SourceVerified()
	}
	return state, nil
}

// EpisodeDiskState is the same idea per episode: which episodes of one copy
// have files, and the best known quality among them.
type EpisodeDiskState struct {
	HasFile        bool
	Best           *quality.Quality
	SourceVerified bool
}

// SetFileMediaInfo records a probe result: the measured record, where the
// quality now comes from, and when the measurement happened.
//
// A failed probe is still a result — provenance `failed`, probed_at set — so
// the next scan does not re-probe an unreadable file forever.
func (d *DB) SetFileMediaInfo(ctx context.Context, fileID int64, info mediainfo.Info,
	prov mediainfo.Provenance, conf mediainfo.Confidence, at time.Time) error {
	raw := ""
	if info.Container != "" {
		if b, err := json.Marshal(info); err == nil {
			raw = string(b)
		}
	}
	if !prov.Valid() {
		prov = mediainfo.ProvenanceUnknown
	}
	if !conf.Valid() {
		conf = mediainfo.ConfidenceNone
	}
	return d.Write.SetFileMediaInfo(ctx, sqlitegen.SetFileMediaInfoParams{
		MediaInfo: raw, QualityProvenance: string(prov),
		QualityConfidence: string(conf), ProbedAt: at.UnixMilli(), ID: fileID,
	})
}

// SetFileQualityFrom stores a file's quality together with where it came from.
// Prefer this over SetFileQuality: a quality with no provenance is exactly the
// ambiguity ADR 0013 set out to remove.
func (d *DB) SetFileQualityFrom(ctx context.Context, fileID int64, q quality.Quality,
	prov mediainfo.Provenance, conf mediainfo.Confidence) error {
	if !prov.Valid() {
		prov = mediainfo.ProvenanceUnknown
	}
	if !conf.Valid() {
		conf = mediainfo.ConfidenceNone
	}
	return d.Write.SetFileQualityWithProvenance(ctx, sqlitegen.SetFileQualityWithProvenanceParams{
		Quality: q.String(), QualityProvenance: string(prov),
		QualityConfidence: string(conf), ID: fileID,
	})
}
