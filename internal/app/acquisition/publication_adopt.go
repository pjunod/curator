package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// ---- adoption: every committed Runner publication is picked up once -------

// Runner announces a publication on its event stream, its ordered history and
// its deliveries feed, and Curator attaches each announcement to the row that
// carries its transfer token. A fact that reached Curator while it had no row
// for the token (the row was removed from Activity while its salvage was still
// pending, or the feed moved past it during an outage) was not Curator's at
// that moment and the cursor moved on; the directory, committed by its
// .runner-manifest.json, then sat under the completed root with no
// publication, imported and cleaned by nothing.
//
// The completed roots are the ground truth, so the hourly pass reads them:
// a payload directory (payloadDirs) no publication claims, whose manifest is valid and
// names a Curator transfer token, is attributed by that token exactly as the
// feed would have attributed it (the row's publication, completed or
// salvage), and a publication row is unique per directory, so a replay or a
// later feed fact finds the row this made. A token with no row left is an
// orphan: Curator minted it, the attempt it named is gone from its records
// and its want was grabbed again when that attempt failed, so nothing can
// import it; it is recorded as a live publication whose files are discarded,
// and the same pass retires the directory file by file.

// runnerManifestFile is .runner-manifest.json as Runner writes it: the
// manifest plus the job and transfer it belongs to.
type runnerManifestFile struct {
	Generation  string               `json:"generation"`
	Job         int64                `json:"job"`
	Transfer    string               `json:"transfer"`
	Digest      string               `json:"digest"`
	CrossVolume bool                 `json:"cross_volume"`
	Files       []ports.ManifestFile `json:"files"`
}

// orphanReasonPrefix starts the error of an adopted directory with no row.
const orphanReasonPrefix = "no download row for its transfer"

// adoptUnclaimedPublications runs the adoption over every completed root and
// returns how many directories it attributed and how many it orphaned.
func (s *Service) adoptUnclaimedPublications(ctx context.Context) (adopted, orphaned int, err error) {
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	claimed, err := s.db.ExistingPublicationPaths(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, root := range roots {
		dirs, err := payloadDirs(root)
		if err != nil {
			// An unreadable or unmounted root is not evidence that anything
			// is missing: the next pass reads it again.
			s.log.Debug("adoption: completed root unreadable", "root", root, "err", err)
			continue
		}
		for _, path := range dirs {
			if ctx.Err() != nil {
				return adopted, orphaned, ctx.Err()
			}
			if claimed[path] {
				continue
			}
			ok, orphan, err := s.adoptDirectory(ctx, path)
			if err != nil {
				s.log.Warn("adoption: publication directory not adopted; the next pass retries", "path", path, "err", err)
				continue
			}
			switch {
			case orphan:
				orphaned++
			case ok:
				adopted++
			}
		}
	}
	if adopted > 0 || orphaned > 0 {
		s.log.Info("adoption: unclaimed Runner publications picked up", "attributed", adopted, "orphaned", orphaned)
	}
	return adopted, orphaned, nil
}

// adoptDirectory adopts one unclaimed directory. ok reports a publication
// now claims it; orphan that it was recorded with no row.
func (s *Service) adoptDirectory(ctx context.Context, path string) (ok, orphan bool, err error) {
	raw, err := readManifestFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil // not committed (or not Runner's): nothing to adopt
	}
	if errors.Is(err, errManifestTooLarge) {
		return false, false, nil // not a manifest this pass can attribute
	}
	if err != nil {
		return false, false, err
	}
	var mf runnerManifestFile
	if err := json.Unmarshal(raw, &mf); err != nil {
		return false, false, nil // not a manifest this pass can attribute
	}
	m := ports.Manifest{Generation: mf.Generation, Files: mf.Files, Digest: mf.Digest, CrossVolume: mf.CrossVolume, Source: "file"}
	if mf.Generation == "" || verifyManifestShape(m, mf.Generation) != nil {
		return false, false, nil
	}
	// Curator mints every transfer token it submits as t-<n>-<hex>: a
	// directory without one was not Curator's to import or delete.
	if !strings.HasPrefix(mf.Transfer, "t-") {
		return false, false, nil
	}
	rawManifest, err := json.Marshal(m)
	if err != nil {
		return false, false, err
	}
	dl, found, err := s.db.DownloadByTransferAnyClient(ctx, mf.Transfer)
	if err != nil {
		return false, false, err
	}
	if !found {
		return s.adoptOrphan(ctx, path, mf, rawManifest)
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		return false, false, err
	}
	remote := unmapLocalPath(cfg.PathMappings, path)
	if ports.MapRemotePath(cfg.PathMappings, remote) != path {
		return false, false, fmt.Errorf("the client's path mappings do not map %s back to itself", path)
	}
	pub := &ports.RunnerPublication{Path: remote, Generation: mf.Generation, ManifestDigest: mf.Digest, Files: len(m.Files),
		CrossVolume: mf.CrossVolume, Kind: "completed"}
	if !rowTerminal(dl) {
		// The row has no terminal yet: only Runner says how the transfer
		// ended. Its request receipt is asked; nothing is inferred from the
		// directory. Unreachable, unknown or not yet terminal: the next
		// pass asks again.
		feeds, ok := runnerFeedsOf(s.newClient(cfg))
		if !ok {
			return false, false, nil
		}
		adm, known, err := feeds.GetRequestAdmission(ctx, dl.Transfer)
		if err != nil || !known {
			return false, false, nil
		}
		t, terminal := admissionTerminal(adm)
		if !terminal {
			return false, false, nil
		}
		if t.Outcome == ports.OutcomeCompleted && t.Publication != nil && ports.MapRemotePath(cfg.PathMappings, t.Publication.Path) == path {
			// Runner confirms this directory is the transfer's completed
			// publication: recorded as the terminal would have been.
			t.Publication = pub
		}
		if err := s.observe(ctx, dl, cfg, ports.RunnerFact{Kind: ports.FactTerminal, Transfer: dl.Transfer, Handle: ports.Handle(adm.JobID),
			Terminal: &t, Source: "completed-root scan (request receipt)"}); err != nil {
			return false, false, err
		}
		if dl, err = s.db.GetDownload(ctx, dl.ID); err != nil {
			return false, false, err
		}
	}
	if _, claimed, err := s.db.PublicationByPath(ctx, path); err != nil {
		return false, false, err
	} else if !claimed {
		if !rowTerminal(dl) {
			return false, false, nil
		}
		// A row whose terminal Runner gave (failed, cancelled, or completed
		// with its publication elsewhere): the directory is a salvage or a
		// late publication of it, imported under the row as the feed's
		// publication_ready would be.
		pub.Kind = "salvage"
		if err := s.observe(ctx, dl, cfg, ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: dl.Transfer, Publication: pub, Source: "completed-root scan"}); err != nil {
			return false, false, err
		}
	}
	p, claimedNow, err := s.db.PublicationByPath(ctx, path)
	if err != nil || !claimedNow {
		return false, false, err
	}
	// The manifest this pass read is the publication's from now on: the
	// cleanup requires the file to stay while files remain.
	s.recordFileManifest(ctx, p, m)
	s.log.Info("adoption: Runner publication attributed to its download", "path", path, "download", dl.ID, "publication", p.ID, "kind", p.Kind)
	return true, false, nil
}

// rowTerminal reports a row whose Runner terminal is written.
func rowTerminal(dl sqlite.Download) bool {
	return (dl.RunnerOutcome != "" && !dl.Provisional) || dl.State == "failed" || dl.State == "imported" || dl.State == "notified"
}

// orphanMinAge is how long a committed directory must have sat with no row
// before it is an orphan: a transfer Curator is still recording (a restore,
// a row written after the submit) has its row by then.
const orphanMinAge = 24 * time.Hour

// adoptOrphan records a committed directory whose Curator transfer token has
// no row, once it is old enough and every enabled Runner client confirms the
// token is unknown to it or terminal (none says it is still running). An
// unreachable Runner, or none configured, means the next pass asks again.
func (s *Service) adoptOrphan(ctx context.Context, path string, mf runnerManifestFile, rawManifest []byte) (bool, bool, error) {
	info, err := os.Stat(filepath.Join(path, ports.ManifestFileName))
	if err != nil || time.Since(info.ModTime()) < orphanMinAge {
		return false, false, nil
	}
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return false, false, err
	}
	asked := 0
	for _, cfg := range clients {
		if !cfg.Enabled {
			continue
		}
		feeds, ok := runnerFeedsOf(s.newClient(cfg))
		if !ok {
			continue
		}
		adm, known, err := feeds.GetRequestAdmission(ctx, mf.Transfer)
		if err != nil {
			return false, false, nil
		}
		if known {
			if _, terminal := admissionTerminal(adm); !terminal {
				return false, false, nil
			}
		}
		asked++
	}
	if asked == 0 {
		return false, false, nil
	}
	reason := fmt.Sprintf("%s %s (Runner job %d): the attempt is gone from Curator's records, Runner reports it unknown or ended, and its want was grabbed again; nothing can import it, so it is not kept", orphanReasonPrefix, mf.Transfer, mf.Job)
	files := make([]sqlite.PublicationFile, 0, len(mf.Files))
	for _, f := range mf.Files {
		files = append(files, sqlite.PublicationFile{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256})
	}
	_, inserted, err := s.db.RecordOrphanPublication(ctx, sqlite.PublicationWrite{Generation: mf.Generation, Kind: "completed", Path: path,
		RemotePath: path, ManifestJSON: string(rawManifest), Source: "file", CrossVolume: mf.CrossVolume, Cohort: "live"}, files, reason)
	if err != nil || !inserted {
		return false, false, err
	}
	_ = s.db.AddHistory(ctx, "publication_orphaned", 0, filepath.Base(path),
		map[string]any{"publication": path, "transfer": mf.Transfer, "job": mf.Job, "generation": mf.Generation, "reason": reason})
	s.log.Warn("adoption: Runner publication has no download row; its files are not kept", "path", path, "transfer", mf.Transfer, "job", mf.Job)
	return true, true, nil
}

// unmapLocalPath is MapRemotePath backwards: Curator's view of a path as the
// client reports it (the first mapping whose local side contains it).
func unmapLocalPath(maps []ports.PathMapping, p string) string {
	for _, m := range maps {
		local := strings.TrimRight(m.Local, "/")
		if local == "" {
			continue
		}
		remote := strings.TrimRight(m.Remote, "/")
		if p == local {
			return remote
		}
		if strings.HasPrefix(p, local+"/") {
			return remote + p[len(local):]
		}
	}
	return p
}
