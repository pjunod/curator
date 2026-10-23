package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type DeliveryLifecycleReceipt struct {
	ReceiptID                     int64                        `json:"receiptId"`
	ClientID                      int64                        `json:"clientId"`
	DownloadID                    int64                        `json:"downloadId"`
	GenerationKey                 string                       `json:"generationKey"`
	MediaItemID                   int64                        `json:"mediaItemId"`
	CopyID                        int64                        `json:"copyId"`
	DeliveryID                    string                       `json:"deliveryId"`
	PublicationGeneration         string                       `json:"publicationGeneration"`
	ManifestDigest                string                       `json:"manifestDigest"`
	RunnerInstanceID              string                       `json:"runnerInstanceId"`
	JobID                         string                       `json:"jobId"`
	RequestToken                  string                       `json:"requestToken"`
	PublicationPath               string                       `json:"publicationPath"`
	MappedPath                    string                       `json:"mappedPath"`
	ReadyAt                       time.Time                    `json:"readyAt"`
	TerminalOutcome               string                       `json:"terminalOutcome"`
	FailureStage                  string                       `json:"failureStage,omitempty"`
	FailureCause                  string                       `json:"failureCause,omitempty"`
	KeepSourceOnFailure           bool                         `json:"keepSourceOnFailure"`
	State                         string                       `json:"state"`
	Acceptance                    string                       `json:"acceptance"`
	ConsumerReceiptID             string                       `json:"consumerReceiptId,omitempty"`
	Attempts                      int                          `json:"attempts"`
	BlockedSince                  time.Time                    `json:"blockedSince,omitempty"`
	NextAttemptAt                 time.Time                    `json:"nextAttemptAt,omitempty"`
	Error                         string                       `json:"error,omitempty"`
	ManifestFiles                 []ports.RunnerFile           `json:"manifestFiles"`
	SourceFileOutcomes            []ports.RunnerFileOutcome    `json:"sourceFileOutcomes,omitempty"`
	SourceCleanup                 json.RawMessage              `json:"sourceCleanup,omitempty"`
	SourceCleanupAttempts         int                          `json:"sourceCleanupAttempts"`
	SourceCleanupBlockedSince     time.Time                    `json:"sourceCleanupBlockedSince,omitempty"`
	SourceCleanupNextAttemptAt    time.Time                    `json:"sourceCleanupNextAttemptAt,omitempty"`
	SourceCleanupError            string                       `json:"sourceCleanupError,omitempty"`
	DirectoryCleanupState         string                       `json:"directoryCleanupState"`
	DirectoryCleanupAttempts      int                          `json:"directoryCleanupAttempts"`
	DirectoryCleanupBlockedSince  time.Time                    `json:"directoryCleanupBlockedSince,omitempty"`
	DirectoryCleanupNextAttemptAt time.Time                    `json:"directoryCleanupNextAttemptAt,omitempty"`
	DirectoryCleanupError         string                       `json:"directoryCleanupError,omitempty"`
	CuratorFiles                  []sqlite.DeliveryFileOutcome `json:"curatorFiles"`
	AttemptHistory                []DeliveryLifecycleAttempt   `json:"attemptHistory"`
}

type DeliveryLifecycleAttempt struct {
	ID         int64     `json:"id"`
	Stage      string    `json:"stage"`
	Attempt    int       `json:"attempt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
}

type DeliveryLifecyclePage struct {
	Items        []DeliveryLifecycleReceipt `json:"items"`
	NextBeforeID int64                      `json:"nextBeforeId,omitempty"`
}

func (s *Service) DeliveryLifecycle(ctx context.Context, beforeID, downloadID int64, limit int) (DeliveryLifecyclePage, error) {
	if limit < 1 || limit > 100 {
		return DeliveryLifecyclePage{}, fmt.Errorf("limit must be between 1 and 100")
	}
	var receipts []sqlite.DeliveryReceipt
	var next int64
	var err error
	if downloadID > 0 {
		receipts, next, err = s.db.DeliveryReceiptPageForDownload(ctx, downloadID, beforeID, limit)
	} else {
		receipts, next, err = s.db.DeliveryReceiptPage(ctx, beforeID, limit)
	}
	if err != nil {
		return DeliveryLifecyclePage{}, err
	}
	page := DeliveryLifecyclePage{Items: make([]DeliveryLifecycleReceipt, 0, len(receipts)), NextBeforeID: next}
	for _, r := range receipts {
		item, err := s.deliveryLifecycleReceipt(ctx, r)
		if err != nil {
			return DeliveryLifecyclePage{}, err
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func (s *Service) deliveryLifecycleReceipt(ctx context.Context, r sqlite.DeliveryReceipt) (DeliveryLifecycleReceipt, error) {
	details, err := s.db.DeliveryRetryDetails(ctx, r.ID)
	if err != nil {
		return DeliveryLifecycleReceipt{}, err
	}
	files, err := s.db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		return DeliveryLifecycleReceipt{}, err
	}
	events, err := s.db.DeliveryAttemptEvents(ctx, r.ID)
	if err != nil {
		return DeliveryLifecycleReceipt{}, err
	}
	attempts := make([]DeliveryLifecycleAttempt, 0, len(events))
	for _, event := range events {
		attempts = append(attempts, DeliveryLifecycleAttempt{ID: event.ID, Stage: event.Stage, Attempt: event.Attempt, StartedAt: event.StartedAt, FinishedAt: event.FinishedAt, Result: event.Result, Error: event.Error})
	}
	return DeliveryLifecycleReceipt{
		ReceiptID: r.ID, ClientID: r.ClientID, DownloadID: r.DownloadID, GenerationKey: r.GenerationKey,
		MediaItemID: r.MediaItemID, CopyID: r.CopyID, DeliveryID: r.Delivery.DeliveryID,
		PublicationGeneration: r.Delivery.PublicationGeneration, ManifestDigest: r.Delivery.ManifestDigest,
		RunnerInstanceID: r.Delivery.RunnerInstanceID, JobID: r.Delivery.JobID, RequestToken: r.Delivery.RequestToken,
		PublicationPath: r.Delivery.PublicationPath, MappedPath: r.MappedPath, ReadyAt: r.ReadyAt,
		TerminalOutcome: r.Delivery.TerminalOutcome, FailureStage: r.Delivery.FailureStage,
		FailureCause: r.Delivery.FailureCause, KeepSourceOnFailure: r.Delivery.KeepSourceOnFailure,
		State: r.State, Acceptance: r.AckState, ConsumerReceiptID: r.ConsumerReceiptID,
		Attempts: r.Attempts, BlockedSince: r.BlockedSince, NextAttemptAt: r.NextAttemptAt, Error: r.Error,
		ManifestFiles: r.Delivery.Files, SourceFileOutcomes: r.Delivery.FileOutcomes, SourceCleanup: r.Delivery.SourceCleanup,
		SourceCleanupAttempts: details.SourceAttempts, SourceCleanupBlockedSince: details.SourceBlockedSince,
		SourceCleanupNextAttemptAt: details.SourceNext, SourceCleanupError: details.SourceError,
		DirectoryCleanupState: details.DirectoryState, DirectoryCleanupAttempts: details.DirectoryAttempts,
		DirectoryCleanupBlockedSince: details.DirectoryBlockedSince, DirectoryCleanupNextAttemptAt: details.DirectoryNext,
		DirectoryCleanupError: details.DirectoryError, CuratorFiles: files, AttemptHistory: attempts,
	}, nil
}

func (s *Service) DeliveryLifecycleByID(ctx context.Context, receiptID int64) (DeliveryLifecycleReceipt, error) {
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return DeliveryLifecycleReceipt{}, err
	}
	return s.deliveryLifecycleReceipt(ctx, r)
}

func (s *Service) RetryDeliveryHandoff(ctx context.Context, receiptID int64) error {
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "conflict" {
		return fmt.Errorf("delivery identity conflicts with its saved receipt")
	}
	if r.DownloadID > 0 {
		unlock := s.lockDownload(r.DownloadID)
		defer unlock()
	}
	cfg, err := s.db.GetDownloadClient(ctx, r.ClientID)
	if err != nil {
		return err
	}
	client := s.newClient(cfg)
	protocol, ok := client.(ports.DeliveryProtocol)
	if !ok {
		return fmt.Errorf("download client no longer supports Runner delivery handoff")
	}
	if err := s.db.ForceDeliveryRetryNow(ctx, receiptID); err != nil {
		return err
	}
	s.scheduleDeliveryReceipts(ctx, cfg, protocol)
	return nil
}

func (s *Service) RetryRunnerSourceCleanup(ctx context.Context, receiptID int64) error {
	// Use the same per-receipt control lock as the automatic GET refresher.
	// This prevents a list-selected, delayed refresh from issuing after this
	// explicit POST has recorded a provider Retry-After boundary.
	unlockSource := s.lockDeliverySourceControl(receiptID)
	defer unlockSource()
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "conflict" || (r.AckState != "accepted" && (r.AckState != "pending" || r.ConsumerReceiptID == "")) {
		return fmt.Errorf("runner source cleanup requires a locally prepared, non-conflicting delivery")
	}
	if r.DownloadID > 0 {
		unlock := s.lockDownload(r.DownloadID)
		defer unlock()
		// Reload both identity and due time after acquiring the per-download
		// lock. A second click may have waited behind a POST that recorded a
		// new Retry-After deadline.
		fresh, loadErr := s.db.DeliveryReceiptByID(ctx, receiptID)
		if loadErr != nil {
			return loadErr
		}
		if fresh.DownloadID != r.DownloadID || fresh.GenerationKey != r.GenerationKey || fresh.Delivery.DeliveryID != r.Delivery.DeliveryID || fresh.Delivery.RequestToken != r.Delivery.RequestToken {
			return sqlite.ErrDeliveryReceiptStale
		}
		if fresh.State == "conflict" || (fresh.AckState != "accepted" && (fresh.AckState != "pending" || fresh.ConsumerReceiptID == "")) {
			return fmt.Errorf("runner source cleanup requires a locally prepared, non-conflicting delivery")
		}
		r = fresh
	}
	// Explicit retry may bypass Curator's ordinary refresh cadence, but it
	// must honor an authoritative Retry-After supplied by Runner.
	retryDue, err := s.db.DeliverySourceProviderRetryAfter(ctx, receiptID)
	if err != nil {
		return err
	}
	if retryDue.After(time.Now()) {
		return fmt.Errorf("runner source-cleanup retry is due after %s", retryDue.UTC().Format(time.RFC3339))
	}
	cfg, err := s.db.GetDownloadClient(ctx, r.ClientID)
	if err != nil {
		return err
	}
	client := s.newClient(cfg)
	retry, ok := client.(ports.DeliverySourceCleanupRetryProtocol)
	if !ok {
		return fmt.Errorf("download client cannot retry Runner source cleanup")
	}
	fresh, err := retry.RetryDeliverySourceCleanup(ctx, r.Delivery.DeliveryID)
	if err != nil {
		if statusErr := s.db.FailDeliverySourceRefresh(context.WithoutCancel(ctx), receiptID, err.Error(), submissionRetryDeadline(err)); statusErr != nil {
			s.log.Warn("Runner source-cleanup retry deadline could not be persisted", "receipt", receiptID, "err", statusErr)
		}
		return err
	}
	if !sameDeliveryImmutable(r.Delivery, fresh) {
		_ = s.db.SetDeliveryConflict(ctx, receiptID, "Runner delivery changed during source-cleanup retry")
		return fmt.Errorf("runner delivery identity changed during source-cleanup retry")
	}
	retryAfter := 5 * time.Minute
	if deliverySourceCleanupTerminal(fresh.SourceCleanup) {
		retryAfter = 24 * time.Hour
	}
	if err := s.db.RefreshDeliverySourceCleanup(ctx, receiptID, fresh.SourceCleanup, fresh.FileOutcomes, retryAfter); err != nil {
		return err
	}
	return nil
}

func (s *Service) RetryCuratorDeliveryCleanup(ctx context.Context, receiptID int64) error {
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "conflict" {
		return fmt.Errorf("delivery identity conflicts with its saved receipt")
	}
	if err := s.db.ForceDeliveryCleanupRetryNow(ctx, receiptID); err != nil {
		return err
	}
	cfg, err := s.db.GetDownloadClient(ctx, r.ClientID)
	if err != nil {
		return err
	}
	if !s.queueDeliveryCleanup(ctx, cfg, r) {
		return fmt.Errorf("cleanup worker is unavailable or busy; retry again shortly")
	}
	return nil
}

// QueueDeliveryImport records an exact user-selected subset of a durable
// accepted manifest and sends it through the same bounded delivery importer.
func (s *Service) QueueDeliveryImport(ctx context.Context, receiptID int64, paths []string) error {
	if len(paths) == 0 || len(paths) > 10000 {
		return fmt.Errorf("select between 1 and 10000 manifest files")
	}
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "conflict" {
		return fmt.Errorf("delivery identity conflicts with its saved receipt")
	}
	if r.AckState == "pending" && r.ConsumerReceiptID == "" {
		return fmt.Errorf("delivery acceptance is not durably prepared; retry handoff before selecting files")
	}
	unlock := s.lockDownload(r.DownloadID)
	defer unlock()
	// Resolve can wait behind an importer or reconciliation worker. Re-read
	// the receipt after taking that serialization lock so a stale pre-lock
	// snapshot cannot reopen a receipt that has since changed identity/state.
	current, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if current.ID != r.ID || current.DownloadID != r.DownloadID || current.GenerationKey != r.GenerationKey || current.ClientID != r.ClientID || current.DownloadHandle != r.DownloadHandle || current.Delivery.RequestToken != r.Delivery.RequestToken || current.Delivery.PublicationGeneration != r.Delivery.PublicationGeneration || current.Delivery.ManifestDigest != r.Delivery.ManifestDigest || current.MappedPath != r.MappedPath || current.State == "conflict" {
		return fmt.Errorf("delivery identity changed while waiting to select files")
	}
	if current.AckState == "pending" && current.ConsumerReceiptID == "" {
		return fmt.Errorf("delivery acceptance is no longer durably prepared")
	}
	r = current
	if s.ImportRunning(r.DownloadID) {
		return fmt.Errorf("an import is already running for this delivery")
	}
	dl, err := s.db.GetDownload(ctx, r.DownloadID)
	if err != nil {
		return fmt.Errorf("the associated download row is unavailable; resolve its Activity record first: %w", err)
	}
	if dl.GenerationKey != r.GenerationKey || dl.ClientID != r.ClientID || dl.Handle != r.DownloadHandle || dl.Transfer != r.Delivery.RequestToken || dl.MediaItemID != r.MediaItemID || dl.CopyID != r.CopyID {
		return fmt.Errorf("delivery download generation changed before the selected import")
	}
	manifest := make(map[string]bool, len(r.Delivery.Files))
	for _, f := range r.Delivery.Files {
		manifest[f.Path] = true
	}
	selected := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range paths {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(raw)))
		if clean == "." || clean != raw || filepath.IsAbs(raw) || !manifest[clean] {
			return fmt.Errorf("selected path is not an exact relative path in the accepted manifest")
		}
		if !seen[clean] {
			seen[clean] = true
			selected = append(selected, filepath.Join(r.MappedPath, filepath.FromSlash(clean)))
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no manifest files were selected")
	}
	if r.State == "imported" {
		if r.AckState != "accepted" && (r.AckState != "pending" || r.ConsumerReceiptID == "") {
			return fmt.Errorf("an imported delivery must have a locally prepared Runner acceptance before selected re-import")
		}
		rows, err := s.db.DeliveryFileOutcomes(ctx, receiptID)
		if err != nil {
			return err
		}
		byPath := make(map[string]sqlite.DeliveryFileOutcome, len(rows))
		for _, row := range rows {
			byPath[row.Path] = row
		}
		unresolved := false
		for _, absolute := range selected {
			relative, err := filepath.Rel(r.MappedPath, absolute)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("selected path is outside the accepted publication")
			}
			row, ok := byPath[filepath.ToSlash(relative)]
			if !ok || (row.CleanupState != "removed" && row.Disposition != "placed" && row.Disposition != "redundant" && row.Disposition != "disposable_selected_scope") {
				unresolved = true
			}
		}
		if !unresolved {
			return fmt.Errorf("all selected manifest files already have durable outcomes")
		}
	}
	dl.ImportPath, dl.SavePath = r.MappedPath, r.Delivery.PublicationPath
	// Reopening an imported receipt must also reopen its download state. The
	// importer deliberately skips terminal imported rows during recovery.
	if r.State == "imported" {
		dl.State = "awaiting_import"
	}
	dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepManualQueued, At: time.Now().UnixMilli(), Detail: fmt.Sprintf("operator selected %d accepted manifest file(s)", len(selected)), Paths: selected})
	if r.State == "imported" {
		if err := s.db.QueueDeliveryReimport(ctx, r, dl); err != nil {
			return err
		}
	} else if err := s.db.UpdateDownloadHandoff(ctx, dl); err != nil {
		return err
	}
	cfg, err := s.db.GetDownloadClient(ctx, r.ClientID)
	if err != nil {
		return err
	}
	if !s.enqueueDeliveryImport(ctx, dl, cfg, r.ID) {
		return fmt.Errorf("import worker is unavailable or full; retry the selected import")
	}
	return nil
}
