package acquisition

import (
	"context"
	"fmt"
	"time"

	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// The handoff is the stretch between "the download client finished" and
// "the files are in the library". Upstream tools treat it as a black box;
// Monarr makes every step explicit and persisted (downloads.handoff_log)
// so the Activity page can lay out exactly what happened — and, when a
// completed download won't import, exactly where it looked and why it
// stopped.
//
// Steps recorded in the log (a superset of the persisted states — the log
// is finer-grained than the state column):
const (
	stepGrabbed     = "grabbed"
	stepDownloading = "downloading"
	stepDownloaded  = "downloaded"
	stepAwaiting    = "awaiting_import"
	stepImporting   = "importing"
	stepImported    = "imported"
	stepFailed      = "failed"
)

// advance records one handoff step: it appends a log entry, sets the new
// state/progress/error, and persists the row (state + paths + full log). It
// mutates *dl so later calls in the same flow build on the updated log.
func (s *Service) advance(ctx context.Context, dl *sqlite.Download, state string, progress float64, errMsg, step, detail string) {
	dl.State = state
	dl.Progress = progress
	dl.Error = errMsg
	dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{
		Step: step, At: time.Now().UnixMilli(), Detail: detail,
	})
	if err := s.db.UpdateDownloadHandoff(ctx, *dl); err != nil {
		s.log.Warn("handoff: persist failed", "download", dl.ID, "err", err)
	}
}

// recordDownloaded captures the client-reported path and its remote-path
// mapping, then marks the download 'downloaded'. Called once, when a grab
// first shows up complete.
func (s *Service) recordDownloaded(ctx context.Context, dl *sqlite.Download, cfg ports.ClientConfig, st ports.DownloadStatus, source string) {
	dl.SavePath = st.SavePath
	dl.ImportPath = ports.MapRemotePath(cfg.PathMappings, st.SavePath)
	detail := fmt.Sprintf("%s finished; payload at %s", clientLabel(cfg), orNone(st.SavePath))
	if dl.ImportPath != st.SavePath {
		detail += fmt.Sprintf(" (mapped to %s)", dl.ImportPath)
	}
	// Which channel delivered the completion. "(event 913)" versus
	// "(poll)" is how an operator sees that push is actually working —
	// otherwise a dead subscription and a healthy one produce identical
	// traces, just 30 seconds apart, and nobody notices for weeks.
	s.advance(ctx, dl, "downloaded", 1, "", stepDownloaded, traced(detail, source))
}

// onDownloaded is the completed-download branch of the poller. It records
// the download, then imports it (auto). The manual-approval gate is layered
// in on top of this.
func (s *Service) onDownloaded(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, st ports.DownloadStatus, source string) {
	if dl.State == "grabbed" || dl.State == "downloading" {
		s.recordDownloaded(ctx, &dl, cfg, st, source)
	}
	if cfg.ManualApproval {
		if dl.State != "awaiting_import" {
			s.advance(ctx, &dl, "awaiting_import", 1, "", stepAwaiting,
				"held for your approval before import")
		}
		return
	}
	// Fire-and-forget from the poller: runImport records the outcome (state
	// + trace) itself; the error only matters to a user-initiated import.
	_ = s.runImport(ctx, dl)
}

// runImport performs the actual import and records its steps. It reuses the
// path captured at download time (falling back to the raw save path). On
// error the download is surfaced as failed WITHOUT blocklisting — a payload
// Monarr merely couldn't place is a local problem to fix and retry, not a
// bad release to replace. The error is returned as well so a user-initiated
// import can report why it failed.
func (s *Service) runImport(ctx context.Context, dl sqlite.Download) error {
	imp := dl.ImportPath
	if imp == "" {
		imp = dl.SavePath
	}
	s.advance(ctx, &dl, "importing", 1, "", stepImporting,
		"looking for media files in "+orNone(imp))
	// Automatic import: gated by the profile, because that is what a profile
	// is for. A manual import is the override and passes manual=true.
	result, err := s.importDownload(ctx, dl, imp, false)
	if err != nil {
		s.failImport(ctx, &dl, err.Error())
		return err
	}
	detail := fmt.Sprintf("imported %d file(s) into the library", result.Imported)
	if result.Upgraded {
		detail += " (upgrade)"
	}
	// A partial import is not a failure, but it is a thing the user needs to
	// be able to see afterwards without reading the server log.
	if skipped := result.Skipped(); len(skipped) > 0 {
		detail += fmt.Sprintf("; %d skipped — %s", len(skipped), result.reasons(3))
	}
	s.advance(ctx, &dl, "imported", 1, "", stepImported, detail)
	// The library has the files now, so the client's copy is just occupying a
	// disk. Inline rather than left to the hourly sweep: a night of 60 GB
	// remuxes is several hundred gigabytes of "we will get to it".
	s.cleanupAfterImport(ctx, downloadRef{
		ID: dl.ID, MediaItemID: dl.MediaItemID, ClientID: dl.ClientID,
		Handle: dl.Handle, ReleaseTitle: dl.ReleaseTitle, Size: dl.Size,
	})
	s.InvalidateWanted()
	return nil
}

// ImportNow imports (or re-imports) a download on demand: it approves an
// item held for manual approval, and retries one whose import failed after
// the underlying problem (a mount, a path mapping) is fixed. It uses the
// path captured at download time.
func (s *Service) ImportNow(ctx context.Context, id int64) error {
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if dl.State == "imported" {
		return nil
	}
	if dl.ImportPath == "" && dl.SavePath == "" {
		return fmt.Errorf("no download path recorded yet — wait for the download to finish")
	}
	return s.runImport(ctx, dl)
}

// BlocklistReplace declares a download's release bad: it removes the payload
// from the client, blocklists the release so it's never grabbed again, and
// searches for a replacement — the explicit escalation for a download that
// keeps failing to import or is simply wrong.
func (s *Service) BlocklistReplace(ctx context.Context, id int64) error {
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if dl.Handle != "" {
		if cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID); err == nil {
			_ = s.newClient(cfg).Remove(ctx, ports.Handle(dl.Handle), true)
		}
	}
	s.handleFailure(ctx, dl, dl.Progress, "blocklisted by user")
	return nil
}

// failImport marks an import as failed and surfaces the reason, but does NOT
// blocklist or auto-search a replacement: the release downloaded fine, so
// the fix is on Monarr's side (mounts, a remote path mapping, or a manual
// import), and the user drives it.
func (s *Service) failImport(ctx context.Context, dl *sqlite.Download, reason string) {
	s.advance(ctx, dl, "failed", dl.Progress, reason, stepFailed, reason)
	_ = s.db.AddHistory(ctx, "import_failed", dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"reason": reason})
	s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: reason})
	s.log.Warn("import failed; awaiting manual action", "release", dl.ReleaseTitle, "reason", reason)
}

// clientLabel is the client's name, or its type when unnamed.
func clientLabel(cfg ports.ClientConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return cfg.Type
}

func orNone(p string) string {
	if p == "" {
		return "(no path reported)"
	}
	return p
}
