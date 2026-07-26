package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/domain/quality"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
	"github.com/monarr-media/monarr/internal/ports"
)

// ---- profiles ----

// profileDef is the stored shape of a profile (ADR 0014 §1), migrated in place
// by 0019. Only this shape is read: goose is up-only, so there is no version of
// the database that still holds the old allowed+cutoff form, and carrying a
// dual-format reader for a state that cannot exist is how dead code survives.
type profileDef struct {
	Target qualityDef  `json:"target"`
	Floor  *qualityDef `json:"floor"`
}

type qualityDef struct {
	Source     string `json:"source"`
	Resolution int    `json:"resolution"`
}

func (q qualityDef) quality() quality.Quality {
	return quality.Quality{Source: quality.Source(q.Source), Resolution: q.Resolution}
}

func profileFromRow(r sqlitegen.QualityProfile) (quality.Profile, error) {
	var def profileDef
	if err := json.Unmarshal([]byte(r.Definition), &def); err != nil {
		return quality.Profile{}, err
	}
	p := quality.Profile{
		ID: r.ID, Name: r.Name, UpgradesAllowed: r.UpgradesAllowed != 0,
		Target: def.Target.quality(),
	}
	if def.Floor != nil {
		f := def.Floor.quality()
		p.Floor = &f
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

// ErrProfileInUse is returned when a profile cannot be deleted because library
// items, copies, or import lists still point at it. SQLite would happily let
// the delete through and leave them referencing nothing.
var ErrProfileInUse = errors.New("quality profile is still in use")

// profileDefinition renders a profile into the stored JSON.
func profileDefinition(p quality.Profile) (string, error) {
	def := profileDef{Target: qualityDef{Source: string(p.Target.Source), Resolution: p.Target.Resolution}}
	if p.Floor != nil {
		def.Floor = &qualityDef{Source: string(p.Floor.Source), Resolution: p.Floor.Resolution}
	}
	raw, err := json.Marshal(def)
	return string(raw), err
}

// AddProfile stores a new quality profile and returns its id.
func (d *DB) AddProfile(ctx context.Context, p quality.Profile) (int64, error) {
	def, err := profileDefinition(p)
	if err != nil {
		return 0, err
	}
	return d.Write.InsertProfile(ctx, sqlitegen.InsertProfileParams{
		Name: p.Name, Definition: def, UpgradesAllowed: boolInt(p.UpgradesAllowed),
	})
}

// UpdateProfile overwrites a stored profile. ErrNotFound when the id is gone.
func (d *DB) UpdateProfile(ctx context.Context, p quality.Profile) error {
	def, err := profileDefinition(p)
	if err != nil {
		return err
	}
	n, err := d.Write.UpdateProfile(ctx, sqlitegen.UpdateProfileParams{
		Name: p.Name, Definition: def, UpgradesAllowed: boolInt(p.UpgradesAllowed), ID: p.ID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProfile removes a profile, refusing while anything references it.
func (d *DB) DeleteProfile(ctx context.Context, id int64) error {
	refs, err := d.Read.CountProfileReferences(ctx, id)
	if err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: %d item(s), cop(ies) or import list(s) use it", ErrProfileInUse, refs)
	}
	// A default is a reference too, even though no row points at it. Deleting
	// it would leave the setting dangling and every future add of that kind
	// would quietly land back on the built-in.
	if kinds := d.defaultingKinds(ctx, id); len(kinds) > 0 {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = string(k)
		}
		return fmt.Errorf("%w: new %s items use it — pick a different default first",
			ErrProfileIsDefault, strings.Join(names, " and "))
	}
	n, err := d.Write.DeleteProfile(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ProfileReferences counts what would break if a profile were deleted.
func (d *DB) ProfileReferences(ctx context.Context, id int64) (int64, error) {
	return d.Read.CountProfileReferences(ctx, id)
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
		PathMappings: maps, ManualApproval: r.ManualApproval != 0,
		Mode: r.Mode,
	}
}

// clientMode normalizes the update channel. Anything unrecognized —
// including the empty string from an older API client — is 'poll', which
// is the behavior every client has always had. A CHECK constraint would
// otherwise turn a blank field in an old integration into a failed save.
func clientMode(m string) string {
	if m == "push" {
		return "push"
	}
	return "poll"
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
		PathMappings:   string(maps),
		ManualApproval: boolInt(c.ManualApproval),
		Mode:           clientMode(c.Mode),
		AddedAt:        time.Now().UnixMilli(),
	})
}

// UpdateDownloadClient overwrites a stored client (all fields but id and
// added_at). The handler preserves the password when the form sends the
// masked placeholder, so an edit never blanks stored credentials.
func (d *DB) UpdateDownloadClient(ctx context.Context, c ports.ClientConfig) error {
	maps, _ := json.Marshal(c.PathMappings)
	if c.PathMappings == nil {
		maps = []byte("[]")
	}
	return d.Write.UpdateDownloadClient(ctx, sqlitegen.UpdateDownloadClientParams{
		Type: c.Type, Name: c.Name, Url: c.URL, Username: c.Username,
		Password: c.Password, Category: c.Category, Enabled: boolInt(c.Enabled),
		PathMappings:   string(maps),
		ManualApproval: boolInt(c.ManualApproval),
		Mode:           clientMode(c.Mode),
		ID:             c.ID,
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

// HandoffEntry is one step in a download's handoff trace: which step, when
// (unix millis), and a human-readable detail. The slice is persisted as the
// handoff_log JSON column so the whole download → import handoff is laid out
// and inspectable rather than a black box.
type HandoffEntry struct {
	Step   string `json:"step"`
	At     int64  `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// Download is a queue row (persisted Download state machine, blueprint §4.2).
type Download struct {
	ID           int64
	MediaItemID  int64
	CopyID       int64 // 0 = the primary copy
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
	SavePath     string // what the download client reported
	ImportPath   string // where Monarr looks after remote path mapping
	// Transfer names this one transfer end to end (nzbd contract §3.1:
	// t-<id>-<6 hex>). It goes onto the download in the client and will
	// travel on to the media server, so grepping any app's log for it
	// reconstructs the whole story. Empty when the client cannot carry it.
	Transfer  string
	Handoff   []HandoffEntry
	AddedAt   time.Time
	UpdatedAt time.Time
}

func downloadFromRow(r sqlitegen.Download) Download {
	var wants []string
	_ = json.Unmarshal([]byte(r.Wantables), &wants)
	var handoff []HandoffEntry
	_ = json.Unmarshal([]byte(r.HandoffLog), &handoff)
	dl := Download{
		ID: r.ID, MediaItemID: r.MediaItemID, WantableIDs: wants, Season: int(r.Season),
		ReleaseTitle: r.ReleaseTitle, Indexer: r.Indexer, Protocol: r.Protocol,
		Quality: quality.FromString(r.Quality), Size: r.Size, ClientID: r.ClientID,
		Handle: r.Handle, State: r.State, Progress: r.Progress, Error: r.Error,
		SavePath: r.SavePath, ImportPath: r.ImportPath, Transfer: r.Transfer,
		Handoff: handoff,
		AddedAt: time.UnixMilli(r.AddedAt), UpdatedAt: time.UnixMilli(r.UpdatedAt),
	}
	if r.CopyID.Valid {
		dl.CopyID = r.CopyID.Int64
	}
	return dl
}

// InsertDownload records a grab.
func (d *DB) InsertDownload(ctx context.Context, dl Download) (int64, error) {
	wants, _ := json.Marshal(dl.WantableIDs)
	if dl.WantableIDs == nil {
		wants = []byte("[]")
	}
	now := time.Now().UnixMilli()
	p := sqlitegen.InsertDownloadParams{
		MediaItemID: dl.MediaItemID, Wantables: string(wants), Season: int64(dl.Season),
		ReleaseTitle: dl.ReleaseTitle, Indexer: dl.Indexer, Protocol: dl.Protocol,
		Quality: dl.Quality.String(), Size: dl.Size, ClientID: dl.ClientID,
		Handle: dl.Handle, State: dl.State, AddedAt: now, UpdatedAt: now,
	}
	if dl.CopyID != 0 {
		p.CopyID = sql.NullInt64{Int64: dl.CopyID, Valid: true}
	}
	return d.Write.InsertDownload(ctx, p)
}

// SetDownloadHandle records the client-side id and the transfer id, both
// of which are only known after the client has accepted the download.
func (d *DB) SetDownloadHandle(ctx context.Context, id int64, handle, transfer string) error {
	return d.Write.SetDownloadHandle(ctx, sqlitegen.SetDownloadHandleParams{
		Handle: handle, Transfer: transfer, UpdatedAt: time.Now().UnixMilli(), ID: id,
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

// ListInFlightDownloads returns every row that has not finished importing —
// used to suppress re-searching a wantable that already has a download in
// progress or stalled at a failed import.
func (d *DB) ListInFlightDownloads(ctx context.Context) ([]Download, error) {
	rows, err := d.Read.ListInFlightDownloads(ctx)
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

// UpdateDownloadHandoff persists a stage transition together with the
// reported/mapped paths and the full step log. Callers mutate the Download
// in memory (append a HandoffEntry, set State/paths) and pass it here.
func (d *DB) UpdateDownloadHandoff(ctx context.Context, dl Download) error {
	log, _ := json.Marshal(dl.Handoff)
	if dl.Handoff == nil {
		log = []byte("[]")
	}
	return d.Write.UpdateDownloadHandoff(ctx, sqlitegen.UpdateDownloadHandoffParams{
		State: dl.State, Progress: dl.Progress, Error: dl.Error,
		SavePath: dl.SavePath, ImportPath: dl.ImportPath,
		HandoffLog: string(log), UpdatedAt: time.Now().UnixMilli(), ID: dl.ID,
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

// HistoryEvent is one recorded thing that happened to an item: grabbed,
// imported, failed, or a quality claim that did not survive contact with the
// bytes (ADR 0013). Data is the event's own JSON payload, shape by type.
type HistoryEvent struct {
	ID           int64
	At           time.Time
	Type         string
	MediaItemID  int64
	ReleaseTitle string
	Data         string
}

// ListHistory returns the most recent events, newest first.
func (d *DB) ListHistory(ctx context.Context) ([]HistoryEvent, error) {
	rows, err := d.Read.ListHistory(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, HistoryEvent{
			ID: r.ID, At: time.UnixMilli(r.Ts), Type: r.Type,
			MediaItemID: r.MediaItemID, ReleaseTitle: r.ReleaseTitle, Data: r.Data,
		})
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
