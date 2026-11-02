package acquisition

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

var ErrImportControl = errors.New("import control could not be reconciled")

type importAttemptContextKey struct{}
type deliveryAttemptPlanKey struct{}

func withImportAttempt(ctx context.Context, key sqlite.ImportAttemptKey) context.Context {
	return context.WithValue(ctx, importAttemptContextKey{}, key)
}
func importAttemptFrom(ctx context.Context) (sqlite.ImportAttemptKey, bool) {
	key, ok := ctx.Value(importAttemptContextKey{}).(sqlite.ImportAttemptKey)
	return key, ok
}

func importControlError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sqlite.ErrImportCancelled) {
		return errors.Join(ErrImportControl, context.Canceled, err)
	}
	return fmt.Errorf("%w: %w", ErrImportControl, err)
}

func (s *Service) ensureImportContext(ctx context.Context, dl sqlite.Download) (context.Context, error) {
	if dl.ID == 0 {
		return ctx, nil
	}
	key, bound := importAttemptFrom(ctx)
	a, err := s.db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		return ctx, importControlError(err)
	}
	if bound && key != a.ImportAttemptKey {
		return ctx, importControlError(sqlite.ErrImportAttemptChanged)
	}
	if a.Outcome == "pending" || a.Outcome == "cancelled" {
		return ctx, importControlError(sqlite.ErrImportCancelled)
	}
	if a.Outcome == "completed" {
		return ctx, importControlError(sqlite.ErrImportCompleted)
	}
	return withImportAttempt(ctx, a.ImportAttemptKey), nil
}

func (s *Service) completeBoundImport(ctx context.Context, detail string) error {
	key, ok := importAttemptFrom(ctx)
	if !ok {
		return nil
	}
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return importControlError(s.db.CompleteImportAttempt(local, key, detail))
}

// beginInlineImport has the same ticket and epoch contract as asynchronous
// execution. Its caller already holds the canonical download mutex.
func (s *Service) beginInlineImport(ctx context.Context, dl sqlite.Download, key sqlite.ImportAttemptKey) (context.Context, func(), error) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	a, err := s.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		return ctx, nil, importControlError(err)
	}
	if a.ImportAttemptKey != key {
		return ctx, nil, importControlError(sqlite.ErrImportAttemptChanged)
	}
	if a.Outcome != "" {
		return ctx, nil, importControlError(sqlite.ErrImportCancelled)
	}
	if s.importJobs == nil {
		s.importJobs = map[int64]*importTicket{}
	}
	if s.importJobs[dl.ID] != nil {
		return ctx, nil, fmt.Errorf("import is already running or queued")
	}
	bound, cancel := context.WithCancel(withImportAttempt(ctx, key))
	ticket := &importTicket{cancel: cancel, attempt: key}
	s.importJobs[dl.ID] = ticket
	return bound, func() {
		s.finishCancelledImport(bound, key)
		cancel()
		s.importMu.Lock()
		defer s.importMu.Unlock()
		if s.importJobs[dl.ID] == ticket {
			delete(s.importJobs, dl.ID)
		}
	}, nil
}

// finishCancelledImport runs only after the worker's filesystem calls and
// descriptor closes actually returned. The caller owns the download mutex.
func (s *Service) finishCancelledImport(ctx context.Context, key sqlite.ImportAttemptKey) {
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := s.db.FinalizeImportCancellation(local, key)
	if err != nil && !errors.Is(err, sqlite.ErrImportAttemptChanged) && !errors.Is(err, sqlite.ErrImportPublicationPending) {
		s.log.Error("import cancellation remains pending", "download", key.DownloadID, "err", err)
	}
}

type ImportCancellationResult struct {
	Status         string               `json:"status"`
	Attempt        sqlite.ImportAttempt `json:"attempt"`
	ReviewRequired bool                 `json:"reviewRequired"`
}

func (s *Service) CancelImportAttempt(ctx context.Context, id int64, expected *sqlite.ImportAttemptKey) (ImportCancellationResult, error) {
	local, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	dl, err := s.db.GetDownload(local, id)
	if err != nil {
		return ImportCancellationResult{}, err
	}
	a, err := s.db.EnsureImportAttempt(local, id, dl.GenerationKey)
	if err != nil {
		return ImportCancellationResult{}, err
	}
	result := ImportCancellationResult{Attempt: a}
	if expected != nil && *expected != a.ImportAttemptKey {
		return result, sqlite.ErrImportAttemptChanged
	}
	if a.Outcome == "completed" {
		result.Status = "completed"
		return result, sqlite.ErrImportCompleted
	}
	if a.Outcome == "cancelled" {
		result.Status = "cancelled"
		result.ReviewRequired = a.RetainedPublications > 0
		return result, nil
	}
	s.importMu.Lock()
	ticket := s.importJobs[id]
	s.importMu.Unlock()
	if ticket == nil && dl.State != "importing" && dl.State != "downloaded" && dl.State != "awaiting_import" && (dl.State != "failed" || (dl.ImportPath == "" && dl.SavePath == "")) {
		return result, fmt.Errorf("download is not importing")
	}
	unlock, quiescent := s.tryLockDownload(id)
	if quiescent {
		defer unlock()
	}
	a, err = s.db.RequestImportCancellation(local, a.ImportAttemptKey)
	if err != nil {
		return result, err
	}
	result.Attempt = a
	if a.Outcome == "completed" {
		result.Status = "completed"
		return result, sqlite.ErrImportCompleted
	}
	// Registration rechecks durable intent while holding importMu. Together
	// with this post-commit signal, a request cannot miss a ticket created between
	// its initial observation and the durable commit.
	s.importMu.Lock()
	current := s.importJobs[id]
	s.importMu.Unlock()
	if current != nil && current.attempt == a.ImportAttemptKey {
		current.cancel()
	}
	result.Status = "cancel_requested"
	if !quiescent {
		return result, nil
	}
	a, err = s.db.FinalizeImportCancellation(local, a.ImportAttemptKey)
	result.Attempt = a
	if errors.Is(err, sqlite.ErrImportPublicationPending) {
		result.ReviewRequired = true
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if a.Outcome == "completed" {
		result.Status = "completed"
		return result, sqlite.ErrImportCompleted
	}
	result.Status = "cancelled"
	result.ReviewRequired = a.RetainedPublications > 0
	s.importMu.Lock()
	if current := s.importJobs[id]; current != nil && current.attempt == a.ImportAttemptKey {
		current.cancel()
		delete(s.importJobs, id)
	}
	s.importMu.Unlock()
	return result, nil
}

// ReviewImportCancellation requires an explicit external-fencing attestation,
// then proves local quiescence with the same mutexes used by actual writers.
func (s *Service) ReviewImportCancellation(ctx context.Context, key sqlite.ImportAttemptKey, action string, ids []string, acknowledged bool) (sqlite.ImportAttempt, error) {
	if !acknowledged {
		return sqlite.ImportAttempt{}, fmt.Errorf("explicit fencing and retained-byte review acknowledgement required")
	}
	unlock, ok := s.tryLockDownload(key.DownloadID)
	if !ok {
		return sqlite.ImportAttempt{}, sqlite.ErrImportPublicationPending
	}
	defer unlock()
	dl, err := s.db.GetDownload(ctx, key.DownloadID)
	if err != nil {
		return sqlite.ImportAttempt{}, err
	}
	targetCtx, releaseTarget, ok := s.tryLockImportTarget(ctx, dl.MediaItemID)
	if s.ImportRunning(key.DownloadID) || !ok {
		if ok {
			releaseTarget()
		}
		return sqlite.ImportAttempt{}, sqlite.ErrImportPublicationPending
	}
	defer releaseTarget()
	ctx = targetCtx
	switch action {
	case "retain_uncertain":
		return s.db.RetainUncertainImport(ctx, key, ids)
	case "operator_adopted", "operator_disposed":
		if err := s.db.ResolveRetainedImportTargets(ctx, key, ids, action); err != nil {
			return sqlite.ImportAttempt{}, err
		}
		return s.db.GetImportAttempt(ctx, key.DownloadID, key.GenerationKey)
	default:
		return sqlite.ImportAttempt{}, fmt.Errorf("unknown publication review action")
	}
}

func (s *Service) reconcileImportCancellations(ctx context.Context) {
	ctx, release, err := ports.EnterWork(ctx, s.admission, "import cancellation reconciliation")
	if err != nil {
		return
	}
	defer release()
	keys, err := s.db.PendingImportCancellations(ctx)
	if err != nil {
		s.log.Warn("import cancellation scan failed", "err", err)
		return
	}
	for _, key := range keys {
		unlock, ok := s.tryLockDownload(key.DownloadID)
		if !ok {
			continue
		}
		s.finishCancelledImport(ctx, key)
		unlock()
	}
}

func (s *Service) startImportCancellationWorker(ctx context.Context) {
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reconcileImportCancellations(ctx)
			}
		}
	}()
}

type importPolicyRefusal struct{ err error }

func (e importPolicyRefusal) Error() string { return e.err.Error() }
func (e importPolicyRefusal) Unwrap() error { return e.err }
func importPolicyRefusalf(format string, args ...any) error {
	return importPolicyRefusal{fmt.Errorf(format, args...)}
}
func isImportPolicyRefusal(err error) bool {
	var refusal importPolicyRefusal
	return errors.As(err, &refusal)
}
func (s *Service) recordSkippedImportFile(ctx context.Context, src string) error {
	key, ok := importAttemptFrom(ctx)
	if !ok {
		return nil
	}
	return importControlError(s.db.ResolveImportFile(ctx, key, src, "skipped", ""))
}
