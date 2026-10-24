package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/language"
	"github.com/pjunod/monarr/internal/domain/quality"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
	"github.com/pjunod/monarr/internal/ports"
)

// ---- profiles ----

// profileDef is the stored shape of a profile (ADR 0014 §1), migrated in place
// by 0019. Only this shape is read: goose is up-only, so there is no version of
// the database that still holds the old allowed+cutoff form, and carrying a
// dual-format reader for a state that cannot exist is how dead code survives.
type profileDef struct {
	Target           qualityDef  `json:"target"`
	Floor            *qualityDef `json:"floor"`
	DownloadPriority int         `json:"downloadPriority,omitempty"`
	// Languages is the ADR 0022 audio requirement. Omitted when empty, so
	// every definition written before the rule existed reads back as
	// "no opinion" without a migration.
	Languages []string `json:"languages,omitempty"`
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
		Target: def.Target.quality(), DownloadPriority: def.DownloadPriority,
		Languages: language.Normalize(def.Languages),
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
	def := profileDef{
		Target:           qualityDef{Source: string(p.Target.Source), Resolution: p.Target.Resolution},
		DownloadPriority: p.DownloadPriority,
		Languages:        language.Normalize(p.Languages),
	}
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
		return fmt.Errorf("%w: new %s items use it — pick a different default first",
			ErrProfileIsDefault, strings.Join(kinds, " and "))
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
		ID: r.ID, Name: r.Name, URL: r.Url, APIKey: r.ApiKey, DailyRequestCap: int(r.DailyRequestCap),
		Protocol: r.Protocol, Categories: cats, Enabled: r.Enabled != 0,
	}
}

// AddIndexer stores an indexer config.
func (d *DB) AddIndexer(ctx context.Context, c ports.IndexerConfig) (int64, error) {
	cats, _ := json.Marshal(c.Categories)
	if c.Categories == nil {
		cats = []byte("[]")
	}
	id, err := d.Write.InsertIndexer(ctx, sqlitegen.InsertIndexerParams{
		Name: c.Name, Url: c.URL, ApiKey: c.APIKey, Protocol: c.Protocol,
		Categories: string(cats), Enabled: boolInt(c.Enabled), AddedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return 0, err
	}
	if err = d.SetDailyRequestCap(ctx, id, c.DailyRequestCap); err != nil {
		return 0, err
	}
	return id, nil
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
	n, err := d.Write.DeleteIndexer(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ImportedWithPayload returns imported downloads whose payload is still on the
// client's disk, oldest first, capped at limit.
func (d *DB) ImportedWithPayload(ctx context.Context, limit int64) ([]Download, error) {
	rows, err := d.Read.ListImportedWithPayload(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}

// MarkPayloadRemoved records that a download's bytes are no longer on the
// client, so the sweep does not ask again.
func (d *DB) MarkPayloadRemoved(ctx context.Context, id int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	if err := q.MarkPayloadRemoved(ctx, sqlitegen.MarkPayloadRemovedParams{
		UpdatedAt: time.Now().UnixMilli(), ID: id,
	}); err != nil {
		return err
	}
	// The update first writes the durable removal receipt. Only then may a
	// dismissed Activity row lose its remaining cleanup ownership.
	if _, err := q.DeleteDismissedImportedDownload(ctx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- download clients ----

func clientFromRow(r sqlitegen.DownloadClient) ports.ClientConfig {
	var maps []ports.PathMapping
	_ = json.Unmarshal([]byte(r.PathMappings), &maps)
	return ports.ClientConfig{
		ID: r.ID, Type: r.Type, Name: r.Name, URL: r.Url,
		Username: r.Username, Password: r.Password, Category: r.Category, Enabled: r.Enabled != 0,
		PathMappings: maps, ManualApproval: r.ManualApproval != 0,
		Mode: r.Mode, RemoveCompleted: r.RemoveCompleted != 0,
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
		PathMappings:    string(maps),
		ManualApproval:  boolInt(c.ManualApproval),
		Mode:            clientMode(c.Mode),
		RemoveCompleted: boolInt(c.RemoveCompleted),
		AddedAt:         time.Now().UnixMilli(),
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
		PathMappings:    string(maps),
		ManualApproval:  boolInt(c.ManualApproval),
		Mode:            clientMode(c.Mode),
		RemoveCompleted: boolInt(c.RemoveCompleted),
		ID:              c.ID,
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
	n, err := d.Write.DeleteDownloadClient(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
	// Paths is populated only for a queued manual import. Keeping the exact
	// selection in the durable trace means a process restart or later retry
	// cannot silently widen "these files" back to "everything in the folder".
	Paths             []string              `json:"paths,omitempty"`
	ReviewFingerprint string                `json:"reviewFingerprint,omitempty"`
	ReviewPath        string                `json:"reviewPath,omitempty"`
	ReviewFiles       []HandoffFileSnapshot `json:"reviewFiles,omitempty"`
	TargetMediaItemID int64                 `json:"targetMediaItemId,omitempty"`
	TargetCopyID      int64                 `json:"targetCopyId,omitempty"`
	TargetIdentity    string                `json:"targetIdentity,omitempty"`
}

type HandoffFileSnapshot struct {
	Path             string `json:"path"`
	SnapshotIdentity string `json:"snapshotIdentity"`
}

// Download is a queue row (persisted Download state machine, blueprint §4.2).
type Download struct {
	PlanID           int64
	CandidateKey     string
	SubmissionPhase  string
	ExecutionPayload string
	ReservedEpisodes []int64
	ParkedReason     string
	ParkedAt         time.Time
	Superseded       bool
	Observation      string
	CleanupPending   bool
	ID               int64
	MediaItemID      int64
	CopyID           int64 // 0 = the primary copy
	WantableIDs      []string
	Season           int
	ReleaseTitle     string
	Indexer          string
	Protocol         string
	Quality          quality.Quality
	Size             int64
	ClientID         int64
	Handle           string
	State            string
	Progress         float64
	Error            string
	SavePath         string // what the download client reported
	ImportPath       string // where Monarr looks after remote path mapping
	// Transfer is an opaque end-to-end request token carried by compatible
	// download clients and the media server. New admissions use at least 128
	// bits of randomness; persisted legacy tokens remain valid. Empty when the
	// client cannot carry a request token.
	Transfer      string
	Handoff       []HandoffEntry
	MatchEvidence domain.MatchEvidence
	AddedAt       time.Time
	UpdatedAt     time.Time

	RunnerControl             string
	GenerationKey             string
	PayloadRemoved            bool
	Intent                    string
	RequestID                 string
	RequestFingerprint        string
	BlacklistOverride         bool
	DeliveryExpected          bool
	RequestTokenIdempotent    bool
	ExactTransferCorrelation  bool
	ReplacementOperationID    string
	SourceGUID                string
	SourceIndexerID           int64
	RunnerInstanceID          string
	RunnerAdmissionGeneration string
}

type ManualGrabRequest struct {
	RequestID          string
	RequestFingerprint string
	DownloadID         int64
	GenerationKey      string
	CopyID             int64
	CreatedAt          time.Time
}

var ErrManualGrabRequestRetained = errors.New("manual request is retained but its download row has been removed")
var ErrManualSubmissionChanged = errors.New("manual submission generation changed before acknowledgement")

func downloadFromRow(r sqlitegen.Download) Download {
	var wants []string
	_ = json.Unmarshal([]byte(r.Wantables), &wants)
	var handoff []HandoffEntry
	_ = json.Unmarshal([]byte(r.HandoffLog), &handoff)
	var evidence domain.MatchEvidence
	_ = json.Unmarshal([]byte(r.MatchEvidence), &evidence)
	var reserved []int64
	_ = json.Unmarshal([]byte(r.ReservedEpisodes), &reserved)
	dl := Download{
		PlanID: r.PlanID.Int64, CandidateKey: r.CandidateKey, SubmissionPhase: r.SubmissionPhase, ExecutionPayload: r.ExecutionPayload, ReservedEpisodes: reserved, ParkedReason: r.ParkedReason, ParkedAt: time.UnixMilli(r.ParkedAt), Superseded: r.Superseded != 0, Observation: r.Observation, CleanupPending: r.CleanupPending != 0,
		PayloadRemoved: r.PayloadRemoved != 0, GenerationKey: r.GenerationKey, ID: r.ID, MediaItemID: r.MediaItemID, WantableIDs: wants, Season: int(r.Season),
		Intent: r.Intent, RequestID: r.RequestID, RequestFingerprint: r.RequestFingerprint,
		BlacklistOverride: r.BlacklistOverride != 0, DeliveryExpected: r.DeliveryExpected != 0,
		RequestTokenIdempotent:   r.RequestTokenIdempotent != 0,
		ExactTransferCorrelation: r.ExactTransferCorrelation != 0,
		ReplacementOperationID:   r.ReplacementOperationID,
		SourceGUID:               r.SourceGuid, SourceIndexerID: r.SourceIndexerID,
		RunnerInstanceID: r.RunnerInstanceID, RunnerAdmissionGeneration: r.RunnerAdmissionGeneration,
		ReleaseTitle: r.ReleaseTitle, Indexer: r.Indexer, Protocol: r.Protocol,
		Quality: quality.FromString(r.Quality), Size: r.Size, ClientID: r.ClientID,
		Handle: r.Handle, State: r.State, Progress: r.Progress, Error: r.Error,
		SavePath: r.SavePath, ImportPath: r.ImportPath, Transfer: r.Transfer,
		Handoff: handoff, MatchEvidence: evidence, RunnerControl: r.RunnerControl,
		AddedAt: time.UnixMilli(r.AddedAt), UpdatedAt: time.UnixMilli(r.UpdatedAt),
	}
	if r.CopyID.Valid {
		dl.CopyID = r.CopyID.Int64
	}
	return dl
}

// InsertDownload records a grab.
func (d *DB) InsertDownload(ctx context.Context, dl Download) (int64, error) {
	inserted, err := d.InsertDownloadRecord(ctx, dl)
	if err != nil {
		return 0, err
	}
	return inserted.ID, nil
}

// InsertDownloadRecord returns the persisted row, including its immutable
// generation key. Callers that keep a download reference across asynchronous
// work must carry that key so a reused integer ID cannot retarget the work.
func (d *DB) InsertDownloadRecord(ctx context.Context, dl Download) (Download, error) {
	p, err := downloadInsertParams(dl)
	if err != nil {
		return Download{}, err
	}
	id, err := d.Write.InsertDownload(ctx, p)
	if err != nil {
		return Download{}, err
	}
	if dl.ReplacementOperationID != "" {
		result, e := d.W.ExecContext(ctx, `UPDATE downloads SET replacement_operation_id=? WHERE id=? AND generation_key=?`, dl.ReplacementOperationID, id, p.GenerationKey)
		if e != nil {
			return Download{}, e
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			if e != nil {
				return Download{}, e
			}
			return Download{}, ErrNotFound
		}
	}
	return d.GetDownload(ctx, id)
}

func downloadInsertParams(dl Download) (sqlitegen.InsertDownloadParams, error) {
	wants, _ := json.Marshal(dl.WantableIDs)
	if dl.WantableIDs == nil {
		wants = []byte("[]")
	}
	now := time.Now().UnixMilli()
	generation, err := newDownloadGenerationKey()
	if err != nil {
		return sqlitegen.InsertDownloadParams{}, fmt.Errorf("create download generation identity: %w", err)
	}
	evidence, _ := json.Marshal(dl.MatchEvidence)
	submissionPhase := dl.SubmissionPhase
	if submissionPhase == "" {
		// Legacy callers and raw fixture-style inserts represent a request that
		// already exists in the client. Keep the migration's historical default
		// semantics instead of explicitly overriding the CHECKed column with ''.
		submissionPhase = "submitted"
	}
	p := sqlitegen.InsertDownloadParams{
		MediaItemID: dl.MediaItemID, Wantables: string(wants), Season: int64(dl.Season),
		ReleaseTitle: dl.ReleaseTitle, Indexer: dl.Indexer, Protocol: dl.Protocol,
		Quality: dl.Quality.String(), Size: dl.Size, ClientID: dl.ClientID,
		Handle: dl.Handle, State: dl.State, AddedAt: now, UpdatedAt: now,
		MatchEvidence: string(evidence), GenerationKey: generation,
		Intent: defaultDownloadIntent(dl.Intent), RequestID: dl.RequestID,
		RequestFingerprint: dl.RequestFingerprint, BlacklistOverride: boolInt(dl.BlacklistOverride),
		DeliveryExpected:         boolInt(dl.DeliveryExpected),
		RequestTokenIdempotent:   boolInt(dl.RequestTokenIdempotent),
		ExactTransferCorrelation: boolInt(dl.ExactTransferCorrelation),
		ReplacementOperationID:   dl.ReplacementOperationID, SourceGuid: dl.SourceGUID, SourceIndexerID: dl.SourceIndexerID,
		Transfer: dl.Transfer, SubmissionPhase: submissionPhase, ExecutionPayload: dl.ExecutionPayload,
	}
	if dl.CopyID != 0 {
		p.CopyID = sql.NullInt64{Int64: dl.CopyID, Valid: true}
	}
	return p, nil
}

func defaultDownloadIntent(intent string) string {
	if intent == "" {
		return "automatic"
	}
	return intent
}

// GetDownloadByRequestID resolves a persisted manual request before any
// external client call. Repeating the same request therefore returns the
// original generation and transfer token instead of creating another copy.
func (d *DB) GetManualGrabRequest(ctx context.Context, requestID string) (ManualGrabRequest, error) {
	var row ManualGrabRequest
	var createdAt int64
	err := d.R.QueryRowContext(ctx, `SELECT request_id, request_fingerprint, download_id, generation_key, copy_id, created_at
		FROM manual_grab_requests WHERE request_id=?`, requestID).Scan(&row.RequestID, &row.RequestFingerprint,
		&row.DownloadID, &row.GenerationKey, &row.CopyID, &createdAt)
	if err != nil {
		return ManualGrabRequest{}, wrapNotFound(err)
	}
	row.CreatedAt = time.UnixMilli(createdAt)
	return row, nil
}

// InsertManualAdditionalDownload atomically reserves the independent copy and
// its download request before any external Add call. A lost Add response can
// be reconciled by the persisted transfer token without allocating a second
// destination.
func (d *DB) InsertManualAdditionalDownload(ctx context.Context, cp domain.MediaCopy, dl Download, transferForID func(int64) (string, error)) (Download, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return Download{}, err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	copyParams := sqlitegen.InsertMediaCopyParams{
		MediaItemID: cp.MediaItemID, Name: cp.Name, BookType: string(cp.BookType),
		QualityProfileID: cp.QualityProfileID, Path: cp.Path,
		Monitored: 0, AddedAt: time.Now().UnixMilli(),
	}
	if cp.RootFolderID != 0 {
		copyParams.RootFolderID = sql.NullInt64{Int64: cp.RootFolderID, Valid: true}
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM media_items WHERE path=?
		UNION ALL SELECT 1 FROM media_copies WHERE path=?
	)`, cp.Path, cp.Path).Scan(&taken); err != nil {
		return Download{}, err
	}
	if taken != 0 {
		return Download{}, ErrFolderTaken
	}
	copyID, err := q.InsertMediaCopy(ctx, copyParams)
	if err != nil {
		return Download{}, err
	}
	dl.CopyID = copyID
	for i := range dl.WantableIDs {
		dl.WantableIDs[i] = fmt.Sprintf("%s:c%d", dl.WantableIDs[i], copyID)
	}
	dl.Intent = "manual_additional"
	params, err := downloadInsertParams(dl)
	if err != nil {
		return Download{}, err
	}
	id, err := q.InsertDownload(ctx, params)
	if err != nil {
		return Download{}, err
	}
	transfer, err := transferForID(id)
	if err != nil {
		return Download{}, err
	}
	if err := q.SetDownloadHandle(ctx, sqlitegen.SetDownloadHandleParams{Transfer: transfer, UpdatedAt: time.Now().UnixMilli(), ID: id}); err != nil {
		return Download{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE downloads SET execution_payload=?,submission_phase='pending' WHERE id=?`, dl.ExecutionPayload, id); err != nil {
		return Download{}, err
	}
	r, err := q.GetDownload(ctx, id)
	if err != nil {
		return Download{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO manual_grab_requests
		(request_id, request_fingerprint, download_id, generation_key, copy_id, created_at)
		VALUES(?,?,?,?,?,?)`, dl.RequestID, dl.RequestFingerprint, id, r.GenerationKey, copyID, time.Now().UnixMilli()); err != nil {
		return Download{}, err
	}
	if err := tx.Commit(); err != nil {
		return Download{}, err
	}
	return downloadFromRow(r), nil
}

// SetDownloadHandle records the client-side id and the transfer id, both
// of which are only known after the client has accepted the download.
func (d *DB) SetDownloadHandle(ctx context.Context, id int64, handle, transfer string) error {
	return d.Write.SetDownloadHandle(ctx, sqlitegen.SetDownloadHandleParams{
		Handle: handle, Transfer: transfer, UpdatedAt: time.Now().UnixMilli(), ID: id,
	})
}

// CommitManualSubmission records the client handle and submitted phase as one
// durable fact. A row must never appear submitted without the exact handle
// needed to reconcile later client observations.
func (d *DB) CommitManualSubmission(ctx context.Context, id int64, generation, handle, transfer string) error {
	if generation == "" || handle == "" || transfer == "" {
		return fmt.Errorf("manual submission acknowledgement is missing its generation, handle, or transfer token")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET handle=?,transfer=?,submission_phase='submitted',manual_retry_at=0,
		manual_retry_attempts=0,manual_blocked_at=0,error='',updated_at=?
		WHERE id=? AND generation_key=? AND intent='manual_additional' AND transfer=?`,
		handle, transfer, time.Now().UnixMilli(), id, generation, transfer)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrManualSubmissionChanged
	}
	return tx.Commit()
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

// ListImportRetryCandidates returns every environmental import failure still
// requiring attention. Unlike Activity's recent page, this query is not
// capped and is independent of Activity dismissal.
func (d *DB) ListImportRetryCandidates(ctx context.Context) ([]Download, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM downloads
		WHERE state='failed' AND delivery_expected=0
		AND error LIKE '%' || ? || '%'
		AND (import_path != '' OR save_path != '')
		ORDER BY updated_at ASC, id ASC`, "import stopped:")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(ids))
	for _, id := range ids {
		dl, err := d.GetDownload(ctx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, dl)
	}
	return out, nil
}

// ListFolderlessFailedDownloadIDsForItem returns the older failed imports
// that became actionable when an item's missing destination was repaired.
func (d *DB) ListFolderlessFailedDownloadIDsForItem(ctx context.Context, itemID int64) ([]int64, error) {
	return d.Read.ListFolderlessFailedDownloadIDsForItem(ctx, itemID)
}

// UpdateDownloadControl commits suppression and control facts atomically.
func (d *DB) UpdateDownloadControl(ctx context.Context, dl Download) error {
	return d.Write.UpdateDownloadControl(ctx, sqlitegen.UpdateDownloadControlParams{
		RunnerControl: dl.RunnerControl, State: dl.State, Progress: dl.Progress,
		Error: dl.Error, UpdatedAt: time.Now().UnixMilli(), ID: dl.ID,
	})
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

// CommitManualImportAdmission writes the target, selected-file handoff, and
// queue-row state as one durable admission. For a new manual import, the row
// itself is inserted in the same transaction; a failed handoff write must not
// leave a row the caller cannot identify or safely retry.
func (d *DB) CommitManualImportAdmission(ctx context.Context, dl Download) (Download, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return Download{}, err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	if dl.ID == 0 {
		params, err := downloadInsertParams(dl)
		if err != nil {
			return Download{}, err
		}
		dl.ID, err = q.InsertDownload(ctx, params)
		if err != nil {
			return Download{}, err
		}
		dl.GenerationKey = params.GenerationKey
	} else if dl.GenerationKey == "" {
		return Download{}, fmt.Errorf("manual import generation identity is required")
	}

	handoff, err := json.Marshal(dl.Handoff)
	if err != nil {
		return Download{}, err
	}
	if dl.Handoff == nil {
		handoff = []byte("[]")
	}
	result, err := tx.ExecContext(ctx, `UPDATE downloads SET media_item_id=?,copy_id=?,state=?,progress=?,error=?,
		save_path=?,import_path=?,handoff_log=?,updated_at=? WHERE id=? AND generation_key=?`,
		dl.MediaItemID, sql.NullInt64{Int64: dl.CopyID, Valid: dl.CopyID != 0}, dl.State, dl.Progress, dl.Error,
		dl.SavePath, dl.ImportPath, string(handoff), time.Now().UnixMilli(), dl.ID, dl.GenerationKey)
	if err != nil {
		return Download{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Download{}, err
	}
	if rows != 1 {
		return Download{}, ErrNotFound
	}
	row, err := q.GetDownload(ctx, dl.ID)
	if err != nil {
		return Download{}, err
	}
	if err := tx.Commit(); err != nil {
		return Download{}, err
	}
	return downloadFromRow(row), nil
}

// UpdateDownloadTarget persists the target chosen in a manual-import form.
// It is deliberately separate from stage transitions: many automatic
// handoff callers carry only a partial Download and must never erase copy_id.
func (d *DB) UpdateDownloadTarget(ctx context.Context, dl Download) error {
	return d.Write.UpdateDownloadTarget(ctx, sqlitegen.UpdateDownloadTargetParams{
		MediaItemID: dl.MediaItemID,
		CopyID:      sql.NullInt64{Int64: dl.CopyID, Valid: dl.CopyID != 0},
		ImportPath:  dl.ImportPath,
		UpdatedAt:   time.Now().UnixMilli(),
		ID:          dl.ID,
	})
}

// AppendHandoffStep adds one entry to a download's trace without touching
// its state.
//
// The trace's other writer is the acquisition service, which owns the state
// machine and moves it forward. This one is for steps that happen AFTER the
// download is finished with — telling plurx, above all. Those must be able
// to write "here is what happened next" onto the same trace without being
// able to move the download backwards, or a slow notification could
// resurrect an import that already completed.
//
// A download that has since been removed is not an error: the notification
// outliving its queue row is ordinary, and failing here would turn a
// tidy-up into a delivery failure.
func (d *DB) AppendHandoffStep(ctx context.Context, downloadID int64, step, detail string) error {
	if downloadID == 0 {
		return nil
	}
	dl, err := d.GetDownload(ctx, downloadID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	dl.Handoff = append(dl.Handoff, HandoffEntry{
		Step: step, At: time.Now().UnixMilli(), Detail: detail,
	})
	return d.UpdateDownloadHandoff(ctx, dl)
}

// DeleteDownload removes a queue row.
func (d *DB) DeleteDownload(ctx context.Context, id int64) error {
	dl, err := d.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if dl.SubmissionPhase == "pending" || dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "uncertain" || dl.ParkedAt.UnixMilli() > 0 || dl.CleanupPending {
		return fmt.Errorf("download retains unresolved execution or payload custody")
	}
	if n, err := d.Write.DismissImportedDownload(ctx, id); err != nil || n > 0 {
		return err
	}
	n, err := d.Write.DeleteDownload(ctx, id)
	if err == nil && n == 0 {
		return fmt.Errorf("download retains unresolved placement custody")
	}
	return err
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

// ---- the Activity page's own reads -------------------------------------
//
// The page used to render whatever `ListRecentDownloads` handed back — one
// table, every state, capped at a hundred rows and growing until it hit the
// cap. These are the queries that let it show a page of one group instead:
// the work in flight is always complete, and everything terminal is paged.

// QueueFilter names a group of states, not a state: "active" is everything
// still moving or awaiting a decision, which is the one view that has to be
// whole. "" is everything.
type QueueFilter string

const (
	QueueActive   QueueFilter = "active"
	QueueImported QueueFilter = "imported"
	QueueFailed   QueueFilter = "failed"
)

// QueuePage returns one page of the queue, filtered by group and by a
// case-insensitive substring of the release title.
func (d *DB) QueuePage(ctx context.Context, filter QueueFilter, search string, limit, offset int) ([]Download, error) {
	f := string(filter)
	rows, err := d.Read.ListQueuePage(ctx, sqlitegen.ListQueuePageParams{
		Column1: f, Column2: f, State: f,
		Column4: search, LOWER: search,
		Limit: int64(limit), Offset: int64(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}

// QueueCounts is how many downloads are in each state, for the section
// headings — so a collapsed "Finished (1,284)" costs one aggregate rather
// than 1,284 rows over the wire.
func (d *DB) QueueCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := d.Read.CountDownloadsByState(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.State] = r.N
	}
	return out, nil
}

// ClearImportedDownloads dismisses finished Activity. Imported rows with
// remaining payloads stay internal until cleanup confirms their removal.
func (d *DB) ClearImportedDownloads(ctx context.Context) (int64, error) {
	return d.dismissTerminalDownloads(ctx, nil)
}

// ClearFailedDownloads drops every failed row from Activity. It deliberately
// leaves files, blocklists, library records, and download clients alone.
func (d *DB) ClearFailedDownloads(ctx context.Context) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	dismissed, err := q.DismissFailedDownloadsWithPendingRunnerReceipt(ctx)
	if err != nil {
		return 0, err
	}
	deleted, err := q.DeleteFailedDownloads(ctx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return dismissed + deleted, nil
}

// PruneTerminalDownloads ages terminal Activity out without discarding pending
// imported cleanup. Never touches a row that is still moving, however old.
func (d *DB) PruneTerminalDownloads(ctx context.Context, before time.Time) (int64, error) {
	return d.dismissTerminalDownloads(ctx, &before)
}

func (d *DB) dismissTerminalDownloads(ctx context.Context, before *time.Time) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	var hidden, deleted int64
	if before == nil {
		hidden, err = q.DismissImportedDownloads(ctx)
	} else {
		hidden, err = q.DismissImportedDownloadsBefore(ctx, before.UnixMilli())
	}
	if err != nil {
		return 0, err
	}
	if before == nil {
		deleted, err = q.DeleteImportedDownloads(ctx)
	} else {
		deleted, err = q.DeleteTerminalDownloadsBefore(ctx, before.UnixMilli())
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return hidden + deleted, nil
}

// PruneHistory deletes history events older than `before`.
func (d *DB) PruneHistory(ctx context.Context, before time.Time) (int64, error) {
	return d.Write.DeleteHistoryBefore(ctx, before.UnixMilli())
}

// HistoryCount is the total number of recorded events.
func (d *DB) HistoryCount(ctx context.Context) (int64, error) {
	return d.Read.CountHistory(ctx)
}

// HistoryPage returns one page of the timeline, newest first. `item` of 0
// means every item.
func (d *DB) HistoryPage(ctx context.Context, item int64, limit, offset int) ([]HistoryEvent, error) {
	var rows []sqlitegen.HistoryEvent
	var err error
	if item == 0 {
		rows, err = d.Read.ListHistoryPage(ctx, sqlitegen.ListHistoryPageParams{
			Limit: int64(limit), Offset: int64(offset),
		})
	} else {
		rows, err = d.Read.ListHistoryPageForItem(ctx, sqlitegen.ListHistoryPageForItemParams{
			MediaItemID: item, Limit: int64(limit), Offset: int64(offset),
		})
	}
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
