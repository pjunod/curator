package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/monarr-media/monarr/internal/domain/format"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
	"github.com/monarr-media/monarr/internal/ports"
)

// ---- blocklist (Phase 3) ----

// BlocklistEntry is one blocked release.
type BlocklistEntry struct {
	ID           int64     `json:"id"`
	MediaItemID  int64     `json:"mediaItemId"`
	ReleaseTitle string    `json:"releaseTitle"`
	Indexer      string    `json:"indexer"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"createdAt"`
}

// AddBlocklist records a failed release so automation never re-grabs it.
func (d *DB) AddBlocklist(ctx context.Context, mediaItemID int64, title, indexer, reason string) error {
	_, err := d.Write.InsertBlocklist(ctx, sqlitegen.InsertBlocklistParams{
		MediaItemID: mediaItemID, ReleaseTitle: title, Indexer: indexer,
		Reason: reason, CreatedAt: time.Now().UnixMilli(),
	})
	return err
}

// IsBlocklisted reports whether (title, indexer) is blocked.
func (d *DB) IsBlocklisted(ctx context.Context, title, indexer string) (bool, error) {
	n, err := d.Read.CountBlocklisted(ctx, sqlitegen.CountBlocklistedParams{
		ReleaseTitle: title, Indexer: indexer,
	})
	return n > 0, err
}

// ListBlocklist returns recent blocklist entries.
func (d *DB) ListBlocklist(ctx context.Context) ([]BlocklistEntry, error) {
	rows, err := d.Read.ListBlocklist(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BlocklistEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, BlocklistEntry{
			ID: r.ID, MediaItemID: r.MediaItemID, ReleaseTitle: r.ReleaseTitle,
			Indexer: r.Indexer, Reason: r.Reason, CreatedAt: time.UnixMilli(r.CreatedAt),
		})
	}
	return out, nil
}

// DeleteBlocklist removes one entry (the release becomes grabbable again).
func (d *DB) DeleteBlocklist(ctx context.Context, id int64) error {
	return d.Write.DeleteBlocklist(ctx, id)
}

// ---- notifiers (Phase 3) ----

// AddNotifier stores a notifier configuration.
func (d *DB) AddNotifier(ctx context.Context, n ports.NotifierConfig) (int64, error) {
	settings, _ := json.Marshal(n.Settings)
	if n.Settings == nil {
		settings = []byte("{}")
	}
	return d.Write.InsertNotifier(ctx, sqlitegen.InsertNotifierParams{
		Type: n.Type, Name: n.Name, Settings: string(settings),
		OnGrab: boolInt(n.OnGrab), OnImport: boolInt(n.OnImport),
		OnFailed: boolInt(n.OnFailed), OnHealth: boolInt(n.OnHealth),
		Enabled: boolInt(n.Enabled),
	})
}

func notifierToPort(r sqlitegen.Notifier) ports.NotifierConfig {
	var settings map[string]string
	_ = json.Unmarshal([]byte(r.Settings), &settings)
	return ports.NotifierConfig{
		ID: r.ID, Type: r.Type, Name: r.Name, Settings: settings,
		OnGrab: r.OnGrab != 0, OnImport: r.OnImport != 0,
		OnFailed: r.OnFailed != 0, OnHealth: r.OnHealth != 0,
		Enabled: r.Enabled != 0,
	}
}

// ListNotifiers returns every stored notifier.
func (d *DB) ListNotifiers(ctx context.Context) ([]ports.NotifierConfig, error) {
	rows, err := d.Read.ListNotifiers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.NotifierConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, notifierToPort(r))
	}
	return out, nil
}

// GetNotifier returns one notifier or ErrNotFound.
func (d *DB) GetNotifier(ctx context.Context, id int64) (ports.NotifierConfig, error) {
	r, err := d.Read.GetNotifier(ctx, id)
	if err != nil {
		return ports.NotifierConfig{}, wrapNotFound(err)
	}
	return notifierToPort(r), nil
}

// UpdateNotifier replaces a notifier in place.
//
// Editing used to mean delete-and-recreate, which changed the id — and the
// id is what the delivery log hangs off. Changing a URL should not throw
// away the record of every delivery that came before it.
func (d *DB) UpdateNotifier(ctx context.Context, n ports.NotifierConfig) error {
	settings, _ := json.Marshal(n.Settings)
	if n.Settings == nil {
		settings = []byte("{}")
	}
	return d.Write.UpdateNotifier(ctx, sqlitegen.UpdateNotifierParams{
		Type: n.Type, Name: n.Name, Settings: string(settings),
		OnGrab: boolInt(n.OnGrab), OnImport: boolInt(n.OnImport),
		OnFailed: boolInt(n.OnFailed), OnHealth: boolInt(n.OnHealth),
		Enabled: boolInt(n.Enabled), ID: n.ID,
	})
}

// DeleteNotifier removes a notifier and the deliveries that belonged to it.
func (d *DB) DeleteNotifier(ctx context.Context, id int64) error {
	// No foreign key on notifier_deliveries — deliveries outlive nothing,
	// but they must not outlive their notifier and become rows nobody can
	// name. Cleared first so a failure leaves orphans rather than a
	// notifier that cannot be removed.
	if err := d.Write.DeleteDeliveriesForNotifier(ctx, id); err != nil {
		return err
	}
	return d.Write.DeleteNotifier(ctx, id)
}

// ---- notification deliveries (plan §5.5) ----

// Delivery is one attempt-tracked notification.
type Delivery struct {
	ID         int64  `json:"id"`
	NotifierID int64  `json:"notifierId"`
	DownloadID int64  `json:"downloadId,omitempty"`
	Event      string `json:"event"`
	Payload    string `json:"-"`
	Attempts   int64  `json:"attempts"`
	LastError  string `json:"lastError,omitempty"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status"` // pending | ok | failed
	NextAt     int64  `json:"nextAt,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

func deliveryFrom(r sqlitegen.NotifierDelivery) Delivery {
	return Delivery{
		ID: r.ID, NotifierID: r.NotifierID, DownloadID: r.DownloadID,
		Event: r.Event, Payload: r.Payload, Attempts: r.Attempts,
		LastError: r.LastError, Result: r.Result, Status: r.Status,
		NextAt: r.NextAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// EnqueueDelivery records a notification to deliver, due now.
func (d *DB) EnqueueDelivery(ctx context.Context, notifierID, downloadID int64, event, payload string) (Delivery, error) {
	now := time.Now().UnixMilli()
	r, err := d.Write.EnqueueDelivery(ctx, sqlitegen.EnqueueDeliveryParams{
		NotifierID: notifierID, DownloadID: downloadID, Event: event,
		Payload: payload, NextAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Delivery{}, err
	}
	return deliveryFrom(r), nil
}

// DueDeliveries returns pending deliveries whose time has come.
func (d *DB) DueDeliveries(ctx context.Context, limit int64) ([]Delivery, error) {
	rows, err := d.Read.DueDeliveries(ctx, sqlitegen.DueDeliveriesParams{
		NextAt: time.Now().UnixMilli(), Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, deliveryFrom(r))
	}
	return out, nil
}

// SettleDelivery writes the outcome of one attempt.
func (d *DB) SettleDelivery(ctx context.Context, del Delivery) error {
	return d.Write.SettleDelivery(ctx, sqlitegen.SettleDeliveryParams{
		Attempts: del.Attempts, LastError: del.LastError, Result: del.Result,
		Status: del.Status, NextAt: del.NextAt,
		UpdatedAt: time.Now().UnixMilli(), ID: del.ID,
	})
}

// ListDeliveries returns a notifier's most recent deliveries, newest first.
func (d *DB) ListDeliveries(ctx context.Context, notifierID, limit int64) ([]Delivery, error) {
	rows, err := d.Read.ListDeliveries(ctx, sqlitegen.ListDeliveriesParams{
		NotifierID: notifierID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, deliveryFrom(r))
	}
	return out, nil
}

// ---- calendar (Phase 3) ----

// CalendarEntry is one dated library event: an episode airing or a
// movie/book release.
type CalendarEntry struct {
	Date        string `json:"date"` // ISO date
	Kind        string `json:"kind"` // episode | movie | book
	MediaItemID int64  `json:"mediaItemId"`
	Title       string `json:"title"`
	Detail      string `json:"detail"` // "S01E03 — Pilot", "by Author", ""
	HasFile     bool   `json:"hasFile"`
	// External ids, so a consumer can resolve this entry against its OWN
	// library instead of matching on the title. For an episode these are the
	// SHOW's ids — an episode's own identity is not what names the series it
	// belongs to, and the thing a reader wants beside "S04E02" is the show.
	TmdbID int64  `json:"tmdbId,omitempty"`
	ImdbID string `json:"imdbId,omitempty"`
}

// Calendar returns episodes airing and movies/books released in [start, end]
// (ISO dates, inclusive), merged and date-ordered.
func (d *DB) Calendar(ctx context.Context, start, end string) ([]CalendarEntry, error) {
	eps, err := d.Read.ListEpisodesAiring(ctx, sqlitegen.ListEpisodesAiringParams{
		AirDate: start, AirDate_2: end,
	})
	if err != nil {
		return nil, err
	}
	items, err := d.Read.ListItemsReleasedBetween(ctx, sqlitegen.ListItemsReleasedBetweenParams{
		ReleaseDate: start, ReleaseDate_2: end,
	})
	if err != nil {
		return nil, err
	}
	out := make([]CalendarEntry, 0, len(eps)+len(items))
	for _, e := range eps {
		detail := fmt.Sprintf("S%02dE%02d", e.SeasonNumber, e.EpisodeNumber)
		if e.EpisodeTitle != "" {
			detail += " — " + e.EpisodeTitle
		}
		out = append(out, CalendarEntry{
			Date: e.AirDate, Kind: "episode", MediaItemID: e.MediaItemID,
			Title: e.SeriesTitle, Detail: detail, HasFile: e.HasFile,
			TmdbID: e.TmdbID, ImdbID: e.ImdbID,
		})
	}
	for _, m := range items {
		detail := ""
		if m.Author != "" {
			detail = "by " + m.Author
		}
		out = append(out, CalendarEntry{
			Date: m.ReleaseDate, Kind: m.Kind, MediaItemID: m.ID,
			Title: m.Title, Detail: detail, HasFile: m.HasFile,
			TmdbID: m.TmdbID, ImdbID: m.ImdbID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

// ---- custom formats (Phase 5) ----

// AddCustomFormat stores a scoring rule.
func (d *DB) AddCustomFormat(ctx context.Context, f format.CustomFormat) (int64, error) {
	id, err := d.Write.InsertCustomFormat(ctx, sqlitegen.InsertCustomFormatParams{
		Name: f.Name, Pattern: f.Pattern, Score: int64(f.Score),
	})
	if isConstraint(err) {
		return 0, ErrDuplicate
	}
	return id, err
}

// ListCustomFormats returns every scoring rule.
func (d *DB) ListCustomFormats(ctx context.Context) ([]format.CustomFormat, error) {
	rows, err := d.Read.ListCustomFormats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]format.CustomFormat, 0, len(rows))
	for _, r := range rows {
		out = append(out, format.CustomFormat{
			ID: r.ID, Name: r.Name, Pattern: r.Pattern, Score: int(r.Score),
		})
	}
	return out, nil
}

// DeleteCustomFormat removes a scoring rule.
func (d *DB) DeleteCustomFormat(ctx context.Context, id int64) error {
	return d.Write.DeleteCustomFormat(ctx, id)
}

// ---- import lists (Phase 5) ----

// ImportList is one auto-add source.
type ImportList struct {
	ID               int64             `json:"id"`
	Name             string            `json:"name"`
	Type             string            `json:"type"`
	Config           map[string]string `json:"config"`
	Kind             string            `json:"kind"`
	RootFolderID     int64             `json:"rootFolderId"`
	QualityProfileID int64             `json:"qualityProfileId"`
	Monitored        bool              `json:"monitored"`
	Enabled          bool              `json:"enabled"`
}

// AddImportList stores a list source.
func (d *DB) AddImportList(ctx context.Context, l ImportList) (int64, error) {
	cfg, _ := json.Marshal(l.Config)
	if l.Config == nil {
		cfg = []byte("{}")
	}
	return d.Write.InsertImportList(ctx, sqlitegen.InsertImportListParams{
		Name: l.Name, Type: l.Type, Config: string(cfg), Kind: l.Kind,
		RootFolderID: l.RootFolderID, QualityProfileID: l.QualityProfileID,
		Monitored: boolInt(l.Monitored), Enabled: boolInt(l.Enabled),
	})
}

// ListImportLists returns every list source.
func (d *DB) ListImportLists(ctx context.Context) ([]ImportList, error) {
	rows, err := d.Read.ListImportLists(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ImportList, 0, len(rows))
	for _, r := range rows {
		var cfg map[string]string
		_ = json.Unmarshal([]byte(r.Config), &cfg)
		out = append(out, ImportList{
			ID: r.ID, Name: r.Name, Type: r.Type, Config: cfg, Kind: r.Kind,
			RootFolderID: r.RootFolderID, QualityProfileID: r.QualityProfileID,
			Monitored: r.Monitored != 0, Enabled: r.Enabled != 0,
		})
	}
	return out, nil
}

// DeleteImportList removes a list source.
func (d *DB) DeleteImportList(ctx context.Context, id int64) error {
	return d.Write.DeleteImportList(ctx, id)
}

// ---- bulk edit (Phase 5) ----

// BulkUpdateItem applies the non-nil fields to one media item.
func (d *DB) BulkUpdateItem(ctx context.Context, id int64, monitored *bool, profileID *int64) error {
	p := sqlitegen.UpdateMediaItemBulkParams{ID: id, UpdatedAt: time.Now().UnixMilli()}
	if monitored != nil {
		p.Monitored = sql.NullInt64{Int64: boolInt(*monitored), Valid: true}
	}
	if profileID != nil {
		p.QualityProfileID = sql.NullInt64{Int64: *profileID, Valid: true}
	}
	return d.Write.UpdateMediaItemBulk(ctx, p)
}
