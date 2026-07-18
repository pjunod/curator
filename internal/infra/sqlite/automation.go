package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

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

// DeleteNotifier removes a notifier.
func (d *DB) DeleteNotifier(ctx context.Context, id int64) error {
	return d.Write.DeleteNotifier(ctx, id)
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
