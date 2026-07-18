package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/monarr-media/monarr/internal/domain/quality"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
	"github.com/monarr-media/monarr/internal/ports"
)

// ---- profiles ----

type profileDef struct {
	Allowed []struct {
		Source     string `json:"source"`
		Resolution int    `json:"resolution"`
	} `json:"allowed"`
	Cutoff struct {
		Source     string `json:"source"`
		Resolution int    `json:"resolution"`
	} `json:"cutoff"`
}

func profileFromRow(r sqlitegen.QualityProfile) (quality.Profile, error) {
	var def profileDef
	if err := json.Unmarshal([]byte(r.Definition), &def); err != nil {
		return quality.Profile{}, err
	}
	p := quality.Profile{
		ID: r.ID, Name: r.Name, UpgradesAllowed: r.UpgradesAllowed != 0,
		Cutoff: quality.Quality{Source: quality.Source(def.Cutoff.Source), Resolution: def.Cutoff.Resolution},
	}
	for _, a := range def.Allowed {
		p.Allowed = append(p.Allowed, quality.Quality{Source: quality.Source(a.Source), Resolution: a.Resolution})
	}
	return p, nil
}

// ListProfiles returns all quality profiles.
func (d *DB) ListProfiles(ctx context.Context) ([]quality.Profile, error) {
	rows, err := d.Read.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]quality.Profile, 0, len(rows))
	for _, r := range rows {
		p, err := profileFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GetProfile returns one profile or ErrNotFound.
func (d *DB) GetProfile(ctx context.Context, id int64) (quality.Profile, error) {
	r, err := d.Read.GetProfile(ctx, id)
	if err != nil {
		return quality.Profile{}, wrapNotFound(err)
	}
	return profileFromRow(r)
}

// ---- indexers ----

func indexerFromRow(r sqlitegen.Indexer) ports.IndexerConfig {
	var cats []int
	_ = json.Unmarshal([]byte(r.Categories), &cats)
	return ports.IndexerConfig{
		ID: r.ID, Name: r.Name, URL: r.Url, APIKey: r.ApiKey,
		Protocol: r.Protocol, Categories: cats, Enabled: r.Enabled != 0,
	}
}

// AddIndexer stores an indexer config.
func (d *DB) AddIndexer(ctx context.Context, c ports.IndexerConfig) (int64, error) {
	cats, _ := json.Marshal(c.Categories)
	if c.Categories == nil {
		cats = []byte("[]")
	}
	return d.Write.InsertIndexer(ctx, sqlitegen.InsertIndexerParams{
		Name: c.Name, Url: c.URL, ApiKey: c.APIKey, Protocol: c.Protocol,
		Categories: string(cats), Enabled: boolInt(c.Enabled), AddedAt: time.Now().UnixMilli(),
	})
}

// ListIndexers returns all stored indexers.
func (d *DB) ListIndexers(ctx context.Context) ([]ports.IndexerConfig, error) {
	rows, err := d.Read.ListIndexers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.IndexerConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, indexerFromRow(r))
	}
	return out, nil
}

// GetIndexer returns one indexer or ErrNotFound.
func (d *DB) GetIndexer(ctx context.Context, id int64) (ports.IndexerConfig, error) {
	r, err := d.Read.GetIndexer(ctx, id)
	if err != nil {
		return ports.IndexerConfig{}, wrapNotFound(err)
	}
	return indexerFromRow(r), nil
}

// DeleteIndexer removes an indexer config.
func (d *DB) DeleteIndexer(ctx context.Context, id int64) error {
	return d.Write.DeleteIndexer(ctx, id)
}

// ---- download clients ----

func clientFromRow(r sqlitegen.DownloadClient) ports.ClientConfig {
	var maps []ports.PathMapping
	_ = json.Unmarshal([]byte(r.PathMappings), &maps)
	return ports.ClientConfig{
		ID: r.ID, Type: r.Type, Name: r.Name, URL: r.Url,
		Username: r.Username, Password: r.Password, Category: r.Category, Enabled: r.Enabled != 0,
		PathMappings: maps,
	}
}

// AddDownloadClient stores a client config.
func (d *DB) AddDownloadClient(ctx context.Context, c ports.ClientConfig) (int64, error) {
	maps, _ := json.Marshal(c.PathMappings)
	if c.PathMappings == nil {
		maps = []byte("[]")
	}
	return d.Write.InsertDownloadClient(ctx, sqlitegen.InsertDownloadClientParams{
		Type: c.Type, Name: c.Name, Url: c.URL, Username: c.Username,
		Password: c.Password, Category: c.Category, Enabled: boolInt(c.Enabled),
		PathMappings: string(maps),
		AddedAt:      time.Now().UnixMilli(),
	})
}

// ListDownloadClients returns all stored clients.
func (d *DB) ListDownloadClients(ctx context.Context) ([]ports.ClientConfig, error) {
	rows, err := d.Read.ListDownloadClients(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.ClientConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, clientFromRow(r))
	}
	return out, nil
}

// GetDownloadClient returns one client or ErrNotFound.
func (d *DB) GetDownloadClient(ctx context.Context, id int64) (ports.ClientConfig, error) {
	r, err := d.Read.GetDownloadClient(ctx, id)
	if err != nil {
		return ports.ClientConfig{}, wrapNotFound(err)
	}
	return clientFromRow(r), nil
}

// DeleteDownloadClient removes a client config.
func (d *DB) DeleteDownloadClient(ctx context.Context, id int64) error {
	return d.Write.DeleteDownloadClient(ctx, id)
}

// ---- downloads (queue) ----

// Download is a queue row (persisted Download state machine, blueprint §4.2).
type Download struct {
	ID           int64
	MediaItemID  int64
	WantableIDs  []string
	Season       int
	ReleaseTitle string
	Indexer      string
	Protocol     string
	Quality      quality.Quality
	Size         int64
	ClientID     int64
	Handle       string
	State        string
	Progress     float64
	Error        string
	AddedAt      time.Time
	UpdatedAt    time.Time
}

func downloadFromRow(r sqlitegen.Download) Download {
	var wants []string
	_ = json.Unmarshal([]byte(r.Wantables), &wants)
	return Download{
		ID: r.ID, MediaItemID: r.MediaItemID, WantableIDs: wants, Season: int(r.Season),
		ReleaseTitle: r.ReleaseTitle, Indexer: r.Indexer, Protocol: r.Protocol,
		Quality: quality.FromString(r.Quality), Size: r.Size, ClientID: r.ClientID,
		Handle: r.Handle, State: r.State, Progress: r.Progress, Error: r.Error,
		AddedAt: time.UnixMilli(r.AddedAt), UpdatedAt: time.UnixMilli(r.UpdatedAt),
	}
}

// InsertDownload records a grab.
func (d *DB) InsertDownload(ctx context.Context, dl Download) (int64, error) {
	wants, _ := json.Marshal(dl.WantableIDs)
	if dl.WantableIDs == nil {
		wants = []byte("[]")
	}
	now := time.Now().UnixMilli()
	return d.Write.InsertDownload(ctx, sqlitegen.InsertDownloadParams{
		MediaItemID: dl.MediaItemID, Wantables: string(wants), Season: int64(dl.Season),
		ReleaseTitle: dl.ReleaseTitle, Indexer: dl.Indexer, Protocol: dl.Protocol,
		Quality: dl.Quality.String(), Size: dl.Size, ClientID: dl.ClientID,
		Handle: dl.Handle, State: dl.State, AddedAt: now, UpdatedAt: now,
	})
}

// ListActiveDownloads returns rows still moving through the state machine.
func (d *DB) ListActiveDownloads(ctx context.Context) ([]Download, error) {
	rows, err := d.Read.ListActiveDownloads(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}

// ListRecentDownloads returns the last 100 rows, any state.
func (d *DB) ListRecentDownloads(ctx context.Context) ([]Download, error) {
	rows, err := d.Read.ListRecentDownloads(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}

// UpdateDownloadState transitions a queue row.
func (d *DB) UpdateDownloadState(ctx context.Context, id int64, state string, progress float64, errMsg string) error {
	return d.Write.UpdateDownloadState(ctx, sqlitegen.UpdateDownloadStateParams{
		State: state, Progress: progress, Error: errMsg,
		UpdatedAt: time.Now().UnixMilli(), ID: id,
	})
}

// DeleteDownload removes a queue row.
func (d *DB) DeleteDownload(ctx context.Context, id int64) error {
	return d.Write.DeleteDownload(ctx, id)
}

// GetDownload returns one queue row or ErrNotFound.
func (d *DB) GetDownload(ctx context.Context, id int64) (Download, error) {
	r, err := d.Read.GetDownload(ctx, id)
	if err != nil {
		return Download{}, wrapNotFound(err)
	}
	return downloadFromRow(r), nil
}

// ---- file quality + history ----

// SetFileQuality stores the parsed quality for a media file.
func (d *DB) SetFileQuality(ctx context.Context, fileID int64, q quality.Quality) error {
	return d.Write.SetFileQuality(ctx, sqlitegen.SetFileQualityParams{Quality: q.String(), ID: fileID})
}

// BestQualityForItem returns the highest-ranked quality among an item's
// files; ok=false when the item has no files with known quality.
func (d *DB) BestQualityForItem(ctx context.Context, itemID int64) (quality.Quality, bool, error) {
	rows, err := d.Read.ListFileQualitiesForItem(ctx, sql.NullInt64{Int64: itemID, Valid: true})
	if err != nil {
		return quality.Quality{}, false, err
	}
	var best *quality.Quality
	for _, r := range rows {
		if r.Quality == "" {
			continue
		}
		q := quality.FromString(r.Quality)
		if best == nil || quality.Better(q, *best) {
			best = &q
		}
	}
	if best == nil {
		return quality.Quality{}, false, nil
	}
	return *best, true, nil
}

// FileQualities maps file id → parsed quality for an item's files (files
// with unknown quality are omitted).
func (d *DB) FileQualities(ctx context.Context, itemID int64) (map[int64]quality.Quality, error) {
	rows, err := d.Read.ListFileQualitiesForItem(ctx, sql.NullInt64{Int64: itemID, Valid: true})
	if err != nil {
		return nil, err
	}
	out := map[int64]quality.Quality{}
	for _, r := range rows {
		if r.Quality != "" {
			out[r.ID] = quality.FromString(r.Quality)
		}
	}
	return out, nil
}

// AddHistory appends an event (grabbed | imported | failed).
func (d *DB) AddHistory(ctx context.Context, eventType string, mediaItemID int64, releaseTitle string, data any) error {
	raw, _ := json.Marshal(data)
	if data == nil {
		raw = []byte("{}")
	}
	return d.Write.InsertHistory(ctx, sqlitegen.InsertHistoryParams{
		Ts: time.Now().UnixMilli(), Type: eventType,
		MediaItemID: mediaItemID, ReleaseTitle: releaseTitle, Data: string(raw),
	})
}
