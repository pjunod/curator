package acquisition

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

const (
	deliveryPageSize       = 100
	deliveryPagesPerSweep  = 10
	deliveryReceiptsPerRun = 64
	deliveryMaxFiles       = 10000
	deliveryMaxPathBytes   = 4096
	deliveryMaxTotalBytes  = int64(2 << 40) // bound one worker's hash pass to 2 TiB
)

func deliveryAttemptError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// discoverRunnerDeliveries is intentionally a cheap control-plane step. It
// journals bounded pages, then schedules any associated receipt on the same
// two workers used for ordinary imports. It never hashes payload bytes.
func (s *Service) discoverRunnerDeliveries(ctx context.Context, cfg ports.ClientConfig, client ports.DownloadClient) {
	protocol, ok := client.(ports.DeliveryProtocol)
	if !ok {
		return
	}
	if enabled, err := s.db.RunnerDeliveryV1Enabled(ctx, cfg.ID); err == nil && enabled {
		s.rememberRunnerDelivery(cfg.ID)
	}
	after, err := s.db.DeliveryCursor(ctx, cfg.ID)
	if err != nil {
		s.log.Warn("delivery cursor read failed", "client", cfg.Name, "err", err)
		return
	}
	for pageNo := 0; pageNo < deliveryPagesPerSweep; pageNo++ {
		page, err := protocol.ListDeliveries(ctx, after, deliveryPageSize)
		if err != nil {
			s.log.Warn("delivery discovery failed", "client", cfg.Name, "err", err)
			break
		}
		if page.ProtocolVersion != 1 || len(page.Deliveries) > deliveryPageSize {
			s.log.Warn("delivery discovery returned invalid page", "client", cfg.Name)
			break
		}
		s.rememberRunnerDelivery(cfg.ID)
		if err := s.db.EnableRunnerDeliveryV1(ctx, cfg.ID); err != nil {
			s.log.Warn("Runner delivery capability could not be persisted", "client", cfg.Name, "err", err)
		}
		for _, delivery := range page.Deliveries {
			mapped := ""
			if filepath.IsAbs(delivery.PublicationPath) {
				mapped = ports.MapRemotePath(cfg.PathMappings, delivery.PublicationPath)
			}
			r, saveErr := s.db.SaveDiscoveredDelivery(ctx, cfg.ID, delivery, mapped)
			if saveErr != nil {
				s.log.Warn("delivery journal write failed", "client", cfg.Name, "delivery", delivery.DeliveryID, "err", saveErr)
				return
			}
			if r.State != "conflict" && (delivery.DeliveryID == "" || delivery.JobID == "" || delivery.RequestToken == "" || delivery.RunnerInstanceID == "" || mapped == "") {
				r.Error = "delivery is missing a stable identity, request token, Runner instance, or mapped publication path"
				r.NextAttemptAt = time.Now().Add(time.Minute)
				if err := s.db.SetDeliveryRetry(ctx, r); err != nil {
					s.log.Warn("delivery validation state write failed", "client", cfg.Name, "err", err)
				}
			}
		}
		// Persist every row before moving this durable cursor. Runner may scan
		// accepted rows while returning only pending ones, so an empty result
		// with an advancing cursor is not the end of the sequence.
		if page.NextCursor == 0 && after != 0 {
			if err := s.db.SetDeliveryCursor(ctx, cfg.ID, 0); err != nil {
				s.log.Warn("delivery cursor wrap failed", "client", cfg.Name, "err", err)
			}
			break
		}
		if page.NextCursor < after {
			s.log.Warn("delivery cursor moved backwards", "client", cfg.Name, "after", after, "next", page.NextCursor)
			break
		}
		if err := s.db.SetDeliveryCursor(ctx, cfg.ID, page.NextCursor); err != nil {
			s.log.Warn("delivery cursor write failed", "client", cfg.Name, "err", err)
			break
		}
		if page.NextCursor == after {
			if after != 0 {
				if err := s.db.SetDeliveryCursor(ctx, cfg.ID, 0); err != nil {
					s.log.Warn("delivery cursor reset failed", "client", cfg.Name, "err", err)
				}
			}
			break
		}
		after = page.NextCursor
	}
	s.scheduleDeliveryReceipts(ctx, cfg, protocol)
	s.refreshDeliverySourceCleanup(ctx, cfg, client)
	s.scheduleDeliveryFileCleanup(ctx, cfg)
}

func (s *Service) refreshDeliverySourceCleanup(ctx context.Context, cfg ports.ClientConfig, client ports.DownloadClient) {
	lookup, ok := client.(ports.DeliveryLookupProtocol)
	if !ok {
		return
	}
	receipts, err := s.db.DeliveryReceiptsNeedingSourceRefresh(ctx, cfg.ID, 4)
	if err != nil {
		s.log.Warn("Runner source-cleanup refresh list failed", "client", cfg.Name, "err", err)
		return
	}
	deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	for _, r := range receipts {
		if ctx.Err() != nil {
			return
		}
		if err := s.refreshOneDeliverySourceCleanup(deadline, lookup, r); err != nil {
			s.log.Warn("Runner source-cleanup progress refresh failed", "delivery", r.Delivery.DeliveryID, "err", err)
			// A timeout or transport outage is shared by later receipts. Stop this
			// control-plane pass instead of issuing a queue of redundant requests.
			return
		}
	}
}

// refreshOneDeliverySourceCleanup serializes automatic GET refresh with the
// explicit source-cleanup POST. It re-reads both receipt identity and local /
// provider deadlines after taking the per-receipt control lock, since the
// scheduler's initial list may have gone stale while it waited.
func (s *Service) refreshOneDeliverySourceCleanup(ctx context.Context, lookup ports.DeliveryLookupProtocol, selected sqlite.DeliveryReceipt) error {
	unlock, acquired := s.tryLockDeliverySourceControl(selected.ID)
	if !acquired {
		// A manual source-cleanup POST owns this receipt. Leave the durable due
		// time unchanged and let the next bounded refresh sweep revisit it.
		return nil
	}
	defer unlock()
	r, err := s.db.DeliveryReceiptByID(ctx, selected.ID)
	if err != nil {
		return err
	}
	if r.ClientID != selected.ClientID || r.DownloadID != selected.DownloadID || r.GenerationKey != selected.GenerationKey || r.Delivery.DeliveryID != selected.Delivery.DeliveryID || r.Delivery.RequestToken != selected.Delivery.RequestToken {
		return sqlite.ErrDeliveryReceiptStale
	}
	if r.State == "conflict" || (r.AckState != "accepted" && (r.AckState != "pending" || r.ConsumerReceiptID == "")) {
		return nil
	}
	_, localDue, err := s.db.DeliverySourceRefreshError(ctx, r.ID)
	if err != nil {
		return err
	}
	providerDue, err := s.db.DeliverySourceProviderRetryAfter(ctx, r.ID)
	if err != nil {
		return err
	}
	if localDue.After(time.Now()) || providerDue.After(time.Now()) {
		return nil
	}
	fresh, err := lookup.GetDelivery(ctx, r.Delivery.DeliveryID)
	if err != nil {
		if stateErr := s.db.FailDeliverySourceRefresh(context.WithoutCancel(ctx), r.ID, err.Error(), submissionRetryDeadline(err)); stateErr != nil {
			s.log.Warn("Runner source-cleanup refresh failure state write failed", "delivery", r.Delivery.DeliveryID, "err", stateErr)
		}
		return err
	}
	if !sameDeliveryImmutable(r.Delivery, fresh) {
		_ = s.db.SetDeliveryConflict(context.WithoutCancel(ctx), r.ID, "Runner delivery changed during source-cleanup refresh")
		return nil
	}
	retryAfter := 5 * time.Minute
	if deliverySourceCleanupTerminal(fresh.SourceCleanup) {
		retryAfter = 24 * time.Hour
	}
	return s.db.RefreshDeliverySourceCleanup(ctx, r.ID, fresh.SourceCleanup, fresh.FileOutcomes, retryAfter)
}

func deliverySourceCleanupTerminal(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	var terminal bool
	var visit func(any)
	visit = func(v any) {
		switch item := v.(type) {
		case map[string]any:
			for key, child := range item {
				if strings.EqualFold(key, "status") || strings.EqualFold(key, "state") {
					if status, ok := child.(string); ok {
						switch strings.ToLower(status) {
						case "complete", "completed", "removed", "succeeded", "success":
							terminal = true
						case "pending", "blocked", "retained", "retry", "failed":
							terminal = false
							return
						}
					}
				}
				visit(child)
				if terminal {
					return
				}
			}
		case []any:
			for _, child := range item {
				visit(child)
				if terminal {
					return
				}
			}
		}
	}
	visit(value)
	return terminal
}

func sameDeliveryImmutable(a, b ports.RunnerDelivery) bool {
	if a.Sequence != b.Sequence || a.DeliveryID != b.DeliveryID || a.PublicationGeneration != b.PublicationGeneration ||
		a.SourceArtifactID != b.SourceArtifactID || a.SourceGeneration != b.SourceGeneration ||
		a.ManifestDigest != b.ManifestDigest || a.ArtifactID != b.ArtifactID || a.JobID != b.JobID ||
		a.RequestToken != b.RequestToken || a.RunnerInstanceID != b.RunnerInstanceID || a.PublicationPath != b.PublicationPath ||
		!a.ReadyAt.Equal(b.ReadyAt) || a.TerminalOutcome != b.TerminalOutcome || a.FailureStage != b.FailureStage ||
		a.FailureCause != b.FailureCause || a.KeepSourceOnFailure != b.KeepSourceOnFailure ||
		len(a.Files) != len(b.Files) || len(a.FileOutcomes) != len(b.FileOutcomes) {
		return false
	}
	af, bf := append([]ports.RunnerFile(nil), a.Files...), append([]ports.RunnerFile(nil), b.Files...)
	sort.Slice(af, func(i, j int) bool { return bytes.Compare([]byte(af[i].Path), []byte(af[j].Path)) < 0 })
	sort.Slice(bf, func(i, j int) bool { return bytes.Compare([]byte(bf[i].Path), []byte(bf[j].Path)) < 0 })
	for i := range af {
		if af[i] != bf[i] {
			return false
		}
	}
	ao, bo := append([]ports.RunnerFileOutcome(nil), a.FileOutcomes...), append([]ports.RunnerFileOutcome(nil), b.FileOutcomes...)
	sort.Slice(ao, func(i, j int) bool { return bytes.Compare([]byte(ao[i].SourcePath), []byte(ao[j].SourcePath)) < 0 })
	sort.Slice(bo, func(i, j int) bool { return bytes.Compare([]byte(bo[i].SourcePath), []byte(bo[j].SourcePath)) < 0 })
	for i := range ao {
		ao[i].SourceCleanupStatus, bo[i].SourceCleanupStatus = "", ""
		if ao[i].SourcePath != bo[i].SourcePath || ao[i].Classification != bo[i].Classification || ao[i].Outcome != bo[i].Outcome || ao[i].Reason != bo[i].Reason || ao[i].RepairDependency != bo[i].RepairDependency || (ao[i].PublicationPath == nil) != (bo[i].PublicationPath == nil) || (ao[i].PublicationPath != nil && *ao[i].PublicationPath != *bo[i].PublicationPath) {
			return false
		}
	}
	return true
}

func (s *Service) rememberRunnerDelivery(clientID int64) {
	s.deliveryCapabilityMu.Lock()
	if s.deliveryCapabilities == nil {
		s.deliveryCapabilities = map[int64]bool{}
	}
	s.deliveryCapabilities[clientID] = true
	s.deliveryCapabilityMu.Unlock()
}

func (s *Service) runnerDeliveryEnabled(ctx context.Context, cfg ports.ClientConfig) bool {
	s.deliveryCapabilityMu.RLock()
	known := s.deliveryCapabilities[cfg.ID]
	s.deliveryCapabilityMu.RUnlock()
	if known {
		return true
	}
	enabled, err := s.db.RunnerDeliveryV1Enabled(ctx, cfg.ID)
	if err != nil {
		s.log.Warn("Runner delivery capability lookup failed", "client", cfg.Name, "err", err)
		return cfg.Type == "nzbd"
	}
	if enabled {
		s.rememberRunnerDelivery(cfg.ID)
	}
	return enabled
}

func (s *Service) scheduleDeliveryReceipts(ctx context.Context, cfg ports.ClientConfig, protocol ports.DeliveryProtocol) {
	receipts, err := s.db.DeliveryReceiptsForClient(ctx, cfg.ID, deliveryReceiptsPerRun)
	if err != nil {
		s.log.Warn("delivery journal read failed", "client", cfg.Name, "err", err)
		return
	}
	for _, r := range receipts {
		if ctx.Err() != nil {
			return
		}
		if r.State == "conflict" {
			continue
		}
		if !r.NextAttemptAt.IsZero() && r.NextAttemptAt.After(time.Now()) {
			continue
		}
		if r.State == "imported" {
			if r.AckState == "pending" {
				if err := s.completeDeliveryAcceptance(ctx, cfg, r.ID); err != nil && !errors.Is(err, ports.ErrDeliveryConflict) {
					s.log.Warn("durable Runner acknowledgement retry failed", "delivery", r.Delivery.DeliveryID, "err", err)
					// One transport timeout is enough to diagnose a shared outage.
					// The remaining receipts retain their independently due state.
					return
				}
			}
			continue
		}
		if r.DownloadID == 0 {
			matches, err := s.db.FindDownloadsForDelivery(ctx, cfg.ID, r.Delivery.JobID, r.Delivery.RequestToken)
			if err != nil {
				s.deliveryRetry(ctx, &r, "download association lookup failed: "+err.Error())
				continue
			}
			if len(matches) != 1 {
				s.deliveryRetry(ctx, &r, fmt.Sprintf("delivery remains unresolved: request token and Runner job matched %d downloads", len(matches)))
				continue
			}
			dl := matches[0]
			if dl.GenerationKey == "" || dl.Handle != r.Delivery.JobID || dl.Transfer != r.Delivery.RequestToken {
				s.deliveryRetry(ctx, &r, "delivery association lacks the expected download generation, handle, or transfer token")
				continue
			}
			r.DownloadID, r.GenerationKey, r.DownloadHandle = dl.ID, dl.GenerationKey, dl.Handle
			r.MediaItemID, r.CopyID = dl.MediaItemID, dl.CopyID
			r.Error = ""
			r.NextAttemptAt = time.Time{}
			if err := s.db.AssociateDelivery(ctx, r); err != nil {
				s.log.Warn("delivery association journal failed", "delivery", r.Delivery.DeliveryID, "err", err)
				continue
			}
		}
		dl, err := s.db.GetDownload(ctx, r.DownloadID)
		if err != nil {
			s.deliveryRetry(ctx, &r, "accepted delivery's associated download row is unavailable: "+err.Error())
			continue
		}
		if dl.ClientID != cfg.ID || dl.GenerationKey != r.GenerationKey || dl.Handle != r.DownloadHandle || dl.Transfer != r.Delivery.RequestToken || dl.MediaItemID != r.MediaItemID || dl.CopyID != r.CopyID {
			s.deliveryRetry(ctx, &r, "delivery association no longer matches its recorded download generation and copy")
			continue
		}
		if dl.State == "imported" {
			if r.State == "ready" && hasQueuedDeliverySelection(dl) {
				s.queueDeliveryImport(ctx, cfg, dl, r)
				continue
			}
			if r.AckState == "accepted" {
				s.markDeliveryImported(ctx, r.ID)
				continue
			}
			if r.AckState == "pending" {
				s.queueDeliveryImport(ctx, cfg, dl, r)
				continue
			}
			s.deliveryRetry(ctx, &r, "download was imported before this Runner delivery was durably accepted; automatic adoption is blocked")
			continue
		}
		s.queueDeliveryImport(ctx, cfg, dl, r)
	}
}

func hasQueuedDeliverySelection(dl sqlite.Download) bool {
	for i := len(dl.Handoff) - 1; i >= 0; i-- {
		if dl.Handoff[i].Step == stepManualQueued {
			return len(dl.Handoff[i].Paths) > 0
		}
	}
	return false
}

func (s *Service) queueDeliveryImport(ctx context.Context, cfg ports.ClientConfig, dl sqlite.Download, r sqlite.DeliveryReceipt) {
	if s.ImportRunning(dl.ID) {
		r.NextAttemptAt = time.Now().Add(5 * time.Minute)
		if err := s.db.SetDeliveryRetry(ctx, r); err != nil {
			s.log.Warn("delivery worker lease write failed", "delivery", r.Delivery.DeliveryID, "err", err)
		}
		return
	}
	// Persist a short lease before enqueue so repeated polls cannot keep the
	// same oldest rows at the front of the bounded journal query.
	r.NextAttemptAt = time.Now().Add(5 * time.Minute)
	if err := s.db.SetDeliveryRetry(ctx, r); err != nil {
		s.log.Warn("delivery worker lease write failed", "delivery", r.Delivery.DeliveryID, "err", err)
		return
	}
	if !s.enqueueDeliveryImport(ctx, dl, cfg, r.ID) {
		r.NextAttemptAt = time.Time{}
		if err := s.db.SetDeliveryRetry(ctx, r); err != nil {
			s.log.Warn("delivery queue retry write failed", "delivery", r.Delivery.DeliveryID, "err", err)
		}
		s.log.Warn("delivery import queue full; will retry", "client", cfg.Name, "delivery", r.Delivery.DeliveryID)
	}
}

func (s *Service) deliveryRetry(ctx context.Context, r *sqlite.DeliveryReceipt, message string, cause ...error) {
	r.Error = message
	if r.BlockedSince.IsZero() {
		r.BlockedSince = time.Now()
	}
	r.Attempts++
	delay := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}[min(r.Attempts-1, 3)]
	r.NextAttemptAt = time.Now().Add(delay)
	var providerDeadline time.Time
	if len(cause) > 0 {
		providerDeadline = submissionRetryDeadline(cause[0])
	}
	if providerDeadline.After(r.NextAttemptAt) {
		r.NextAttemptAt = providerDeadline
	}
	if time.Since(r.BlockedSince) >= 30*time.Minute {
		s.log.Warn("Runner delivery has remained blocked for over 30 minutes", "delivery", r.Delivery.DeliveryID, "reason", message)
	}
	if err := s.db.SetDeliveryRetry(ctx, *r, providerDeadline); err != nil {
		s.log.Warn("delivery retry state write failed", "delivery", r.Delivery.DeliveryID, "err", err)
	}
}

func (s *Service) markDeliveryImported(ctx context.Context, receiptID int64) {
	_, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		s.log.Warn("delivery receipt recovery failed", "receipt", receiptID, "err", err)
		return
	}
	if err := s.db.SetDeliveryImported(ctx, receiptID); err != nil {
		s.log.Warn("delivery import receipt write failed", "receipt", receiptID, "err", err)
	}
}

// completeDeliveryAcceptance retries only the small idempotent Runner ack.
// A receipt reaches ack_state=pending only after its manifest was validated
// and the stable consumer receipt ID was committed locally.
func (s *Service) completeDeliveryAcceptance(ctx context.Context, cfg ports.ClientConfig, receiptID int64) error {
	unlock := s.lockDeliveryAcknowledgement(receiptID)
	defer unlock()
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.AckState == "accepted" {
		return nil
	}
	if r.AckState != "pending" || r.ConsumerReceiptID == "" {
		return fmt.Errorf("delivery acceptance has not been prepared")
	}
	due, err := s.db.DeliveryAcknowledgementRetryDue(ctx, receiptID, time.Now())
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	event, err := s.db.BeginDeliveryAttempt(ctx, receiptID, "ack")
	if err != nil {
		return fmt.Errorf("record delivery acknowledgement attempt: %w", err)
	}
	result := "succeeded"
	var attemptErr error
	defer func() {
		if attemptErr != nil {
			result = "failed"
			if errors.Is(attemptErr, ports.ErrDeliveryConflict) {
				result = "conflict"
			}
		}
		if finishErr := s.db.FinishDeliveryAttempt(ctx, event.ID, result, deliveryAttemptError(attemptErr)); finishErr != nil {
			s.log.Error("delivery acknowledgement attempt history could not be finished", "receipt", receiptID, "err", finishErr)
		}
	}()
	client := s.newClient(cfg)
	protocol, ok := client.(ports.DeliveryProtocol)
	if !ok {
		attemptErr = fmt.Errorf("download client lost delivery protocol capability")
		return attemptErr
	}
	ack := ports.RunnerDeliveryAcceptance{ProtocolVersion: 1, PublicationGeneration: r.Delivery.PublicationGeneration, ManifestDigest: r.Delivery.ManifestDigest, ConsumerReceiptID: r.ConsumerReceiptID}
	if attemptErr = protocol.AcceptDelivery(ctx, r.Delivery.DeliveryID, ack); attemptErr != nil {
		if errors.Is(attemptErr, ports.ErrDeliveryConflict) {
			_ = s.db.SetDeliveryConflict(ctx, r.ID, attemptErr.Error())
			return attemptErr
		}
		s.deliveryRetry(ctx, &r, "Runner acceptance is pending: "+attemptErr.Error(), attemptErr)
		return attemptErr
	}
	attemptErr = s.db.SetDeliveryAccepted(ctx, r)
	return attemptErr
}

func (s *Service) runDeliveryImport(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, receiptID int64) error {
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "imported" {
		if r.AckState == "pending" {
			return s.completeDeliveryAcceptance(ctx, cfg, receiptID)
		}
		return nil
	}
	event, err := s.db.BeginDeliveryAttempt(ctx, receiptID, "import")
	if err != nil {
		return fmt.Errorf("record delivery import attempt: %w", err)
	}
	err = s.runDeliveryImportAttempt(ctx, dl, cfg, receiptID)
	result := "succeeded"
	if err != nil {
		result = "failed"
	}
	if finishErr := s.db.FinishDeliveryAttempt(ctx, event.ID, result, deliveryAttemptError(err)); finishErr != nil {
		if err == nil {
			return fmt.Errorf("finish delivery import attempt history: %w", finishErr)
		}
		s.log.Error("delivery import attempt history could not be finished", "receipt", receiptID, "err", finishErr)
	}
	return err
}

func (s *Service) runDeliveryImportAttempt(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, receiptID int64) error {
	r, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if r.State == "imported" && r.AckState == "pending" {
		return s.completeDeliveryAcceptance(ctx, cfg, receiptID)
	}
	if r.State == "conflict" || r.DownloadID != dl.ID || r.GenerationKey != dl.GenerationKey || r.MediaItemID != dl.MediaItemID || r.CopyID != dl.CopyID {
		return fmt.Errorf("delivery association changed before import")
	}
	var files []verifiedDeliveryFile
	if r.AckState != "accepted" && r.AckState != "pending" {
		legacy, fenceErr := s.db.LegacyCleanupReview(ctx, r.GenerationKey, r.MappedPath)
		if fenceErr != nil {
			return fenceErr
		}
		if legacy {
			s.deliveryRetry(ctx, &r, "automatic Runner delivery import is blocked for the legacy cleanup review cohort")
			return fmt.Errorf("delivery belongs to the legacy cleanup review cohort")
		}
		files, err = s.verifyDeliveryManifest(ctx, cfg, r)
		if err != nil {
			s.deliveryRetry(ctx, &r, err.Error())
			return err
		}
		if r.ConsumerReceiptID == "" {
			raw := make([]byte, 24)
			if _, err := rand.Read(raw); err != nil {
				s.deliveryRetry(ctx, &r, "could not create stable acceptance receipt: "+err.Error())
				return err
			}
			r.ConsumerReceiptID = hex.EncodeToString(raw)
		}
		r.AckState, r.State, r.Error = "pending", "ready", ""
		r.NextAttemptAt = time.Now().Add(time.Minute)
		if err := s.db.PrepareDeliveryAcceptance(ctx, r); err != nil {
			return err
		}
	}
	if files == nil {
		files, err = s.verifyDeliveryManifest(ctx, cfg, r)
		if err != nil {
			s.deliveryRetry(ctx, &r, err.Error())
			return err
		}
	}
	if r.AckState == "pending" {
		if ackErr := s.completeDeliveryAcceptance(ctx, cfg, receiptID); errors.Is(ackErr, ports.ErrDeliveryConflict) {
			return ackErr
		}
		// A transport failure leaves the durable pending receipt in place, but
		// the locally verified publication can still be imported. A 409 is
		// authoritative and is handled above before any library mutation.
		r, err = s.db.DeliveryReceiptByID(ctx, receiptID)
		if err != nil {
			return err
		}
		if r.State == "conflict" {
			return fmt.Errorf("runner rejected the accepted delivery tuple")
		}
	}
	if len(files) > 0 {
		if err := s.recheckDeliveryFiles(ctx, r, files); err != nil {
			s.deliveryRetry(ctx, &r, err.Error())
			return err
		}
	}
	latestReceipt, err := s.db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		return err
	}
	if latestReceipt.State == "conflict" || latestReceipt.Delivery.PublicationGeneration != r.Delivery.PublicationGeneration || latestReceipt.Delivery.ManifestDigest != r.Delivery.ManifestDigest || latestReceipt.Delivery.PublicationPath != r.Delivery.PublicationPath {
		return fmt.Errorf("runner delivery tuple changed before import")
	}
	r = latestReceipt
	// Re-read and bind the download context after the network acknowledgement.
	fresh, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil {
		return err
	}
	if fresh.GenerationKey != r.GenerationKey || fresh.Transfer != r.Delivery.RequestToken || fresh.Handle != r.DownloadHandle || fresh.MediaItemID != r.MediaItemID || fresh.CopyID != r.CopyID {
		return fmt.Errorf("delivery download generation changed after acceptance")
	}
	fresh.ImportPath, fresh.SavePath = r.MappedPath, r.Delivery.PublicationPath
	if fresh.State != "importing" {
		s.advance(ctx, &fresh, "importing", 1, "", stepImporting, "validating and importing accepted Runner manifest")
	}
	item, err := s.db.GetMediaItemFull(ctx, fresh.MediaItemID)
	if err != nil {
		return err
	}
	priorRows, err := s.db.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil {
		return err
	}
	priorByPath := make(map[string]sqlite.DeliveryFileOutcome, len(priorRows))
	for _, row := range priorRows {
		priorByPath[row.Path] = row
	}
	selected := make([]string, 0, len(files))
	expectedDigests := make(map[string]string, len(files))
	for _, f := range files {
		if deliveryImportablePath(item.Kind, f.absolute) {
			selected = append(selected, f.absolute)
		}
		expectedDigests[f.absolute] = f.digest
	}
	manual := fresh.Intent == "manual_additional"
	for i := len(fresh.Handoff) - 1; i >= 0; i-- {
		if fresh.Handoff[i].Step == stepManualQueued {
			manual = true
			requested := fresh.Handoff[i].Paths
			if len(requested) == 0 {
				return fmt.Errorf("manual delivery import has no persisted selected paths")
			}
			manifestPaths := make(map[string]bool, len(r.Delivery.Files))
			for _, f := range r.Delivery.Files {
				manifestPaths[filepath.Clean(filepath.Join(r.MappedPath, filepath.FromSlash(f.Path)))] = true
			}
			verifiedPaths := make(map[string]bool, len(files))
			for _, f := range files {
				verifiedPaths[filepath.Clean(f.absolute)] = true
			}
			selected = selected[:0]
			for _, p := range requested {
				p = filepath.Clean(p)
				if !manifestPaths[p] {
					return fmt.Errorf("manually selected path is absent from the accepted delivery manifest: %s", p)
				}
				if verifiedPaths[p] && deliveryImportablePath(item.Kind, p) {
					selected = append(selected, p)
				}
			}
			if len(selected) == 0 && !manualSelectionAlreadyResolved(requested, r, priorByPath) {
				return fmt.Errorf("manual delivery selection contains no importable media files")
			}
			break
		}
	}
	selectedSet := map[string]bool{}
	for _, p := range selected {
		selectedSet[filepath.Clean(p)] = true
	}
	verifiedByPath := make(map[string]verifiedDeliveryFile, len(files))
	for _, f := range files {
		verifiedByPath[f.relative] = f
	}
	manifest := make([]sqlite.DeliveryManifestFile, 0, len(r.Delivery.Files))
	for _, f := range r.Delivery.Files {
		generation := ""
		if verified, ok := verifiedByPath[f.Path]; ok {
			generation = verified.generation
		} else if prior, ok := priorByPath[f.Path]; ok {
			generation = prior.FileGeneration
		}
		manifest = append(manifest, sqlite.DeliveryManifestFile{Path: f.Path, SizeBytes: f.SizeBytes, SHA256: f.SHA256, FileGeneration: generation})
	}
	selectedManifest := map[string]bool{}
	for _, f := range files {
		if selectedSet[filepath.Clean(f.absolute)] {
			selectedManifest[f.relative] = true
		}
	}
	var selectedPtr map[string]bool
	if manual {
		selectedPtr = selectedManifest
	}
	if err := s.db.EnsureDeliveryFileOutcomes(ctx, receiptID, manifest, selectedPtr, fresh.Intent == "manual_additional"); err != nil {
		return fmt.Errorf("persist delivery file ledger: %w", err)
	}
	acceptedGenerations := make(map[string]string, len(files))
	for _, file := range files {
		acceptedGenerations[filepath.Clean(file.absolute)] = file.generation
	}
	ctx = withExpectedSourceDigests(ctx, expectedDigests)
	ctx = withAcceptedSourceGenerations(ctx, acceptedGenerations)
	ctx = withAcceptedPublicationGeneration(ctx, r.Delivery.PublicationGeneration)
	ctx = withIndependentPlacementCopy(ctx)
	var result ImportResult
	if len(selected) > 0 {
		result, err = s.importDownloadFiles(ctx, fresh, r.MappedPath, selected, manual)
	}
	if err == nil {
		// Give the sidecar matcher the full verified manifest. It only places a
		// sidecar when it can associate it with media imported in this attempt
		// or with a previously committed media ledger row. This lets a manual
		// Resolve select one media file and still bring along its matching
		// subtitle/artwork, while leaving sidecars for unselected siblings alone.
		result, err = s.importDeliverySidecars(ctx, r, files, result)
	}
	if ledgerErr := s.recordDeliveryImportOutcomes(ctx, r, files, result); ledgerErr != nil {
		s.log.Error("accepted delivery file outcomes could not be recorded", "delivery", r.Delivery.DeliveryID, "err", ledgerErr)
		if err == nil {
			err = ledgerErr
		}
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.failImport(ctx, &fresh, err.Error())
		}
		s.deliveryRetry(ctx, &r, "accepted delivery import is pending: "+err.Error(), err)
		return err
	}
	detail := fmt.Sprintf("imported %d file(s) from accepted Runner manifest", result.Imported)
	if manual {
		detail = fmt.Sprintf("manually imported %d selected file(s) from accepted Runner manifest", result.Imported)
	}
	if skipped := result.Skipped(); len(skipped) > 0 {
		detail += fmt.Sprintf("; %d skipped — %s", len(skipped), result.reasons(3))
	}
	s.advance(ctx, &fresh, "imported", 1, "", stepImported, detail)
	s.InvalidateWanted()
	if r.AckState == "pending" {
		if ackErr := s.completeDeliveryAcceptance(ctx, cfg, receiptID); errors.Is(ackErr, ports.ErrDeliveryConflict) {
			return ackErr
		}
		// Local publication can finish during a Runner outage; the scheduler
		// retries this acknowledgement from the persisted receipt without needing
		// the source path or Activity row to remain available.
	}
	if err := s.db.SetDeliveryImported(ctx, r.ID); err != nil {
		return err
	}
	return nil
}

func deliveryImportablePath(kind domain.MediaKind, name string) bool {
	if kind == domain.KindBook {
		return filename.IsBook(name)
	}
	return filename.IsVideo(name)
}

func manualSelectionAlreadyResolved(requested []string, r sqlite.DeliveryReceipt, prior map[string]sqlite.DeliveryFileOutcome) bool {
	if len(requested) == 0 {
		return false
	}
	manifest := make(map[string]string, len(r.Delivery.Files))
	for _, f := range r.Delivery.Files {
		manifest[filepath.Clean(filepath.Join(r.MappedPath, filepath.FromSlash(f.Path)))] = f.Path
	}
	for _, path := range requested {
		relative, ok := manifest[filepath.Clean(path)]
		if !ok {
			return false
		}
		row, ok := prior[relative]
		if !ok || (row.CleanupState != "removed" && row.Disposition != "placed" && row.Disposition != "redundant" && row.Disposition != "disposable_selected_scope") {
			return false
		}
	}
	return true
}

type verifiedDeliveryFile struct {
	relative, absolute          string
	root, relativeRoot          string
	size                        int64
	digest                      string
	generation                  string
	rootInfo, payloadInfo, info os.FileInfo
}

func (s *Service) verifyDeliveryManifest(ctx context.Context, cfg ports.ClientConfig, r sqlite.DeliveryReceipt) ([]verifiedDeliveryFile, error) {
	v := r.Delivery
	if len(v.Files) == 0 || len(v.Files) > deliveryMaxFiles {
		return nil, fmt.Errorf("runner delivery manifest has an invalid file count")
	}
	if !validSHA256(v.ManifestDigest) {
		return nil, fmt.Errorf("runner delivery manifest digest is malformed")
	}
	if !filepath.IsAbs(r.MappedPath) || !filepath.IsAbs(v.PublicationPath) {
		return nil, fmt.Errorf("runner publication path is not absolute")
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return nil, err
	}
	root, rel, err := deliveryRootAndRelative(r.MappedPath, roots)
	if err != nil {
		return nil, err
	}
	if err := s.checkStorageIdentity(ctx, r.MappedPath); err != nil {
		return nil, err
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open verified completed root: %w", err)
	}
	defer func() { _ = rootHandle.Close() }()
	rootInfo, err := rootHandle.Stat(".")
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("completed root is unavailable")
	}
	payload, err := openDeliveryDirectoryNoSymlinks(rootHandle, rel)
	if err != nil {
		return nil, fmt.Errorf("open Runner publication within completed root: %w", err)
	}
	defer func() { _ = payload.Close() }()
	info, err := payload.Stat(".")
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("runner publication is not an available directory")
	}
	if err := s.db.EnsureDeliveryDirectoryIdentity(ctx, r.ID, deliveryDirectoryObjectIdentity(info)); err != nil {
		return nil, fmt.Errorf("runner publication directory no longer matches its accepted generation")
	}
	files := append([]ports.RunnerFile(nil), v.Files...)
	// The wire order is not trusted; canonical digest ordering is raw UTF-8.
	sort.Slice(files, func(i, j int) bool { return bytes.Compare([]byte(files[i].Path), []byte(files[j].Path)) < 0 })
	seen := map[string]bool{}
	var total int64
	for _, f := range files {
		if !safeDeliveryRelativePath(f.Path) || len([]byte(f.Path)) > deliveryMaxPathBytes || !utf8.ValidString(f.Path) {
			return nil, fmt.Errorf("runner manifest contains an unsafe relative path")
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("runner manifest contains duplicate path %q", f.Path)
		}
		seen[f.Path] = true
		if f.SizeBytes < 0 || f.SizeBytes > deliveryMaxTotalBytes-total || !validSHA256(f.SHA256) {
			return nil, fmt.Errorf("runner manifest has invalid file size or SHA-256 for %q", f.Path)
		}
		total += f.SizeBytes
	}
	digest, err := runnerManifestDigest(files)
	if err != nil {
		return nil, err
	}
	if digest != v.ManifestDigest {
		return nil, fmt.Errorf("runner manifest digest does not match its file list")
	}
	ledgerRows, err := s.db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	ledgerByPath := make(map[string]sqlite.DeliveryFileOutcome, len(ledgerRows))
	for _, row := range ledgerRows {
		ledgerByPath[row.Path] = row
	}
	verified := make([]verifiedDeliveryFile, 0, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prior, exists := ledgerByPath[f.Path]
		if exists && (prior.CleanupState == "removed" || prior.Disposition == "placed" || prior.Disposition == "redundant" || prior.Disposition == "disposable_selected_scope") {
			continue
		}
		verifiedFile, err := func() (verifiedDeliveryFile, error) {
			fileParent, file, named, err := openDeliveryFileNoSymlinks(payload, filepath.FromSlash(f.Path))
			if err != nil {
				return verifiedDeliveryFile{}, fmt.Errorf("open manifest file %q: %w", f.Path, err)
			}
			defer func() { _ = fileParent.Close() }()
			defer func() { _ = file.Close() }()
			st, err := file.Stat()
			if err != nil {
				return verifiedDeliveryFile{}, err
			}
			if !st.Mode().IsRegular() || st.Size() != f.SizeBytes || !os.SameFile(named, st) {
				return verifiedDeliveryFile{}, fmt.Errorf("manifest file %q size or type changed", f.Path)
			}
			generation := deliveryFileGeneration(named)
			if exists && prior.FileGeneration != "" && prior.FileGeneration != generation {
				return verifiedDeliveryFile{}, fmt.Errorf("manifest file %q was replaced after its accepted observation", f.Path)
			}
			h := sha256.New()
			n, copyErr := copyHashContext(ctx, h, file)
			after, statErr := file.Stat()
			if copyErr != nil {
				return verifiedDeliveryFile{}, fmt.Errorf("hash manifest file %q: %w", f.Path, copyErr)
			}
			if statErr != nil {
				return verifiedDeliveryFile{}, statErr
			}
			if !os.SameFile(named, after) || deliveryFileGeneration(after) != generation || n != f.SizeBytes || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
				return verifiedDeliveryFile{}, fmt.Errorf("manifest file %q failed size or SHA-256 verification", f.Path)
			}
			return verifiedDeliveryFile{relative: f.Path, absolute: filepath.Join(r.MappedPath, filepath.FromSlash(f.Path)), root: root, relativeRoot: rel, size: f.SizeBytes, digest: f.SHA256, generation: generation, rootInfo: rootInfo, payloadInfo: info, info: st}, nil
		}()
		if err != nil {
			return nil, err
		}
		verified = append(verified, verifiedFile)
	}
	return verified, nil
}

// Reopen through os.Root and bind every component to the captured identities
// and content before handing the absolute paths to the established placement
// code. That code independently rejects symlink escapes and rechecks type and
// size while it copies.
func (s *Service) recheckDeliveryFiles(ctx context.Context, r sqlite.DeliveryReceipt, files []verifiedDeliveryFile) error {
	if len(files) == 0 {
		return fmt.Errorf("runner delivery manifest is empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkStorageIdentity(ctx, r.MappedPath); err != nil {
		return err
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return err
	}
	root, rel, err := deliveryRootAndRelative(r.MappedPath, roots)
	if err != nil {
		return err
	}
	first := files[0]
	if filepath.Clean(root) != filepath.Clean(first.root) || filepath.Clean(rel) != filepath.Clean(first.relativeRoot) {
		return fmt.Errorf("completed root changed during delivery verification")
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	rootInfo, err := anchor.Stat(".")
	if err != nil || !rootInfo.IsDir() || !os.SameFile(first.rootInfo, rootInfo) {
		return fmt.Errorf("completed root changed during delivery verification")
	}
	payload, err := openDeliveryDirectoryNoSymlinks(anchor, rel)
	if err != nil {
		return err
	}
	defer func() { _ = payload.Close() }()
	payloadInfo, err := payload.Stat(".")
	if err != nil || !payloadInfo.IsDir() || !os.SameFile(first.payloadInfo, payloadInfo) {
		return fmt.Errorf("runner publication directory changed during delivery verification")
	}
	for _, expected := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent, f, named, err := openDeliveryFileNoSymlinks(payload, filepath.FromSlash(expected.relative))
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			_ = parent.Close()
			return err
		}
		if !st.Mode().IsRegular() || !os.SameFile(named, st) || !os.SameFile(expected.info, st) || st.Size() != expected.size || deliveryFileGeneration(st) != expected.generation {
			_ = f.Close()
			_ = parent.Close()
			return fmt.Errorf("manifest file %q changed after verification", expected.relative)
		}
		h := sha256.New()
		n, copyErr := copyHashContext(ctx, h, f)
		closeErr := f.Close()
		parentCloseErr := parent.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if parentCloseErr != nil {
			return parentCloseErr
		}
		if n != expected.size || hex.EncodeToString(h.Sum(nil)) != expected.digest {
			return fmt.Errorf("manifest file %q content changed after verification", expected.relative)
		}
	}
	return nil
}

func copyHashContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// runnerManifestDigest is the shared Runner/Curator v1 canonical encoding.
// Length is the UTF-8 byte count, not rune count; path sorting is bytewise.
func runnerManifestDigest(files []ports.RunnerFile) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("runner delivery manifest is empty")
	}
	ordered := append([]ports.RunnerFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return bytes.Compare([]byte(ordered[i].Path), []byte(ordered[j].Path)) < 0 })
	h := sha256.New()
	_, _ = h.Write([]byte("runner-manifest-v1\n"))
	for _, f := range ordered {
		if !utf8.ValidString(f.Path) {
			return "", fmt.Errorf("runner manifest path is not valid UTF-8")
		}
		_, _ = io.WriteString(h, strconv.Itoa(len([]byte(f.Path)))+":")
		_, _ = h.Write([]byte(f.Path))
		_, _ = h.Write([]byte("\n"))
		_, _ = io.WriteString(h, strconv.FormatInt(f.SizeBytes, 10)+"\n"+f.SHA256+"\n")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeDeliveryRelativePath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validSHA256(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func deliveryRootAndRelative(payload string, roots []string) (string, string, error) {
	// Mapped paths can use a host alias such as /var/folders while the
	// configured root was canonicalized through /private/var. Resolve the
	// existing path's aliases before the lexical containment check; otherwise
	// valid local Runner publications fail closed even though both paths name
	// the same directory tree.
	payload = canonicalCompletedPath(filepath.Clean(payload))
	for _, root := range roots {
		root = canonicalCompletedPath(filepath.Clean(root))
		if !within(root, payload) || root == payload {
			continue
		}
		rel, err := filepath.Rel(root, payload)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return root, rel, nil
		}
	}
	return "", "", fmt.Errorf("mapped Runner publication is outside configured completed storage")
}
