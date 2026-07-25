package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is returned when a unique constraint rejects an insert
// (e.g. the same TMDB id added twice, or a root folder path re-added).
var ErrDuplicate = errors.New("already exists")

func wrapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func isConstraint(err error) bool {
	// modernc/sqlite constraint errors carry the SQLITE_CONSTRAINT text.
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

// ---- mapping ----

func itemToDomain(r sqlitegen.MediaItem) domain.MediaItem {
	var genres []string
	_ = json.Unmarshal([]byte(r.Genres), &genres)
	var ratings []domain.Rating
	_ = json.Unmarshal([]byte(r.Ratings), &ratings)
	item := domain.MediaItem{
		ID:        r.ID,
		Kind:      domain.MediaKind(r.Kind),
		Title:     r.Title,
		SortTitle: r.SortTitle,
		Year:      int(r.Year),
		Author:    r.Author,
		IDs: domain.ExternalIDs{
			TMDB: r.TmdbID, IMDB: r.ImdbID, TVDB: r.TvdbID,
			ISBN13: r.Isbn13, OLID: r.Olid, ASIN: r.Asin,
		},
		Overview:         r.Overview,
		PosterPath:       r.PosterPath,
		BackdropPath:     r.BackdropPath,
		Genres:           genres,
		Status:           r.Status,
		ReleaseDate:      r.ReleaseDate,
		Runtime:          int(r.Runtime),
		Rating:           r.Rating,
		RatingVotes:      int(r.RatingVotes),
		Ratings:          ratings,
		Source:           r.Source,
		Monitored:        r.Monitored != 0,
		QualityProfileID: r.QualityProfileID,
		Path:             r.Path,
		Ended:            r.Ended != 0,
		AddedAt:          time.UnixMilli(r.AddedAt),
		UpdatedAt:        time.UnixMilli(r.UpdatedAt),
	}
	if r.RootFolderID.Valid {
		item.RootFolderID = r.RootFolderID.Int64
	}
	return item
}

func insertParams(m domain.MediaItem, now time.Time) sqlitegen.InsertMediaItemParams {
	genres, _ := json.Marshal(m.Genres)
	if m.Genres == nil {
		genres = []byte("[]")
	}
	ratings := marshalRatings(m.Ratings)
	profileID := m.QualityProfileID
	if profileID == 0 {
		profileID = 1
	}
	p := sqlitegen.InsertMediaItemParams{
		Source:           m.Source,
		Kind:             string(m.Kind),
		Title:            m.Title,
		SortTitle:        m.SortTitle,
		Year:             int64(m.Year),
		Author:           m.Author,
		QualityProfileID: profileID,
		TmdbID:           m.IDs.TMDB,
		ImdbID:           m.IDs.IMDB,
		TvdbID:           m.IDs.TVDB,
		Isbn13:           m.IDs.ISBN13,
		Olid:             m.IDs.OLID,
		Asin:             m.IDs.ASIN,
		Overview:         m.Overview,
		PosterPath:       m.PosterPath,
		BackdropPath:     m.BackdropPath,
		Genres:           string(genres),
		Status:           m.Status,
		ReleaseDate:      m.ReleaseDate,
		Runtime:          int64(m.Runtime),
		Rating:           m.Rating,
		RatingVotes:      int64(m.RatingVotes),
		Ratings:          ratings,
		Monitored:        boolInt(m.Monitored),
		Path:             m.Path,
		Ended:            boolInt(m.Ended),
		AddedAt:          now.UnixMilli(),
		UpdatedAt:        now.UnixMilli(),
	}
	if m.RootFolderID != 0 {
		p.RootFolderID = sql.NullInt64{Int64: m.RootFolderID, Valid: true}
	}
	return p
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func marshalRatings(rs []domain.Rating) string {
	if rs == nil {
		return "[]"
	}
	b, _ := json.Marshal(rs)
	return string(b)
}

// ---- media items ----

// CreateMediaItem inserts the aggregate root and, for series, its seasons
// and episodes, in one transaction. Returns the new id, or ErrDuplicate if
// the (kind, tmdb id) pair already exists.
func (d *DB) CreateMediaItem(ctx context.Context, m domain.MediaItem) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)

	id, err := q.InsertMediaItem(ctx, insertParams(m, time.Now()))
	if err != nil {
		if isConstraint(err) {
			return 0, ErrDuplicate
		}
		return 0, err
	}
	for _, s := range m.Seasons {
		if _, err := q.InsertSeason(ctx, sqlitegen.InsertSeasonParams{
			MediaItemID: id, Number: int64(s.Number), Monitored: boolInt(s.Monitored),
		}); err != nil {
			return 0, err
		}
		for _, e := range s.Episodes {
			if _, err := q.InsertEpisode(ctx, sqlitegen.InsertEpisodeParams{
				MediaItemID:   id,
				SeasonNumber:  int64(e.SeasonNumber),
				EpisodeNumber: int64(e.EpisodeNumber),
				AbsoluteNum:   int64(e.AbsoluteNum),
				Title:         e.Title,
				AirDate:       e.AirDate,
				Monitored:     boolInt(e.Monitored),
			}); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// GetMediaItemFull loads an item with seasons, episodes, files, and links.
func (d *DB) GetMediaItemFull(ctx context.Context, id int64) (domain.MediaItem, error) {
	row, err := d.Read.GetMediaItem(ctx, id)
	if err != nil {
		return domain.MediaItem{}, wrapNotFound(err)
	}
	item := itemToDomain(row)

	if item.Kind == domain.KindSeries {
		seasons, err := d.Read.ListSeasons(ctx, id)
		if err != nil {
			return domain.MediaItem{}, err
		}
		episodes, err := d.Read.ListEpisodes(ctx, id)
		if err != nil {
			return domain.MediaItem{}, err
		}
		links, err := d.Read.ListFileEpisodeLinksForItem(ctx, sql.NullInt64{Int64: id, Valid: true})
		if err != nil {
			return domain.MediaItem{}, err
		}
		hasFile := map[int64]bool{}
		for _, l := range links {
			hasFile[l.EpisodeID] = true
		}
		bySeason := map[int64][]domain.Episode{}
		for _, e := range episodes {
			bySeason[e.SeasonNumber] = append(bySeason[e.SeasonNumber], domain.Episode{
				ID:            e.ID,
				SeasonNumber:  int(e.SeasonNumber),
				EpisodeNumber: int(e.EpisodeNumber),
				AbsoluteNum:   int(e.AbsoluteNum),
				Title:         e.Title,
				AirDate:       e.AirDate,
				Monitored:     e.Monitored != 0,
				HasFile:       hasFile[e.ID],
			})
		}
		for _, s := range seasons {
			item.Seasons = append(item.Seasons, domain.Season{
				ID:        s.ID,
				Number:    int(s.Number),
				Monitored: s.Monitored != 0,
				Episodes:  bySeason[s.Number],
			})
		}
		sort.Slice(item.Seasons, func(i, j int) bool { return item.Seasons[i].Number < item.Seasons[j].Number })
	}

	files, err := d.ListFilesForItem(ctx, id)
	if err != nil {
		return domain.MediaItem{}, err
	}
	item.Files = files
	copies, err := d.ListMediaCopies(ctx, id)
	if err != nil {
		return domain.MediaItem{}, err
	}
	item.Copies = copies
	return item, nil
}

// ---- media copies (multi-quality targets) ----

func copyFromRow(r sqlitegen.MediaCopy) domain.MediaCopy {
	c := domain.MediaCopy{
		ID: r.ID, MediaItemID: r.MediaItemID, Name: r.Name,
		QualityProfileID: r.QualityProfileID, Path: r.Path,
		Monitored: r.Monitored != 0, AddedAt: time.UnixMilli(r.AddedAt),
	}
	if r.RootFolderID.Valid {
		c.RootFolderID = r.RootFolderID.Int64
	}
	return c
}

// AddMediaCopy stores an additional quality target for an item.
func (d *DB) AddMediaCopy(ctx context.Context, c domain.MediaCopy) (int64, error) {
	p := sqlitegen.InsertMediaCopyParams{
		MediaItemID: c.MediaItemID, Name: c.Name,
		QualityProfileID: c.QualityProfileID, Path: c.Path,
		Monitored: boolInt(c.Monitored), AddedAt: time.Now().UnixMilli(),
	}
	if c.RootFolderID != 0 {
		p.RootFolderID = sql.NullInt64{Int64: c.RootFolderID, Valid: true}
	}
	return d.Write.InsertMediaCopy(ctx, p)
}

// ListMediaCopies returns an item's copies.
func (d *DB) ListMediaCopies(ctx context.Context, itemID int64) ([]domain.MediaCopy, error) {
	rows, err := d.Read.ListMediaCopies(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.MediaCopy, 0, len(rows))
	for _, r := range rows {
		out = append(out, copyFromRow(r))
	}
	return out, nil
}

// GetMediaCopy returns one copy of one item, or ErrNotFound.
func (d *DB) GetMediaCopy(ctx context.Context, itemID, copyID int64) (domain.MediaCopy, error) {
	r, err := d.Read.GetMediaCopy(ctx, sqlitegen.GetMediaCopyParams{ID: copyID, MediaItemID: itemID})
	if err != nil {
		return domain.MediaCopy{}, wrapNotFound(err)
	}
	return copyFromRow(r), nil
}

// UpdateMediaCopy stores name/profile/monitored edits for a copy.
func (d *DB) UpdateMediaCopy(ctx context.Context, c domain.MediaCopy) error {
	n, err := d.Write.UpdateMediaCopy(ctx, sqlitegen.UpdateMediaCopyParams{
		Name: c.Name, QualityProfileID: c.QualityProfileID,
		Monitored: boolInt(c.Monitored), ID: c.ID, MediaItemID: c.MediaItemID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMediaCopy removes a copy and its file RECORDS; disk is untouched.
func (d *DB) DeleteMediaCopy(ctx context.Context, itemID, copyID int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	if err := q.DeleteMediaFilesForCopy(ctx, sql.NullInt64{Int64: copyID, Valid: true}); err != nil {
		return err
	}
	n, err := q.DeleteMediaCopy(ctx, sqlitegen.DeleteMediaCopyParams{ID: copyID, MediaItemID: itemID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// GetMediaItemByKindTmdb returns the item id for (kind, tmdb) or ErrNotFound.
func (d *DB) GetMediaItemByKindTmdb(ctx context.Context, kind domain.MediaKind, tmdbID int64) (int64, error) {
	row, err := d.Read.GetMediaItemByKindTmdb(ctx, sqlitegen.GetMediaItemByKindTmdbParams{
		Kind: string(kind), TmdbID: tmdbID,
	})
	if err != nil {
		return 0, wrapNotFound(err)
	}
	return row.ID, nil
}

// GetMediaItemByKindTvdb returns the item id for (kind, tvdb) or ErrNotFound.
//
// A zero tvdb_id means "not known", not a real id, so it never matches —
// without that guard every series TMDB has no TVDB id for would collapse
// onto whichever one was added first (ADR 0011 §4).
func (d *DB) GetMediaItemByKindTvdb(ctx context.Context, kind domain.MediaKind, tvdbID int64) (int64, error) {
	if tvdbID == 0 {
		return 0, ErrNotFound
	}
	row, err := d.Read.GetMediaItemByKindTvdb(ctx, sqlitegen.GetMediaItemByKindTvdbParams{
		Kind: string(kind), TvdbID: tvdbID,
	})
	if err != nil {
		return 0, wrapNotFound(err)
	}
	return row.ID, nil
}

// GetMediaItemByKindOlid returns the item id for (kind, olid) or ErrNotFound.
func (d *DB) GetMediaItemByKindOlid(ctx context.Context, kind domain.MediaKind, olid string) (int64, error) {
	row, err := d.Read.GetMediaItemByKindOlid(ctx, sqlitegen.GetMediaItemByKindOlidParams{
		Kind: string(kind), Olid: olid,
	})
	if err != nil {
		return 0, wrapNotFound(err)
	}
	return row.ID, nil
}

// ListMediaItems returns summaries (no children), optionally filtered by
// kind, with completeness stats hydrated (EpisodeCount / EpisodeFileCount /
// FileCount).
func (d *DB) ListMediaItems(ctx context.Context, kind domain.MediaKind) ([]domain.MediaItem, error) {
	var rows []sqlitegen.MediaItem
	var err error
	if kind == "" {
		rows, err = d.Read.ListMediaItems(ctx)
	} else {
		rows, err = d.Read.ListMediaItemsByKind(ctx, string(kind))
	}
	if err != nil {
		return nil, err
	}
	stats, err := d.Read.ListMediaItemStats(ctx, time.Now().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]sqlitegen.ListMediaItemStatsRow, len(stats))
	for _, s := range stats {
		byID[s.ID] = s
	}
	out := make([]domain.MediaItem, 0, len(rows))
	for _, r := range rows {
		item := itemToDomain(r)
		if s, ok := byID[r.ID]; ok {
			item.EpisodeCount = int(s.AiredEpisodes)
			item.EpisodeFileCount = int(s.HaveEpisodes)
			item.FileCount = int(s.Files)
			item.Quality = weakestQuality(textOf(s.Qualities))
		}
		out = append(out, item)
	}
	return out, nil
}

// textOf reads a column sqlc typed as interface{} (which COALESCE makes it
// do). The driver hands TEXT back as []byte here, not string — asserting
// only for string compiles, runs, and silently reports every item as having
// no quality at all.
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

// weakestQuality picks the lowest-ranked quality from the comma-joined list
// the stats query returns.
//
// The weakest, not the best: it is the one that decides whether the item is
// still being hunted. A series with nine 1080p episodes and one 720p has not
// finished at 1080p, and a grid that says otherwise is the reason someone
// has to open every item to find out.
func weakestQuality(joined string) quality.Quality {
	var worst quality.Quality
	var have bool
	for _, raw := range strings.Split(joined, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		q := quality.FromString(raw)
		if !have || quality.Rank(q) < quality.Rank(worst) {
			worst, have = q, true
		}
	}
	return worst
}

// SetSeasonMonitored flips one season's monitored flag and cascades it to
// the season's episodes (an unmonitored season wants none of them).
// ErrNotFound when the item has no such season.
func (d *DB) SetSeasonMonitored(ctx context.Context, itemID int64, season int, monitored bool) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	n, err := q.SetSeasonMonitored(ctx, sqlitegen.SetSeasonMonitoredParams{
		Monitored: boolInt(monitored), MediaItemID: itemID, Number: int64(season),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := q.SetSeasonEpisodesMonitored(ctx, sqlitegen.SetSeasonEpisodesMonitoredParams{
		Monitored: boolInt(monitored), MediaItemID: itemID, SeasonNumber: int64(season),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetEpisodeMonitored flips one episode's monitored flag. ErrNotFound when
// the episode doesn't exist or belongs to another item.
func (d *DB) SetEpisodeMonitored(ctx context.Context, itemID, episodeID int64, monitored bool) error {
	n, err := d.Write.SetEpisodeMonitored(ctx, sqlitegen.SetEpisodeMonitoredParams{
		Monitored: boolInt(monitored), ID: episodeID, MediaItemID: itemID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateMediaItemPlacement stores a per-item edit: monitoring, quality
// profile, root folder, and folder path. Disk is never touched.
func (d *DB) UpdateMediaItemPlacement(ctx context.Context, m domain.MediaItem) error {
	p := sqlitegen.UpdateMediaItemPlacementParams{
		Monitored:        boolInt(m.Monitored),
		QualityProfileID: m.QualityProfileID,
		Path:             m.Path,
		UpdatedAt:        time.Now().UnixMilli(),
		ID:               m.ID,
	}
	if m.RootFolderID != 0 {
		p.RootFolderID = sql.NullInt64{Int64: m.RootFolderID, Valid: true}
	}
	return d.Write.UpdateMediaItemPlacement(ctx, p)
}

// UpdateMediaItemMetadata rewrites the provider-hydrated fields of an item
// (the metadata.refresh path). For series it also upserts seasons and
// episodes: new ones appear, existing ones keep their monitored flags, and
// nothing is ever deleted — files may point at episode rows.
func (d *DB) UpdateMediaItemMetadata(ctx context.Context, id int64, m domain.MediaItem) error {
	genres, _ := json.Marshal(m.Genres)
	if m.Genres == nil {
		genres = []byte("[]")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)

	if err := q.UpdateMediaItemMetadata(ctx, sqlitegen.UpdateMediaItemMetadataParams{
		Title:        m.Title,
		SortTitle:    m.SortTitle,
		Year:         int64(m.Year),
		Author:       m.Author,
		ImdbID:       m.IDs.IMDB,
		TvdbID:       m.IDs.TVDB,
		Isbn13:       m.IDs.ISBN13,
		Asin:         m.IDs.ASIN,
		Overview:     m.Overview,
		PosterPath:   m.PosterPath,
		BackdropPath: m.BackdropPath,
		Genres:       string(genres),
		Status:       m.Status,
		ReleaseDate:  m.ReleaseDate,
		Runtime:      int64(m.Runtime),
		Rating:       m.Rating,
		RatingVotes:  int64(m.RatingVotes),
		Ratings:      marshalRatings(m.Ratings),
		Ended:        boolInt(m.Ended),
		UpdatedAt:    time.Now().UnixMilli(),
		ID:           id,
	}); err != nil {
		return err
	}
	for _, s := range m.Seasons {
		if err := q.UpsertSeasonKeepFlags(ctx, sqlitegen.UpsertSeasonKeepFlagsParams{
			MediaItemID: id, Number: int64(s.Number), Monitored: boolInt(s.Monitored),
		}); err != nil {
			return err
		}
		for _, e := range s.Episodes {
			if err := q.UpsertEpisodeMeta(ctx, sqlitegen.UpsertEpisodeMetaParams{
				MediaItemID:   id,
				SeasonNumber:  int64(e.SeasonNumber),
				EpisodeNumber: int64(e.EpisodeNumber),
				AbsoluteNum:   int64(e.AbsoluteNum),
				Title:         e.Title,
				AirDate:       e.AirDate,
				Monitored:     boolInt(e.Monitored),
			}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// DeleteMediaItem removes the item; children cascade.
func (d *DB) DeleteMediaItem(ctx context.Context, id int64) error {
	return d.Write.DeleteMediaItem(ctx, id)
}

// GetEpisodeID resolves (item, season, episode) to an episode id.
func (d *DB) GetEpisodeID(ctx context.Context, itemID int64, season, episode int) (int64, error) {
	row, err := d.Read.GetEpisodeByNumber(ctx, sqlitegen.GetEpisodeByNumberParams{
		MediaItemID: itemID, SeasonNumber: int64(season), EpisodeNumber: int64(episode),
	})
	if err != nil {
		return 0, wrapNotFound(err)
	}
	return row.ID, nil
}

// ---- root folders ----

// AddRootFolder registers a library root; ErrDuplicate if the path exists.
func (d *DB) AddRootFolder(ctx context.Context, path string, kind domain.RootKind) (domain.RootFolder, error) {
	if kind == "" {
		kind = domain.KindMixed
	}
	id, err := d.Write.InsertRootFolder(ctx, sqlitegen.InsertRootFolderParams{
		Path: path, Kind: string(kind), AddedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		if isConstraint(err) {
			return domain.RootFolder{}, ErrDuplicate
		}
		return domain.RootFolder{}, err
	}
	return domain.RootFolder{ID: id, Path: path, Kind: kind, AddedAt: time.Now()}, nil
}

// SetRootFolderKind changes what a root is declared to hold. Retyping never
// touches the items already in it: an item's own kind is authoritative
// (ADR 0002), and the root's kind only routes future decisions.
func (d *DB) SetRootFolderKind(ctx context.Context, id int64, kind domain.RootKind) error {
	return d.Write.UpdateRootFolderKind(ctx, sqlitegen.UpdateRootFolderKindParams{
		Kind: string(kind), ID: id,
	})
}

// ListRootFolders returns all library roots.
func (d *DB) ListRootFolders(ctx context.Context) ([]domain.RootFolder, error) {
	rows, err := d.Read.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RootFolder, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.RootFolder{
			ID: r.ID, Path: r.Path, Kind: domain.RootKind(r.Kind), AddedAt: time.UnixMilli(r.AddedAt),
		})
	}
	return out, nil
}

// GetRootFolder returns one root folder or ErrNotFound.
func (d *DB) GetRootFolder(ctx context.Context, id int64) (domain.RootFolder, error) {
	r, err := d.Read.GetRootFolder(ctx, id)
	if err != nil {
		return domain.RootFolder{}, wrapNotFound(err)
	}
	return domain.RootFolder{
		ID: r.ID, Path: r.Path, Kind: domain.RootKind(r.Kind), AddedAt: time.UnixMilli(r.AddedAt),
	}, nil
}

// ---- ignored paths ----

// IgnorePath records that a directory is not media, so scans stop offering
// it (ADR 0009 §4). Idempotent: re-ignoring updates the reason.
func (d *DB) IgnorePath(ctx context.Context, path, reason string) error {
	return d.Write.InsertIgnoredPath(ctx, sqlitegen.InsertIgnoredPathParams{
		Path: path, Reason: reason, IgnoredAt: time.Now().UnixMilli(),
	})
}

// ListIgnoredPaths returns every dismissed path, oldest key order.
func (d *DB) ListIgnoredPaths(ctx context.Context) ([]domain.IgnoredPath, error) {
	rows, err := d.Read.ListIgnoredPaths(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.IgnoredPath, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.IgnoredPath{
			Path: r.Path, Reason: r.Reason, IgnoredAt: time.UnixMilli(r.IgnoredAt),
		})
	}
	return out, nil
}

// UnignorePath undoes a dismissal, so the path is offered again.
func (d *DB) UnignorePath(ctx context.Context, path string) error {
	return d.Write.DeleteIgnoredPath(ctx, path)
}

// DeleteRootFolder removes a root folder registration (never touches disk).
func (d *DB) DeleteRootFolder(ctx context.Context, id int64) error {
	return d.Write.DeleteRootFolder(ctx, id)
}

// ---- files ----

// UpsertFile records a file on disk. itemID 0 stores it as unmatched;
// copyID 0 attributes it to the primary. On path conflict the existing
// row's copy attribution is preserved (rescans must not stomp imports).
func (d *DB) UpsertFile(ctx context.Context, itemID, copyID int64, path string, size int64) (int64, error) {
	p := sqlitegen.UpsertMediaFileParams{
		Path: path, Size: size, AddedAt: time.Now().UnixMilli(),
	}
	if itemID != 0 {
		p.MediaItemID = sql.NullInt64{Int64: itemID, Valid: true}
	}
	if copyID != 0 {
		p.CopyID = sql.NullInt64{Int64: copyID, Valid: true}
	}
	return d.Write.UpsertMediaFile(ctx, p)
}

// ReplaceFileEpisodeLinks sets the exact episode set a file covers.
func (d *DB) ReplaceFileEpisodeLinks(ctx context.Context, fileID int64, episodeIDs []int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	if err := q.ClearFileEpisodeLinks(ctx, fileID); err != nil {
		return err
	}
	for _, epID := range episodeIDs {
		if err := q.LinkFileEpisode(ctx, sqlitegen.LinkFileEpisodeParams{
			MediaFileID: fileID, EpisodeID: epID,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListFilesForItem returns the item's files with their episode links.
func (d *DB) ListFilesForItem(ctx context.Context, itemID int64) ([]domain.MediaFile, error) {
	rows, err := d.Read.ListMediaFilesForItem(ctx, sql.NullInt64{Int64: itemID, Valid: true})
	if err != nil {
		return nil, err
	}
	links, err := d.Read.ListFileEpisodeLinksForItem(ctx, sql.NullInt64{Int64: itemID, Valid: true})
	if err != nil {
		return nil, err
	}
	byFile := map[int64][]int64{}
	for _, l := range links {
		byFile[l.MediaFileID] = append(byFile[l.MediaFileID], l.EpisodeID)
	}
	out := make([]domain.MediaFile, 0, len(rows))
	for _, r := range rows {
		f := domain.MediaFile{
			ID: r.ID, Path: r.Path, Size: r.Size,
			EpisodeIDs: byFile[r.ID], AddedAt: time.UnixMilli(r.AddedAt),
		}
		if r.MediaItemID.Valid {
			f.MediaItemID = r.MediaItemID.Int64
		}
		if r.CopyID.Valid {
			f.CopyID = r.CopyID.Int64
		}
		out = append(out, f)
	}
	return out, nil
}

// ListAllFiles returns every known file (matched or not).
func (d *DB) ListAllFiles(ctx context.Context) ([]domain.MediaFile, error) {
	rows, err := d.Read.ListAllMediaFiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.MediaFile, 0, len(rows))
	for _, r := range rows {
		f := domain.MediaFile{ID: r.ID, Path: r.Path, Size: r.Size, AddedAt: time.UnixMilli(r.AddedAt)}
		if r.MediaItemID.Valid {
			f.MediaItemID = r.MediaItemID.Int64
		}
		if r.CopyID.Valid {
			f.CopyID = r.CopyID.Int64
		}
		out = append(out, f)
	}
	return out, nil
}

// DeleteFile removes a file record (never touches disk).
func (d *DB) DeleteFile(ctx context.Context, id int64) error {
	return d.Write.DeleteMediaFile(ctx, id)
}
