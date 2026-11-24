package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// ---- what may be a publication's directory ---------------------------------

// A publication is one payload directory. A completed root, the "completed"
// storage container under a working-tree root, or any directory above them is
// never a payload: a publication there "contains" every payload beside it, so
// Download storage explained every folder as inside it and refused their
// whole-folder deletes (field data 2026-11-12: publication 344 at
// /working/monarr/completed). Migration 0040 had fenced every download's
// import_path, and a pre-delivery download whose single-file payload sat
// directly in the completed directory reported that directory as its import
// path; migration 0073 then made each fenced path a historical publication.

// payloadContainer reports a path that is a completed root, a root's
// completed storage container, or a directory above either — compared both
// as written and through the roots' bind-mount aliases (storagePath: the same
// container is /downloads and /working/monarr/completed).
func payloadContainer(path string, roots []string) bool {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return true
	}
	candidates := []string{path, storagePath(path, roots)}
	for _, root := range roots {
		root = filepath.Clean(root)
		for _, r := range []string{root, storagePath(root, roots)} {
			for _, p := range candidates {
				if within(p, r) || p == filepath.Join(r, "completed") {
					return true
				}
			}
		}
	}
	return false
}

// isPayloadContainer is payloadContainer over the configured roots. It fails
// closed: when the roots cannot be read the path is refused (an error the
// caller holds on, so the fact is read again).
func (s *Service) isPayloadContainer(ctx context.Context, path string) (bool, error) {
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return true, fmt.Errorf("completed roots unreadable; %s is not accepted as a publication: %w", path, err)
	}
	return payloadContainer(path, roots), nil
}

// payloadDirs lists the payload directories of one completed root: its
// children, and for a working-tree root the children of its completed
// container instead of the container itself (as the inventory lists them).
func payloadDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		if e.Name() != "completed" {
			out = append(out, path)
			continue
		}
		inner, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, c := range inner {
			if c.IsDir() {
				out = append(out, filepath.Join(path, c.Name()))
			}
		}
	}
	return out, nil
}

// ---- record-only publications end without a person --------------------------

// closeRecordOnlyPublications ends the publications that are only a record of
// a path (no download row, no manifest, no file: the migration-0040 review
// cohort and directories the cutover froze), when the evidence allows:
//
//   - the path was never a payload directory (payloadContainer): the record
//     is invalid;
//   - the directory is gone while the storage that held it is verifiably
//     present before and after the check (inside a root: the root's storage
//     identity; outside every root: its parent sits on the mount of a
//     verified completed root). A path on no verified mount is never closed.
//
// The record is deleted, freeing its path (a folder that returns there is a
// new payload, not this record), and a history event keeps what it was.
// Nothing on disk is touched. A record whose directory is still there waits
// for its review batch (ADR 0026, addendum 10), and one whose storage cannot
// be verified now is checked again on the next pass.
func (s *Service) closeRecordOnlyPublications(ctx context.Context) (int, error) {
	pubs, err := s.db.RecordOnlyPublications(ctx)
	if err != nil || len(pubs) == 0 {
		return 0, err
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, p := range pubs {
		if ctx.Err() != nil {
			return closed, ctx.Err()
		}
		reason, ok := s.recordOnlyEnd(ctx, p, roots)
		if !ok {
			continue
		}
		done, err := s.db.DeleteRecordOnlyPublication(ctx, p.ID)
		if err != nil {
			return closed, err
		}
		if !done {
			continue
		}
		closed++
		_ = s.db.AddHistory(ctx, "publication_closed", 0, filepath.Base(p.Path), map[string]any{"publication": p.Path, "id": p.ID,
			"cohort": p.Cohort, "error": p.Error, "reason": reason})
		s.log.Info("publication record closed", "publication", p.ID, "path", p.Path, "reason", reason)
	}
	if closed > 0 {
		s.log.Info("record-only publications closed", "closed", closed, "of", len(pubs))
	}
	return closed, nil
}

// recordOnlyEnd decides whether a record-only publication ends now, and why.
func (s *Service) recordOnlyEnd(ctx context.Context, p sqlite.Publication, roots []string) (string, bool) {
	if payloadContainer(p.Path, roots) {
		return fmt.Sprintf("not a payload directory: %s is a completed root or its storage container (recorded from a pre-delivery download's save directory); record closed, nothing deleted", p.Path), true
	}
	inside := false
	for _, root := range roots {
		if within(root, p.Path) || within(root, storagePath(p.Path, roots)) {
			inside = true
		}
	}
	verify := func() error {
		if inside {
			// An unmounted share is an empty directory, not a payload that
			// is gone: only a root whose storage identity holds proves it.
			return s.checkStorageIdentity(ctx, p.Path)
		}
		return s.onVerifiedMount(ctx, filepath.Dir(p.Path))
	}
	if verify() != nil {
		return "", false
	}
	if _, err := os.Lstat(p.Path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", false // still there (its review decides it), or unreadable now
	}
	if verify() != nil {
		return "", false // the storage moved while it was checked
	}
	return "its directory is gone from verified storage; record closed, nothing deleted", true
}

// onVerifiedMount reports nil when dir exists and sits on the same storage
// (mount source, filesystem root and type) as a completed root whose own
// identity the inventory verified and that still holds. A path on any other
// mount — or an empty mountpoint — is not verified.
func (s *Service) onVerifiedMount(ctx context.Context, dir string) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not present", dir)
	}
	observed, err := observeStorageIdentity(dir)
	if err != nil {
		return err
	}
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	for _, root := range inventory.Roots {
		if root.Error != "" || root.StorageIdentity == "" {
			continue
		}
		current, err := observeStorageIdentity(root.Path)
		if err != nil || (current != root.StorageIdentity && !canStrengthenStorageIdentity(root.Path, root.StorageIdentity, current)) {
			continue
		}
		if observed == current {
			return nil
		}
	}
	return fmt.Errorf("%s is on no verified completed storage", dir)
}

// ---- every waiting publication has an exit ----------------------------------

// offerWaitingPublications is the hourly offer of every publication waiting
// for (or stuck in) an import, independent of the poll: the poll offers only
// an enabled Runner client's publications, so one whose client was deleted,
// disabled or replaced would otherwise wait forever. An enabled Runner
// client's publication is offered to the import workers (the import's
// deadline and attempt cap then end it if it cannot finish; a hold is the
// environment's and clears itself); any other is ended now, its committed
// placements recorded, the rest discarded and its want grabbed again
// (endPublication).
func (s *Service) offerWaitingPublications(ctx context.Context) (offered, ended int, err error) {
	pubs, err := s.db.OfferablePublications(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, p := range pubs {
		if ctx.Err() != nil {
			return offered, ended, ctx.Err()
		}
		if s.ImportRunning(p.DownloadID) {
			continue
		}
		dl, err := s.db.GetDownload(ctx, p.DownloadID)
		if err != nil {
			continue
		}
		cfg, cerr := s.db.GetDownloadClient(ctx, dl.ClientID)
		if cerr == nil && cfg.Enabled && isRunnerClient(s.newClient(cfg)) {
			if s.enqueuePublicationImport(ctx, dl, cfg, p.ID) {
				offered++
			}
			continue
		}
		unlock := s.lockDownload(p.DownloadID)
		cur, gerr := s.db.GetPublication(ctx, p.ID)
		if gerr == nil && !s.ImportRunning(p.DownloadID) && (cur.State == "discovered" || cur.State == "importing") {
			if cerr != nil {
				cfg = ports.ClientConfig{}
			}
			s.endPublication(ctx, dl, cfg, cur, nil, "not imported: its download client is gone, disabled or not Runner")
			ended++
		}
		unlock()
	}
	if offered > 0 || ended > 0 {
		s.log.Info("waiting publications offered", "offered", offered, "ended", ended, "of", len(pubs))
	}
	return offered, ended, nil
}

// salvagePendingLimit bounds how long a row waits, hidden, for a salvage
// Runner reported pending: Runner's receipt keeps saying pending after the
// salvage was published, so it cannot say "never" itself.
const salvagePendingLimit = 7 * 24 * time.Hour

// settlePendingSalvages settles, from Runner, the rows hidden because their
// salvage was pending (Clear failed, Remove and retention keep them while it
// is): a receipt Runner no longer has, or one whose salvage is no longer
// pending, settles the row; one still pending past salvagePendingLimit is
// settled none (a directory it publishes later is still adopted, or
// orphaned, by the completed-root pass). The ordinary rules then let the row
// go. An unreachable Runner is asked again next pass.
func (s *Service) settlePendingSalvages(ctx context.Context) (int, error) {
	rows, err := s.db.PendingSalvageDownloads(ctx)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, dl := range rows {
		if ctx.Err() != nil {
			return settled, ctx.Err()
		}
		value := ""
		cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
		var feeds ports.RunnerFeeds
		ok := false
		if err == nil && cfg.Enabled {
			feeds, ok = runnerFeedsOf(s.newClient(cfg))
		}
		switch {
		case !ok:
			value = "none" // no Runner left to deliver it
		default:
			adm, known, aerr := feeds.GetRequestAdmission(ctx, dl.Transfer)
			switch {
			case aerr != nil:
				continue
			case !known:
				value = "none"
			case adm.Salvage != "" && adm.Salvage != "pending":
				value = adm.Salvage
			case !dl.RunnerOutcomeAt.IsZero() && time.Since(dl.RunnerOutcomeAt) > salvagePendingLimit:
				value = "none"
			default:
				continue
			}
		}
		if err := s.db.SettleRunnerSalvage(ctx, dl.ID, value); err != nil {
			return settled, err
		}
		settled++
		s.log.Info("pending salvage settled", "download", dl.ID, "salvage", value)
	}
	return settled, nil
}
