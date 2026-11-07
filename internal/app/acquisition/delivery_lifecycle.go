package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type DeliveryLifecycleReceipt struct {
	OwnedByClientID               int64                        `json:"ownedByClientId,omitempty"`
	LifecycleTiming               *ports.RunnerLifecycleTiming `json:"lifecycleTiming,omitempty"`
	RunnerStages                  *ports.RunnerStages          `json:"runnerStages,omitempty"`
	SourceCopies                  []ports.RunnerSourceCopy     `json:"sourceCopies,omitempty"`
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
	CuratorFiles                  []DeliveryLifecycleFile      `json:"curatorFiles"`
	PendingDeleteRequests         []DeliveryDeleteRequest      `json:"pendingDeleteRequests,omitempty"`
	AttemptHistory                []DeliveryLifecycleAttempt   `json:"attemptHistory"`
}

// DeliveryLifecycleFile is the public projection of the persisted file ledger.
// Keep API casing explicit rather than exposing database struct field names.
type DeliveryLifecycleFile struct {
	ReceiptID            int64     `json:"receiptId"`
	Path                 string    `json:"path"`
	SizeBytes            int64     `json:"sizeBytes"`
	SHA256               string    `json:"sha256"`
	FileGeneration       string    `json:"fileGeneration"`
	Disposition          string    `json:"disposition"`
	CleanupState         string    `json:"cleanupState"`
	Reason               string    `json:"reason"`
	LibraryPath          string    `json:"libraryPath"`
	ManualAdditional     bool      `json:"manualAdditional"`
	CleanupError         string    `json:"cleanupError"`
	CleanupAttempts      int       `json:"cleanupAttempts"`
	CleanupBlockedSince  time.Time `json:"cleanupBlockedSince"`
	CleanupNextAttemptAt time.Time `json:"cleanupNextAttemptAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
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
	batches, err := s.db.ExplicitDeliveryDeleteBatchesPending(ctx, r.ID, 100)
	if err != nil {
		return DeliveryLifecycleReceipt{}, err
	}
	pending := make([]DeliveryDeleteRequest, 0, len(batches))
	for _, b := range batches {
		paths := []string{}
		for _, f := range b.Files {
			if !f.HistoryRecorded {
				paths = append(paths, f.Path)
			}
		}
		pending = append(pending, DeliveryDeleteRequest{ID: b.ID, Paths: paths, Error: b.RetryError})
	}
	viewState := r.State
	if r.OwnedByClientID > 0 {
		viewState = "owned_by_other_client"
	}
	return DeliveryLifecycleReceipt{OwnedByClientID: r.OwnedByClientID,
		PendingDeleteRequests: pending, RunnerStages: r.Delivery.RunnerStages, SourceCopies: r.Delivery.SourceCopies, LifecycleTiming: r.Delivery.LifecycleTiming,
		ReceiptID: r.ID, ClientID: r.ClientID, DownloadID: r.DownloadID, GenerationKey: r.GenerationKey,
		MediaItemID: r.MediaItemID, CopyID: r.CopyID, DeliveryID: r.Delivery.DeliveryID,
		PublicationGeneration: r.Delivery.PublicationGeneration, ManifestDigest: r.Delivery.ManifestDigest,
		RunnerInstanceID: r.Delivery.RunnerInstanceID, JobID: r.Delivery.JobID, RequestToken: r.Delivery.RequestToken,
		PublicationPath: r.Delivery.PublicationPath, MappedPath: r.MappedPath, ReadyAt: r.ReadyAt,
		TerminalOutcome: r.Delivery.TerminalOutcome, FailureStage: r.Delivery.FailureStage,
		FailureCause: r.Delivery.FailureCause, KeepSourceOnFailure: r.Delivery.KeepSourceOnFailure,
		State: viewState, Acceptance: r.AckState, ConsumerReceiptID: r.ConsumerReceiptID,
		Attempts: r.Attempts, BlockedSince: r.BlockedSince, NextAttemptAt: r.NextAttemptAt, Error: r.Error,
		ManifestFiles: r.Delivery.Files, SourceFileOutcomes: r.Delivery.FileOutcomes, SourceCleanup: r.Delivery.SourceCleanup,
		SourceCleanupAttempts: details.SourceAttempts, SourceCleanupBlockedSince: details.SourceBlockedSince,
		SourceCleanupNextAttemptAt: details.SourceNext, SourceCleanupError: details.SourceError,
		DirectoryCleanupState: details.DirectoryState, DirectoryCleanupAttempts: details.DirectoryAttempts,
		DirectoryCleanupBlockedSince: details.DirectoryBlockedSince, DirectoryCleanupNextAttemptAt: details.DirectoryNext,
		DirectoryCleanupError: details.DirectoryError, CuratorFiles: lifecycleFileProjection(files), AttemptHistory: attempts,
	}, nil
}

func (s *Service) DeliveryLifecycleByID(ctx context.Context, receiptID int64) (DeliveryLifecycleReceipt, error) {
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return DeliveryLifecycleReceipt{}, err
	}
	return s.deliveryLifecycleReceipt(ctx, r)
}

// RetryDeliveryHandoff re-offers the publication a v1 receipt migrated into
// (pipeline v2: the receipt no longer drives an import of its own).
func (s *Service) RetryDeliveryHandoff(ctx context.Context, receiptID int64) error {
	r, err := s.deliveryReceiptForAction(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "conflict" {
		return fmt.Errorf("delivery identity conflicts with its saved receipt")
	}
	return s.retryReceiptPublication(ctx, r, nil, 0)
}

// ErrRunnerOwnsSource refuses a request that would have Curator ask Runner to
// delete under /processing (contract section 7: no Curator-to-Runner cleanup
// commands). Runner retires its own source.
var ErrRunnerOwnsSource = errors.New("the processing source is Runner's to retire; Curator no longer asks Runner to clean it up")

// RetryRunnerSourceCleanup is refused: see ErrRunnerOwnsSource.
func (s *Service) RetryRunnerSourceCleanup(ctx context.Context, receiptID int64) error {
	if _, err := s.deliveryReceiptForAction(ctx, receiptID); err != nil {
		return err
	}
	return ErrRunnerOwnsSource
}

func (s *Service) RetryCuratorDeliveryCleanup(ctx context.Context, receiptID int64) error {
	r, err := s.deliveryReceiptForAction(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "conflict" {
		return fmt.Errorf("delivery identity conflicts with its saved receipt")
	}
	// A migrated v1 receipt is the historical cohort: no deletion touches it
	// until the M7 review approves its publication (contract section 3.4).
	if pub, ok, err := s.db.PublicationByPath(ctx, receiptPublicationPath(r)); err != nil {
		return err
	} else if ok && pub.Cohort == "historical" && pub.CleanupApprovedAt.IsZero() {
		return ErrHistoricalCohort
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
	r, err := s.deliveryReceiptForAction(ctx, receiptID)
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
	current, err := s.deliveryReceiptForAction(ctx, receiptID)
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
	return s.retryReceiptPublication(ctx, r, selected, r.DownloadID)
}

// ErrHistoricalCohort refuses an automatic-style deletion of a publication
// frozen at the pipeline v2 cutover, until the M7 review approves it.
var ErrHistoricalCohort = errors.New("this publication is in the historical cohort; nothing deletes it until its review batch is approved")

func receiptPublicationPath(r sqlite.DeliveryReceipt) string {
	if r.MappedPath != "" {
		return filepath.Clean(r.MappedPath)
	}
	return filepath.Clean(r.Delivery.PublicationPath)
}

// retryReceiptPublication imports the publication a receipt migrated into:
// all of it, or exactly the selected files (a Resolve selection). held is the
// download whose lock the caller already holds (0 for none); the per-download
// lock is not re-entrant.
func (s *Service) retryReceiptPublication(ctx context.Context, r sqlite.DeliveryReceipt, selected []string, held int64) error {
	pub, ok, err := s.db.PublicationByPath(ctx, receiptPublicationPath(r))
	if err != nil {
		return err
	}
	if !ok || pub.DownloadID == 0 {
		return fmt.Errorf("this delivery has no publication with a download row to import into")
	}
	if pub.DownloadID != held {
		unlock := s.lockDownload(pub.DownloadID)
		defer unlock()
	}
	if s.ImportRunning(pub.DownloadID) {
		return fmt.Errorf("an import is already running for this delivery")
	}
	dl, err := s.db.GetDownload(ctx, pub.DownloadID)
	if err != nil {
		return err
	}
	if len(selected) > 0 {
		dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepManualQueued, At: time.Now().UnixMilli(),
			Detail: fmt.Sprintf("operator selected %d publication file(s)", len(selected)), Paths: selected})
		if err := s.db.UpdateDownloadHandoff(ctx, dl); err != nil {
			return err
		}
	}
	if pub.State != "discovered" {
		if err := s.db.SetPublicationState(ctx, pub.ID, "discovered", ""); err != nil {
			return err
		}
	}
	var cfg ports.ClientConfig
	if dl.ClientID != 0 {
		if cfg, err = s.db.GetDownloadClient(ctx, dl.ClientID); err != nil {
			return err
		}
	}
	if !s.enqueuePublicationImportApproved(ctx, dl, cfg, pub.ID, true) {
		return fmt.Errorf("import worker is unavailable or full; retry the selected import")
	}
	return nil
}

// AbandonDeliveryDeleteRequest serializes with selected-file mutation and import
// so a late worker cannot revive the cancelled selection.
func (s *Service) AbandonDeliveryDeleteRequest(ctx context.Context, receiptID int64, batchID string) error {
	if !s.completedMu.TryLock() {
		return fmt.Errorf("completed inventory is busy; retry cancelling this deletion request shortly")
	}
	defer s.completedMu.Unlock()
	r, err := s.deliveryReceiptForAction(ctx, receiptID)
	if err != nil {
		return err
	}
	unlock, locked := s.tryLockDownload(r.DownloadID)
	if !locked {
		return fmt.Errorf("delivery mutation is active; retry cancelling this deletion request after it finishes")
	}
	defer unlock()
	// Pin the current title after taking this download's mutation lock; no
	// long-running import may hold the completed-inventory mutex via this action.
	current, err := s.deliveryReceiptForAction(ctx, receiptID)
	if err != nil {
		return err
	}
	if current.DownloadID != r.DownloadID || current.GenerationKey != r.GenerationKey {
		return sqlite.ErrDeliveryReceiptStale
	}
	r = current
	ctx, releaseTarget, locked := s.tryLockImportTarget(ctx, r.MediaItemID)
	if !locked {
		return ErrImportTargetsBusy
	}
	defer releaseTarget()
	ctx, release, locked := s.tryLockStorageDecision(ctx)
	if !locked {
		return fmt.Errorf("storage decision is busy; retry cancelling this deletion request shortly")
	}
	defer release()
	current, err = s.deliveryReceiptForAction(ctx, receiptID)
	if err != nil {
		return err
	}
	if current.MediaItemID != r.MediaItemID || current.DownloadID != r.DownloadID || current.GenerationKey != r.GenerationKey {
		return sqlite.ErrDeliveryReceiptStale
	}
	if current.State == "importing" || s.ImportRunning(current.DownloadID) {
		return fmt.Errorf("delivery import is active; wait for it to finish before cancelling deletion")
	}
	return s.db.AbandonExplicitDeliveryDelete(ctx, receiptID, batchID)
}

type DeliveryDeleteRequest struct {
	ID    string   `json:"id"`
	Paths []string `json:"paths"`
	Error string   `json:"error,omitempty"`
}

func lifecycleFileProjection(files []sqlite.DeliveryFileOutcome) []DeliveryLifecycleFile {
	out := make([]DeliveryLifecycleFile, 0, len(files))
	for _, file := range files {
		out = append(out, DeliveryLifecycleFile(file))
	}
	return out
}

func deliveryReceiptActionGuard(receipt sqlite.DeliveryReceipt) error {
	if receipt.OwnedByClientID > 0 {
		return fmt.Errorf("publication belongs to another configured Runner client (ID %d): %s", receipt.OwnedByClientID, receipt.Error)
	}
	return nil
}
func (s *Service) deliveryReceiptForAction(ctx context.Context, id int64) (sqlite.DeliveryReceipt, error) {
	receipt, err := s.db.DeliveryReceiptByID(ctx, id)
	if err != nil {
		return sqlite.DeliveryReceipt{}, err
	}
	if err := deliveryReceiptActionGuard(receipt); err != nil {
		return sqlite.DeliveryReceipt{}, err
	}
	return receipt, nil
}
