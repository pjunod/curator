package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/matcher"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

func aliasFromRow(row sqlitegen.MediaAlias) domain.TitleAlias {
	return domain.TitleAlias{
		ID: row.ID, Title: row.Title, Source: row.Source, SourceID: row.SourceID,
		Language: row.Language, MarketCountry: row.MarketCountry,
		Scope: row.Scope, Role: row.Role, Searchable: row.Searchable != 0,
	}
}

func sourceFromRow(row sqlitegen.MediaIdentitySource) domain.IdentitySourceStatus {
	var countries []domain.CountryEvidence
	_ = json.Unmarshal([]byte(row.Countries), &countries)
	return domain.IdentitySourceStatus{
		Source: row.Source, Countries: countries,
		FetchedAt: millisTime(row.FetchedAt), AttemptedAt: millisTime(row.AttemptedAt),
		RetryAfter: millisTime(row.RetryAfter), LastError: row.LastError,
	}
}

func millisTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

func insertInitialIdentity(ctx context.Context, q *sqlitegen.Queries, itemID int64, item domain.MediaItem) error {
	for _, alias := range item.Aliases {
		if strings.TrimSpace(alias.Title) == "" {
			continue
		}
		if alias.Source == "" {
			alias.Source = item.Source
		}
		if alias.Scope == "" {
			alias.Scope = "work"
		}
		if alias.Role == "" {
			alias.Role = "alternate"
		}
		if _, err := q.InsertMediaAlias(ctx, aliasParams(itemID, alias)); err != nil {
			return err
		}
	}
	for _, source := range item.IdentitySources {
		countries, _ := json.Marshal(source.Countries)
		if err := q.UpsertIdentitySource(ctx, sqlitegen.UpsertIdentitySourceParams{
			MediaItemID: itemID, Source: source.Source, Countries: string(countries),
			FetchedAt: timeMillis(source.FetchedAt), AttemptedAt: timeMillis(source.AttemptedAt),
			RetryAfter: timeMillis(source.RetryAfter), LastError: source.LastError,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) loadItemIdentity(ctx context.Context, item *domain.MediaItem) error {
	aliases, err := d.Read.ListMediaAliases(ctx, item.ID)
	if err != nil {
		return err
	}
	sources, err := d.Read.ListIdentitySources(ctx, item.ID)
	if err != nil {
		return err
	}
	for _, row := range aliases {
		item.Aliases = append(item.Aliases, aliasFromRow(row))
	}
	for _, row := range sources {
		source := sourceFromRow(row)
		item.IdentitySources = append(item.IdentitySources, source)
		item.Countries = append(item.Countries, source.Countries...)
	}
	return nil
}

func (d *DB) loadAllIdentity(ctx context.Context, items []domain.MediaItem) error {
	aliases, err := d.Read.ListAllMediaAliases(ctx)
	if err != nil {
		return err
	}
	sources, err := d.Read.ListAllIdentitySources(ctx)
	if err != nil {
		return err
	}
	byID := make(map[int64]*domain.MediaItem, len(items))
	for i := range items {
		byID[items[i].ID] = &items[i]
	}
	for _, row := range aliases {
		if item := byID[row.MediaItemID]; item != nil {
			item.Aliases = append(item.Aliases, aliasFromRow(row))
		}
	}
	for _, row := range sources {
		if item := byID[row.MediaItemID]; item != nil {
			source := sourceFromRow(row)
			item.IdentitySources = append(item.IdentitySources, source)
			item.Countries = append(item.Countries, source.Countries...)
		}
	}
	return nil
}

// IdentityRevision returns the durable cross-process invalidation revision.
func (d *DB) IdentityRevision(ctx context.Context) (int64, error) {
	return d.Read.IdentityRevision(ctx)
}

// ReplaceIdentitySnapshot atomically replaces one provider's replaceable
// aliases and country snapshot. Manual and historical aliases survive.
func (d *DB) ReplaceIdentitySnapshot(ctx context.Context, itemID int64, source string, aliases []domain.TitleAlias, countries []domain.CountryEvidence, now time.Time) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("identity source is required")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	if err := q.DeleteReplaceableProviderAliases(ctx, sqlitegen.DeleteReplaceableProviderAliasesParams{MediaItemID: itemID, Source: source}); err != nil {
		return err
	}
	for _, alias := range aliases {
		if strings.TrimSpace(alias.Title) == "" {
			continue
		}
		alias.Source = source
		if alias.Scope == "" {
			alias.Scope = "work"
		}
		if alias.Role == "" {
			alias.Role = "alternate"
		}
		if alias.Role == "manual" {
			return fmt.Errorf("provider snapshot cannot write manual alias")
		}
		if _, err := q.InsertMediaAlias(ctx, aliasParams(itemID, alias)); err != nil {
			if isConstraint(err) {
				continue
			}
			return err
		}
	}
	rawCountries, _ := json.Marshal(countries)
	if err := q.UpsertIdentitySource(ctx, sqlitegen.UpsertIdentitySourceParams{
		MediaItemID: itemID, Source: source, Countries: string(rawCountries),
		FetchedAt: now.UnixMilli(), AttemptedAt: now.UnixMilli(),
		RetryAfter: 0, LastError: "",
	}); err != nil {
		return err
	}
	if _, err := q.BumpIdentityRevision(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordIdentityFailure retains the last complete snapshot but records that
// the latest required refresh failed, disabling convention eligibility.
func (d *DB) RecordIdentityFailure(ctx context.Context, itemID int64, source string, attemptedAt, retryAfter time.Time, message string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	existing, getErr := q.GetIdentitySource(ctx, sqlitegen.GetIdentitySourceParams{MediaItemID: itemID, Source: source})
	if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		return getErr
	}
	if errors.Is(getErr, sql.ErrNoRows) {
		existing = sqlitegen.MediaIdentitySource{MediaItemID: itemID, Source: source, Countries: "[]"}
	}
	if err := q.UpsertIdentitySource(ctx, sqlitegen.UpsertIdentitySourceParams{
		MediaItemID: itemID, Source: source, Countries: existing.Countries,
		FetchedAt: existing.FetchedAt, AttemptedAt: attemptedAt.UnixMilli(),
		RetryAfter: timeMillis(retryAfter), LastError: message,
	}); err != nil {
		return err
	}
	if _, err := q.BumpIdentityRevision(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// AddManualAlias validates and stores an operator alias. It need not be
// globally unique; ambiguity is a matcher decision.
func (d *DB) AddManualAlias(ctx context.Context, itemID int64, title string, searchable bool) (domain.TitleAlias, error) {
	title = strings.TrimSpace(title)
	normalized := matcher.NormalizeTitle(title)
	if len(title) == 0 || len(title) > 256 || normalized == "" {
		return domain.TitleAlias{}, fmt.Errorf("manual alias must contain 1-256 characters and a normalized token")
	}
	alias := domain.TitleAlias{Title: title, Source: "manual", Scope: "work", Role: "manual", Searchable: searchable}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return domain.TitleAlias{}, err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	id, err := q.InsertMediaAlias(ctx, aliasParams(itemID, alias))
	if err != nil {
		if isConstraint(err) {
			return domain.TitleAlias{}, ErrDuplicate
		}
		return domain.TitleAlias{}, err
	}
	if _, err := q.BumpIdentityRevision(ctx); err != nil {
		return domain.TitleAlias{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TitleAlias{}, err
	}
	alias.ID = id
	return alias, nil
}

func (d *DB) DeleteManualAlias(ctx context.Context, itemID, aliasID int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	n, err := q.DeleteManualAlias(ctx, sqlitegen.DeleteManualAliasParams{ID: aliasID, MediaItemID: itemID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := q.BumpIdentityRevision(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// FindMediaItemsByExternalID returns every same-kind claimant so legacy
// duplicates become inspectable ambiguity instead of a startup failure.
func (d *DB) FindMediaItemsByExternalID(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) ([]domain.MediaItem, error) {
	var rows []sqlitegen.MediaItem
	var err error
	switch ref.Provider {
	case "imdb":
		rows, err = d.Read.FindMediaItemsByKindImdb(ctx, sqlitegen.FindMediaItemsByKindImdbParams{Kind: string(kind), ImdbID: ref.Value})
	case "tvdb":
		value, parseErr := strconv.ParseInt(ref.Value, 10, 64)
		if parseErr != nil {
			return nil, parseErr
		}
		rows, err = d.Read.FindMediaItemsByKindTvdb(ctx, sqlitegen.FindMediaItemsByKindTvdbParams{Kind: string(kind), TvdbID: value})
	case "tmdb":
		value, parseErr := strconv.ParseInt(ref.Value, 10, 64)
		if parseErr != nil {
			return nil, parseErr
		}
		rows, err = d.Read.FindMediaItemsByKindTmdb(ctx, sqlitegen.FindMediaItemsByKindTmdbParams{Kind: string(kind), TmdbID: value})
	default:
		return nil, fmt.Errorf("unknown external id provider %q", ref.Provider)
	}
	if err != nil {
		return nil, err
	}
	out := make([]domain.MediaItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, itemToDomain(row))
	}
	if err := d.loadAllIdentity(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateMediaIdentity enriches IDs without changing the persisted hydration
// source. Missing values preserve stored IDs; contradictions abort.
func (d *DB) UpdateMediaIdentity(ctx context.Context, itemID int64, incoming domain.ExternalIDs) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	stored, err := q.GetMediaItem(ctx, itemID)
	if err != nil {
		return wrapNotFound(err)
	}
	merged := domain.ExternalIDs{TMDB: stored.TmdbID, IMDB: stored.ImdbID, TVDB: stored.TvdbID}
	if err := mergeExternalIDs(&merged, incoming); err != nil {
		return err
	}
	for _, ref := range refsOf(merged) {
		var matches []sqlitegen.MediaItem
		switch ref.Provider {
		case "tmdb":
			matches, err = q.FindMediaItemsByKindTmdb(ctx, sqlitegen.FindMediaItemsByKindTmdbParams{Kind: stored.Kind, TmdbID: merged.TMDB})
		case "tvdb":
			matches, err = q.FindMediaItemsByKindTvdb(ctx, sqlitegen.FindMediaItemsByKindTvdbParams{Kind: stored.Kind, TvdbID: merged.TVDB})
		case "imdb":
			matches, err = q.FindMediaItemsByKindImdb(ctx, sqlitegen.FindMediaItemsByKindImdbParams{Kind: stored.Kind, ImdbID: merged.IMDB})
		}
		if err != nil {
			return err
		}
		for _, match := range matches {
			if match.ID != itemID {
				return fmt.Errorf("%w: %s %s already belongs to item %d", ErrIdentityConflict, ref.Provider, ref.Value, match.ID)
			}
		}
	}
	if err := q.UpdateMediaExternalIDs(ctx, sqlitegen.UpdateMediaExternalIDsParams{TmdbID: merged.TMDB, ImdbID: merged.IMDB, TvdbID: merged.TVDB, UpdatedAt: time.Now().UnixMilli(), ID: itemID}); err != nil {
		return err
	}
	if _, err := q.BumpIdentityRevision(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func mergeExternalIDs(stored *domain.ExternalIDs, incoming domain.ExternalIDs) error {
	if incoming.TMDB != 0 {
		if stored.TMDB != 0 && stored.TMDB != incoming.TMDB {
			return fmt.Errorf("%w: TMDB", ErrIdentityConflict)
		}
		stored.TMDB = incoming.TMDB
	}
	if incoming.TVDB != 0 {
		if stored.TVDB != 0 && stored.TVDB != incoming.TVDB {
			return fmt.Errorf("%w: TVDB", ErrIdentityConflict)
		}
		stored.TVDB = incoming.TVDB
	}
	if incoming.IMDB != "" {
		if stored.IMDB != "" && !strings.EqualFold(stored.IMDB, incoming.IMDB) {
			return fmt.Errorf("%w: IMDb", ErrIdentityConflict)
		}
		stored.IMDB = strings.ToLower(incoming.IMDB)
	}
	return nil
}

func refsOf(ids domain.ExternalIDs) []domain.ExternalRef {
	var refs []domain.ExternalRef
	if ids.TMDB != 0 {
		refs = append(refs, domain.ExternalRef{Provider: "tmdb", Value: strconv.FormatInt(ids.TMDB, 10)})
	}
	if ids.TVDB != 0 {
		refs = append(refs, domain.ExternalRef{Provider: "tvdb", Value: strconv.FormatInt(ids.TVDB, 10)})
	}
	if ids.IMDB != "" {
		refs = append(refs, domain.ExternalRef{Provider: "imdb", Value: ids.IMDB})
	}
	return refs
}

func timeMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}
