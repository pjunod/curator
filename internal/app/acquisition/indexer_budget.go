package acquisition

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

var indexerRequestLocks sync.Map

func requestLock(id int64) *sync.Mutex {
	v, _ := indexerRequestLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}
func (s *Service) budgetIndexer(cfg ports.IndexerConfig, bucket string, automatic bool) ports.Indexer {
	return s.budgetIndexerStrict(cfg, bucket, automatic, automatic)
}

// budgetIndexerStrict separates the two things "automatic" used to mean: which
// allowance a request is charged to, and whether the indexer must meet the
// strict capabilities required when no person reviews the grab. A manual Auto
// Search is charged as interactive work but still grabs unreviewed.
func (s *Service) budgetIndexerStrict(cfg ports.IndexerConfig, bucket string, automatic, strictCaps bool) ports.Indexer {
	raw := s.newIndexer(cfg)
	if observer, ok := raw.(ports.RequestResultInstaller); ok {
		observer.SetRequestResult(func(ctx context.Context, err error) { s.rememberIndexerDelay(ctx, cfg.ID, err) })
	}
	gate := func(ctx context.Context) (func(), error) {
		mu := requestLock(cfg.ID)
		mu.Lock()
		if err := s.db.ReserveIndexerRequest(ctx, cfg.ID, bucket, automatic, time.Now()); err != nil {
			mu.Unlock()
			return nil, err
		}
		return mu.Unlock, nil
	}
	if installer, ok := raw.(ports.RequestGateInstaller); ok {
		installer.SetRequestGate(gate)
		if strict, ok := raw.(ports.AcquisitionCapabilities); strictCaps && ok {
			return &strictAcquisitionIndexer{Indexer: raw, strict: strict}
		}
		return raw
	}
	return &budgetedIndexer{Indexer: raw, gate: gate}
}

type budgetedIndexer struct {
	ports.Indexer
	gate func(context.Context) (func(), error)
}

func (b *budgetedIndexer) Search(ctx context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	release, e := b.gate(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	return b.Indexer.Search(ctx, q)
}
func (b *budgetedIndexer) FetchRSS(ctx context.Context) ([]ports.Release, error) {
	release, e := b.gate(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	return b.Indexer.FetchRSS(ctx)
}
func (b *budgetedIndexer) Capabilities(ctx context.Context) (ports.IndexerCapabilities, error) {
	if p, ok := b.Indexer.(ports.IndexerCapabilitiesProvider); ok {
		release, e := b.gate(ctx)
		if e != nil {
			return ports.IndexerCapabilities{}, e
		}
		defer release()
		return p.Capabilities(ctx)
	}
	return ports.IndexerCapabilities{}, nil
}
func (s *Service) rememberIndexerDelay(ctx context.Context, id int64, err error) {
	var remote *ports.RemoteError
	if errors.As(err, &remote) && !remote.RetryAt.IsZero() {
		_ = s.db.SetIndexerRetryAt(ctx, id, remote.RetryAt)
	}
}

// RunRSSPacing supplements the scheduler's maximum-idle sweep with persisted
// due times. The sweep mutex coalesces simultaneous scheduler/timer wakeups.
func (s *Service) RunRSSPacing(ctx context.Context) {
	for {
		var due int64
		err := s.db.R.QueryRowContext(ctx, `SELECT COALESCE(min(max(next_rss_at,retry_at)),0) FROM indexers WHERE enabled=1`).Scan(&due)
		wait := 15 * time.Minute
		if err == nil && due > 0 {
			until := time.Until(time.UnixMilli(due))
			if until > 0 && until < wait {
				wait = until
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err = s.SyncRSS(ctx); err != nil {
			s.log.Warn("rss due sweep failed", "err", err)
		}
	}
}

type strictAcquisitionIndexer struct {
	ports.Indexer
	strict ports.AcquisitionCapabilities
}

func (b *strictAcquisitionIndexer) Capabilities(ctx context.Context) (ports.IndexerCapabilities, error) {
	return b.strict.AcquisitionCapabilities(ctx)
}
func (b *strictAcquisitionIndexer) SearchPage(ctx context.Context, q domain.SearchQuery, offset int) (ports.IndexerPage, error) {
	if p, ok := b.Indexer.(ports.PagedIndexer); ok {
		return p.SearchPage(ctx, q, offset)
	}
	rows, err := b.Search(ctx, q)
	return ports.IndexerPage{Releases: rows, Complete: true}, err
}
