package library

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

const (
	identityFreshFor   = 7 * 24 * time.Hour
	identityRetryDelay = time.Hour
)

// IdentityChanged is the immediate in-process invalidation signal. The
// durable revision remains authoritative across service instances.
type IdentityChanged struct {
	ItemID   int64 `json:"itemId"`
	Revision int64 `json:"revision"`
}

func (IdentityChanged) EventType() string { return "library.identity.changed" }

type namedIdentityProvider struct {
	name     string
	provider ports.IdentityMetadataProvider
}

func (s *Service) identityProviders(item domain.MediaItem) []namedIdentityProvider {
	var all []namedIdentityProvider
	if provider, ok := s.meta.(ports.IdentityMetadataProvider); ok && item.IDs.TMDB != 0 {
		all = append(all, namedIdentityProvider{name: "tmdb", provider: provider})
	}
	for _, candidate := range s.series {
		provider, ok := candidate.(ports.IdentityMetadataProvider)
		if ok && (item.IDs.TVDB != 0 || item.IDs.IMDB != "") {
			all = append(all, namedIdentityProvider{name: candidate.Name(), provider: provider})
		}
	}
	// Selected hydration source first. Aliases may enrich across later
	// providers only because all calls use the stored verified IDs.
	for i := range all {
		if all[i].name == item.Source {
			all[0], all[i] = all[i], all[0]
			break
		}
	}
	return all
}

// RefreshIdentity refreshes complete provider snapshots. Ordinary provider
// failure is durably recorded and returned as success so the generic queue
// completes this attempt; the sweep owns retry timing.
func (s *Service) RefreshIdentity(ctx context.Context, itemID int64, force bool) error {
	select {
	case s.identitySem <- struct{}{}:
		defer func() { <-s.identitySem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return err
	}
	if item.Kind == domain.KindBook || item.IsManual() {
		return nil
	}
	now := time.Now()
	status := map[string]domain.IdentitySourceStatus{}
	for _, source := range item.IdentitySources {
		status[source.Source] = source
	}
	for _, entry := range s.identityProviders(item) {
		current := status[entry.name]
		if !current.RetryAfter.IsZero() && now.Before(current.RetryAfter) {
			continue
		}
		if !force && current.LastError == "" && !current.FetchedAt.IsZero() && now.Sub(current.FetchedAt) < identityFreshFor {
			continue
		}
		metadata, fetchErr := entry.provider.IdentityMetadata(ctx, item.Kind, item.IDs)
		if fetchErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			retry := now.Add(identityRetryDelay)
			var remote *ports.RemoteError
			if errors.As(fetchErr, &remote) && !remote.RetryAt.IsZero() {
				retry = remote.RetryAt
			}
			if persistErr := s.db.RecordIdentityFailure(ctx, itemID, entry.name, now, retry, fetchErr.Error()); persistErr != nil {
				return fmt.Errorf("persisting identity failure: %w", persistErr)
			}
			s.log.Warn("library: identity provider failed", "item", itemID, "source", entry.name, "retryAfter", retry, "err", fetchErr)
			continue
		}
		if err := s.db.ReplaceIdentitySnapshot(ctx, itemID, entry.name, metadata.Aliases, metadata.Countries, now); err != nil {
			return err
		}
		s.log.Info("library: identity refreshed", "item", itemID, "source", entry.name, "aliases", len(metadata.Aliases))
	}
	revision, err := s.db.IdentityRevision(ctx)
	if err == nil {
		s.publish(IdentityChanged{ItemID: itemID, Revision: revision})
	}
	return err
}

// SweepIdentity enqueues at most 100 missing/stale provider refreshes. The
// database state is the cursor, so process restart resumes naturally.
func (s *Service) SweepIdentity(ctx context.Context) error {
	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return err
	}
	now := time.Now()
	inserted := 0
	for _, item := range items {
		if inserted >= 100 || ctx.Err() != nil {
			break
		}
		if item.Kind == domain.KindBook || item.IsManual() || len(s.identityProviders(item)) == 0 {
			continue
		}
		needs := len(item.IdentitySources) == 0
		for _, source := range item.IdentitySources {
			if source.RetryAfter.After(now) {
				continue
			}
			if source.FetchedAt.IsZero() || now.Sub(source.FetchedAt) >= identityFreshFor || source.LastError != "" {
				needs = true
			}
		}
		if !needs {
			continue
		}
		queued, err := s.enqueueIdentity(ctx, item.ID, 80)
		if err != nil {
			return err
		}
		if queued {
			inserted++
		}
	}
	return ctx.Err()
}

func (s *Service) EnqueueIdentityRefresh(ctx context.Context, itemID int64, explicit bool) error {
	if s.queue == nil {
		return s.RefreshIdentity(ctx, itemID, explicit)
	}
	priority := int64(80)
	if explicit {
		priority = 40
	}
	_, err := s.enqueueIdentity(ctx, itemID, priority)
	return err
}

func (s *Service) enqueueIdentity(ctx context.Context, itemID int64, priority int64) (bool, error) {
	payload := fmt.Sprintf(`{"itemId":%d}`, itemID)
	return s.queue.EnqueueUnique(ctx, domain.Job{Kind: JobIdentityRefresh, Payload: payload, DedupeKey: fmt.Sprintf("%s:%d", JobIdentityRefresh, itemID), Priority: priority})
}

func (s *Service) AddManualAlias(ctx context.Context, itemID int64, title string, searchable bool) (domain.TitleAlias, error) {
	if _, err := s.db.GetMediaItemFull(ctx, itemID); err != nil {
		return domain.TitleAlias{}, err
	}
	// The store validates too, but it answers with a plain error and the API
	// maps that to 500. A blank or oversized title is the caller's mistake.
	if trimmed := strings.TrimSpace(title); trimmed == "" || len(trimmed) > 256 {
		return domain.TitleAlias{}, fmt.Errorf("%w: alias title must be 1-256 characters", ErrInvalidInput)
	}
	alias, err := s.db.AddManualAlias(ctx, itemID, title, searchable)
	if errors.Is(err, sqlite.ErrDuplicate) {
		return domain.TitleAlias{}, ErrAlreadyExists
	}
	if err == nil {
		revision, _ := s.db.IdentityRevision(ctx)
		s.publish(IdentityChanged{ItemID: itemID, Revision: revision})
	}
	return alias, err
}

func (s *Service) DeleteManualAlias(ctx context.Context, itemID, aliasID int64) error {
	err := s.db.DeleteManualAlias(ctx, itemID, aliasID)
	if err == nil {
		revision, _ := s.db.IdentityRevision(ctx)
		s.publish(IdentityChanged{ItemID: itemID, Revision: revision})
	}
	return err
}
