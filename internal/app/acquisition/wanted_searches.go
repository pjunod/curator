package acquisition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

const (
	WantedSearchJobKind      = "wanted.search"
	DefaultWantedTargetDelay = time.Second
	MaxWantedTargetDelay     = 60 * time.Second
)

var (
	ErrInvalidWantedSearch  = errors.New("invalid wanted search request")
	ErrWantedSearchConflict = errors.New("another wanted search is active")
	errWantedCancelled      = errors.New("wanted search cancelled")
	errWantedReasonChanged  = errors.New("wanted reason changed")
	errWantedNoLonger       = errors.New("target is no longer wanted")
	errWantedInFlight       = errors.New("target is already downloading")
	errWantedRegrabCapped   = errors.New("target reached the re-grab limit")
)

var exactWantedID = regexp.MustCompile(`^(?:movie|book):[1-9][0-9]*(?::c[1-9][0-9]*)?$|^episode:[1-9][0-9]*:[0-9]+:[1-9][0-9]*(?::c[1-9][0-9]*)?$`)

type WantedReason string

const (
	WantedMissing WantedReason = "missing"
	WantedUpgrade WantedReason = "upgrade"
)

type WantedSearchRequest struct {
	Scope       string       `json:"scope"`
	Reason      WantedReason `json:"reason,omitempty"`
	MediaItemID int64        `json:"mediaItemId,omitempty"`
	WantableID  string       `json:"wantableId,omitempty"`
	TargetDelay *int         `json:"targetDelayMs,omitempty"`
}

type WantedSearchRun struct {
	RunID, Scope, ScopeLabel, Status, Error                 string
	Reason                                                  WantedReason
	MediaItemID                                             int64
	WantableID                                              string
	CreatedAt, StartedAt, FinishedAt, CancelRequestedAt     time.Time
	TargetDelayMs                                           int
	Selected, Processed, Searched, Skipped, Failed, Grabbed int
}

type WantedSearchResult struct {
	WantableID, Label, State, Skipped, Grabbed, Error string
	Ordinal, Seen, Matched, Accepted                  int
	FinishedAt                                        time.Time
}

type WantedSearchConflict struct {
	ActiveRunID string
	Message     string
}

func (e *WantedSearchConflict) Error() string { return e.Message }
func (e *WantedSearchConflict) Unwrap() error { return ErrWantedSearchConflict }

type wantedJobEnqueuer interface {
	Enqueue(context.Context, domain.Job) (int64, error)
}

func (s *Service) WithWantedSearchQueue(q wantedJobEnqueuer) *Service {
	s.wantedQueue = q
	return s
}

func validWantedReason(reason WantedReason) bool {
	return reason == WantedMissing || reason == WantedUpgrade
}

func normalizedWantedDelay(req WantedSearchRequest) (time.Duration, error) {
	if req.TargetDelay == nil {
		return DefaultWantedTargetDelay, nil
	}
	if *req.TargetDelay < 0 || *req.TargetDelay > int(MaxWantedTargetDelay/time.Millisecond) {
		return 0, fmt.Errorf("%w: targetDelayMs must be from 0 through 60000", ErrInvalidWantedSearch)
	}
	return time.Duration(*req.TargetDelay) * time.Millisecond, nil
}

func validateWantedSearch(req WantedSearchRequest) error {
	switch req.Scope {
	case "all":
		if req.Reason != "" || req.MediaItemID != 0 || req.WantableID != "" {
			return fmt.Errorf("%w: all scope cannot include a selector", ErrInvalidWantedSearch)
		}
	case "reason":
		if !validWantedReason(req.Reason) || req.MediaItemID != 0 || req.WantableID != "" {
			return fmt.Errorf("%w: reason scope requires exactly one known reason", ErrInvalidWantedSearch)
		}
	case "group":
		if req.MediaItemID <= 0 || req.WantableID != "" || (req.Reason != "" && !validWantedReason(req.Reason)) {
			return fmt.Errorf("%w: group scope requires a positive mediaItemId and optional known reason", ErrInvalidWantedSearch)
		}
	case "target":
		if !exactWantedID.MatchString(req.WantableID) || req.MediaItemID != 0 || req.Reason != "" {
			return fmt.Errorf("%w: target scope requires exactly one wantableId", ErrInvalidWantedSearch)
		}
	default:
		return fmt.Errorf("%w: unknown scope %q", ErrInvalidWantedSearch, req.Scope)
	}
	return nil
}

func wantedReasonOf(w domain.Wantable) WantedReason {
	if _, ok := w.CurrentQuality(); ok {
		return WantedUpgrade
	}
	return WantedMissing
}

func wantedReasonMatchesSnapshot(target sqlite.WantedSearchTarget, w domain.Wantable) bool {
	return target.SelectedReason == string(wantedReasonOf(w))
}

func exactWantedCandidate(parsed parser.Parsed) bool {
	return !parsed.SeasonPack && len(parsed.Episodes) <= 1
}

func selectWantedTargets(wanted []domain.Wantable, req WantedSearchRequest) []domain.Wantable {
	selected := make([]domain.Wantable, 0, len(wanted))
	for _, w := range wanted {
		reason := wantedReasonOf(w)
		switch req.Scope {
		case "all":
			selected = append(selected, w)
		case "reason":
			if reason == req.Reason {
				selected = append(selected, w)
			}
		case "group":
			if w.MediaItemID() == req.MediaItemID && (req.Reason == "" || reason == req.Reason) {
				selected = append(selected, w)
			}
		case "target":
			if string(w.ID()) == req.WantableID {
				selected = append(selected, w)
			}
		}
	}
	return selected
}

func runFromStore(r sqlite.WantedSearchRun) WantedSearchRun {
	return WantedSearchRun{
		RunID: r.RunID, Scope: r.Scope, Reason: WantedReason(r.Reason),
		MediaItemID: r.MediaItemID, WantableID: r.WantableID,
		ScopeLabel: r.ScopeLabel, Status: r.Status, CreatedAt: r.CreatedAt,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		CancelRequestedAt: r.CancelRequestedAt, TargetDelayMs: int(r.TargetDelay / time.Millisecond),
		Selected: r.Selected, Processed: r.Processed, Searched: r.Searched,
		Skipped: r.Skipped, Failed: r.Failed, Grabbed: r.Grabbed, Error: r.Error,
	}
}

func sameWantedSearch(r sqlite.WantedSearchRun, req WantedSearchRequest, delay time.Duration) bool {
	return r.Scope == req.Scope && r.Reason == string(req.Reason) &&
		r.MediaItemID == req.MediaItemID && r.WantableID == req.WantableID && r.TargetDelay == delay
}

func wantedRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// StartWantedSearch accepts and snapshots a complete server-owned scope.
func (s *Service) StartWantedSearch(ctx context.Context, req WantedSearchRequest) (WantedSearchRun, error) {
	if err := validateWantedSearch(req); err != nil {
		return WantedSearchRun{}, err
	}
	delay, err := normalizedWantedDelay(req)
	if err != nil {
		return WantedSearchRun{}, err
	}
	if active, err := s.db.ActiveWantedSearch(ctx); err == nil {
		if sameWantedSearch(active, req, delay) {
			return runFromStore(active), nil
		}
		return WantedSearchRun{}, &WantedSearchConflict{ActiveRunID: active.RunID,
			Message: "another Wanted search is active; cancel it before starting a different scope"}
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return WantedSearchRun{}, err
	}
	if enabled, err := s.enabledIndexers(ctx); err != nil {
		return WantedSearchRun{}, err
	} else if len(enabled) == 0 {
		return WantedSearchRun{}, ErrNoIndexers
	}

	if req.Scope == "group" {
		if _, err := s.db.GetMediaItemFull(ctx, req.MediaItemID); err != nil {
			return WantedSearchRun{}, err
		}
	}
	s.InvalidateWanted()
	wanted, err := s.Wanted(ctx)
	if err != nil {
		return WantedSearchRun{}, err
	}
	selected := selectWantedTargets(wanted, req)
	if req.Scope == "target" && len(selected) == 0 {
		w, resolveErr := s.wantableFromID(ctx, req.WantableID)
		if resolveErr != nil {
			return WantedSearchRun{}, resolveErr
		}
		if string(w.ID()) != req.WantableID {
			return WantedSearchRun{}, fmt.Errorf("%w: wanted target %q", ErrNotFound, req.WantableID)
		}
		selected = append(selected, w)
	}

	label := "All wanted items"
	if req.Scope == "reason" {
		label = fmt.Sprintf("All %s items", strings.ToUpper(string(req.Reason[:1]))+string(req.Reason[1:]))
	} else if req.Scope == "group" {
		item, _ := s.db.GetMediaItemFull(ctx, req.MediaItemID)
		label = item.Title
		if req.Reason != "" {
			label = fmt.Sprintf("%s in %s", strings.ToUpper(string(req.Reason[:1]))+string(req.Reason[1:]), item.Title)
		}
	} else if req.Scope == "target" {
		label = describeTarget(selected[0])
	}
	runID, err := wantedRunID()
	if err != nil {
		return WantedSearchRun{}, err
	}
	now := time.Now().UTC()
	stored := sqlite.WantedSearchRun{RunID: runID, Scope: req.Scope, Reason: string(req.Reason),
		MediaItemID: req.MediaItemID, WantableID: req.WantableID, ScopeLabel: label,
		CreatedAt: now, TargetDelay: delay}
	targets := make([]sqlite.WantedSearchTarget, 0, len(selected))
	for _, w := range selected {
		targets = append(targets, sqlite.WantedSearchTarget{WantableID: string(w.ID()),
			SelectedReason: string(wantedReasonOf(w)), Label: describeTarget(w)})
	}
	if err := s.db.CreateWantedSearch(ctx, stored, targets); err != nil {
		if errors.Is(err, sqlite.ErrWantedSearchActive) {
			active, activeErr := s.db.ActiveWantedSearch(ctx)
			if activeErr == nil && sameWantedSearch(active, req, delay) {
				return runFromStore(active), nil
			}
			if activeErr == nil {
				return WantedSearchRun{}, &WantedSearchConflict{ActiveRunID: active.RunID,
					Message: "another Wanted search is active; cancel it before starting a different scope"}
			}
		}
		return WantedSearchRun{}, err
	}
	if len(targets) == 0 {
		if err := s.db.FinishWantedSearch(ctx, runID, "completed", "", false); err != nil {
			return WantedSearchRun{}, err
		}
	} else if err := s.ReconcileWantedSearches(ctx); err != nil {
		s.log.Warn("wanted search: accepted but initial enqueue needs recovery", "run", runID, "err", err)
	}
	r, err := s.db.GetWantedSearch(ctx, runID)
	return runFromStore(r), err
}

func (s *Service) WantedSearch(ctx context.Context, runID string) (WantedSearchRun, error) {
	r, err := s.db.GetWantedSearch(ctx, runID)
	return runFromStore(r), err
}

func (s *Service) WantedSearchResults(ctx context.Context, runID string, limit, offset int) ([]WantedSearchResult, error) {
	if _, err := s.db.GetWantedSearch(ctx, runID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.ListWantedSearchResults(ctx, runID, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]WantedSearchResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, WantedSearchResult{WantableID: row.WantableID, Label: row.Label,
			State: row.State, Skipped: row.Skipped, Ordinal: row.Ordinal, Seen: row.Seen,
			Matched: row.Matched, Accepted: row.Accepted, Grabbed: row.Grabbed,
			Error: row.Error, FinishedAt: row.FinishedAt})
	}
	return out, nil
}

func (s *Service) CancelWantedSearch(ctx context.Context, runID string) (WantedSearchRun, error) {
	r, err := s.db.GetWantedSearch(ctx, runID)
	if err != nil {
		return WantedSearchRun{}, err
	}
	if r.Status != "queued" && r.Status != "running" {
		return runFromStore(r), nil
	}
	if _, err := s.db.RequestWantedSearchCancel(ctx, runID); err != nil {
		return WantedSearchRun{}, err
	}
	if err := s.ReconcileWantedSearches(ctx); err != nil {
		return WantedSearchRun{}, err
	}
	r, err = s.db.GetWantedSearch(ctx, runID)
	return runFromStore(r), err
}

type wantedChunkPayload struct {
	RunID   string `json:"runId"`
	Ordinal int    `json:"ordinal"`
}

// RegisterWantedSearchJobs wires the one-target chunk handler without
// importing queue infrastructure into the acquisition package.
func RegisterWantedSearchJobs[H ~func(context.Context, domain.Job) error](s *Service, register func(string, H) error) error {
	return register(WantedSearchJobKind, H(s.handleWantedSearchChunk))
}

func (s *Service) handleWantedSearchChunk(ctx context.Context, job domain.Job) error {
	var p wantedChunkPayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return fmt.Errorf("wanted search job payload: %w", err)
	}
	if p.RunID == "" || p.Ordinal < 0 {
		return errors.New("wanted search job payload is incomplete")
	}
	run, err := s.db.GetWantedSearch(ctx, p.RunID)
	if err != nil {
		return err
	}
	if run.Status != "queued" && run.Status != "running" {
		return nil
	}
	if run.Cursor != p.Ordinal {
		return nil
	}
	if !run.CancelRequestedAt.IsZero() {
		return nil
	}
	if err := s.db.StartWantedSearch(ctx, p.RunID); err != nil {
		return err
	}
	target, err := s.db.WantedSearchTargetAt(ctx, p.RunID, p.Ordinal)
	if err != nil {
		return err
	}
	result := s.executeWantedTarget(ctx, run, target)
	next := time.Now().Add(run.TargetDelay)
	_, err = s.db.CommitWantedSearchTarget(ctx, p.RunID, p.Ordinal, result, next)
	return err
}

func (s *Service) executeWantedTarget(ctx context.Context, run sqlite.WantedSearchRun, target sqlite.WantedSearchTarget) sqlite.WantedSearchTarget {
	result := target
	skip := func(reason, message string) sqlite.WantedSearchTarget {
		result.State, result.Skipped, result.Error = "skipped", reason, message
		return result
	}
	fail := func(err error) sqlite.WantedSearchTarget {
		result.State, result.Error = "failed", err.Error()
		return result
	}
	if latest, err := s.db.GetWantedSearch(ctx, run.RunID); err != nil {
		return fail(err)
	} else if !latest.CancelRequestedAt.IsZero() {
		return skip("cancelled", "run cancellation was requested")
	}
	w, err := s.wantableFromID(ctx, target.WantableID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return skip("no_longer_wanted", "target no longer exists")
	}
	if err != nil {
		return skip("no_longer_wanted", err.Error())
	}
	if !w.Monitored() {
		return skip("unmonitored", "target is no longer monitored")
	}
	profile, err := s.db.GetProfile(ctx, w.ProfileID())
	if err != nil {
		return fail(err)
	}
	if !wants(profile, w) {
		return skip("no_longer_wanted", "target is already satisfied")
	}
	if !wantedReasonMatchesSnapshot(target, w) {
		return skip("reason_changed", fmt.Sprintf("target is now %s", wantedReasonOf(w)))
	}
	if len(s.notInFlight(ctx, []domain.Wantable{w})) == 0 {
		return skip("downloading", "target already has a download in progress")
	}
	if capped, _ := s.regrabCapped(ctx, w); capped {
		return skip("regrab_capped", "target reached the automatic re-grab limit")
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return fail(err)
	}
	if len(enabled) == 0 {
		return fail(ErrNoIndexers)
	}
	allowed := exactWantedCandidate
	beforeGrab := func() error {
		latest, err := s.db.GetWantedSearch(ctx, run.RunID)
		if err != nil {
			return err
		}
		if !latest.CancelRequestedAt.IsZero() {
			return errWantedCancelled
		}
		fresh, err := s.wantableFromID(ctx, target.WantableID)
		if err != nil {
			return err
		}
		profile, err := s.db.GetProfile(ctx, fresh.ProfileID())
		if err != nil {
			return err
		}
		if !fresh.Monitored() || !wants(profile, fresh) {
			return errWantedNoLonger
		}
		if len(s.notInFlight(ctx, []domain.Wantable{fresh})) == 0 {
			return errWantedInFlight
		}
		if !wantedReasonMatchesSnapshot(target, fresh) {
			return errWantedReasonChanged
		}
		if capped, _ := s.regrabCapped(ctx, fresh); capped {
			return errWantedRegrabCapped
		}
		return nil
	}
	searchCtx, stopSearch := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-searchCtx.Done():
				return
			case <-ticker.C:
				latest, err := s.db.GetWantedSearch(searchCtx, run.RunID)
				if err == nil && !latest.CancelRequestedAt.IsZero() {
					stopSearch()
					return
				}
			}
		}
	}()
	tally, err := s.searchAndGrabBestScoped(searchCtx, w, enabled, allowed, beforeGrab)
	close(done)
	stopSearch()
	result.Seen, result.Matched, result.Accepted, result.Grabbed = tally.Seen, tally.Matched, tally.Accepted, tally.Grabbed
	if errors.Is(err, errWantedCancelled) || (errors.Is(err, context.Canceled) && ctx.Err() == nil) {
		return skip("cancelled", "run cancellation was requested before grab")
	}
	if errors.Is(err, errWantedReasonChanged) {
		return skip("reason_changed", "target reason changed before grab")
	}
	if errors.Is(err, errWantedNoLonger) {
		return skip("no_longer_wanted", "target changed before search or grab")
	}
	if errors.Is(err, errWantedInFlight) {
		return skip("downloading", "target already has a download in progress")
	}
	if errors.Is(err, errWantedRegrabCapped) {
		return skip("regrab_capped", "target reached the automatic re-grab limit")
	}
	if err != nil {
		return fail(err)
	}
	result.State = "searched"
	return result
}

func (s *Service) enqueueWantedChunk(ctx context.Context, run sqlite.WantedSearchRun) error {
	if s.wantedQueue == nil {
		return errors.New("wanted search queue is not configured")
	}
	payload, _ := json.Marshal(wantedChunkPayload{RunID: run.RunID, Ordinal: run.Cursor})
	id, err := s.wantedQueue.Enqueue(ctx, domain.Job{Kind: WantedSearchJobKind,
		Payload: string(payload), Priority: 100, RunAfter: run.NextReadyAt,
		DedupeKey: "wanted.search:" + run.RunID})
	if err != nil {
		return err
	}
	return s.db.LinkWantedSearchJob(ctx, run.RunID, run.Cursor, id)
}

// ReconcileWantedSearches owns continuation and restart recovery. It is safe
// to call from both the periodic coordinator and a JobFinished notification.
func (s *Service) ReconcileWantedSearches(ctx context.Context) error {
	runs, err := s.db.ListActiveWantedSearches(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		job, jobErr := s.db.FindNewestJobByDedupe(ctx, "wanted.search:"+run.RunID)
		if jobErr != nil && !errors.Is(jobErr, sqlite.ErrNotFound) {
			return jobErr
		}
		hasJob := jobErr == nil
		if !run.CancelRequestedAt.IsZero() {
			// A leased chunk may be between its final validation and grab. Let its
			// cancellation watcher drain it before terminalising the snapshot.
			if hasJob && job.State == domain.JobLeased {
				continue
			}
			if err := s.db.FinishWantedSearch(ctx, run.RunID, "cancelled", "", true); err != nil {
				return err
			}
			continue
		}
		if hasJob && (job.State == domain.JobQueued || job.State == domain.JobLeased) {
			// Enqueue and link are separate durable operations. A newer job proves
			// enqueue committed before a crash; adopt it instead of duplicating it
			// or trusting an older predecessor recorded on the run.
			if run.JobID != job.ID {
				if err := s.db.LinkWantedSearchJob(ctx, run.RunID, run.Cursor, job.ID); err != nil {
					return err
				}
			}
			continue
		}
		if run.Cursor >= run.Selected {
			if err := s.db.FinishWantedSearch(ctx, run.RunID, "completed", "", false); err != nil {
				return err
			}
			continue
		}
		if hasJob && job.State == domain.JobFailed {
			if err := s.db.FinishWantedSearch(ctx, run.RunID, "interrupted", job.LastError, false); err != nil {
				return err
			}
			continue
		}
		if err := s.enqueueWantedChunk(ctx, run); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RunWantedSearchCoordinator(ctx context.Context) {
	_ = s.ReconcileWantedSearches(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.ReconcileWantedSearches(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("wanted search: reconcile failed", "err", err)
			}
		}
	}
}
