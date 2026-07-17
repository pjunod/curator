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
	item := domain.MediaItem{
		ID:        r.ID,
		Kind:      domain.MediaKind(r.Kind),
		Title:     r.Title,
		SortTitle: r.SortTitle,
		Year:      int(r.Year),
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
	p := sqlitegen.InsertMediaItemParams{
		Kind:         string(m.Kind),
		Title:        m.Title,
		SortTitle:    m.SortTitle,
		Year:         int64(m.Year),
		TmdbID:       m.IDs.TMDB,
		ImdbID:       m.IDs.IMDB,
		TvdbID:       m.IDs.TVDB,
		Isbn13:       m.IDs.ISBN13,
		Olid:         m.IDs.OLID,
		Asin:         m.IDs.ASIN,
		Overview:     m.Overview,
		PosterPath:   m.PosterPath,
		BackdropPath: m.BackdropPath,
		Genres:       string(genres),
		Status:       m.Status,
		ReleaseDate:  m.ReleaseDate,
		Runtime:      int64(m.Runtime),
		Monitored:    boolInt(m.Monitored),
		Path:         m.Path,
		Ended:        boolInt(m.Ended),
		AddedAt:      now.UnixMilli(),
		UpdatedAt:    now.UnixMilli(),
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
	return item, nil
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

// ListMediaItems returns summaries (no children), optionally filtered by kind.
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
	out := make([]domain.MediaItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, itemToDomain(r))
	}
	return out, nil
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
func (d *DB) AddRootFolder(ctx context.Context, path string) (domain.RootFolder, error) {
	id, err := d.Write.InsertRootFolder(ctx, sqlitegen.InsertRootFolderParams{
		Path: path, AddedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		if isConstraint(err) {
			return domain.RootFolder{}, ErrDuplicate
		}
		return domain.RootFolder{}, err
	}
	return domain.RootFolder{ID: id, Path: path, AddedAt: time.Now()}, nil
}

// ListRootFolders returns all library roots.
func (d *DB) ListRootFolders(ctx context.Context) ([]domain.RootFolder, error) {
	rows, err := d.Read.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RootFolder, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.RootFolder{ID: r.ID, Path: r.Path, AddedAt: time.UnixMilli(r.AddedAt)})
	}
	return out, nil
}

// GetRootFolder returns one root folder or ErrNotFound.
func (d *DB) GetRootFolder(ctx context.Context, id int64) (domain.RootFolder, error) {
	r, err := d.Read.GetRootFolder(ctx, id)
	if err != nil {
		return domain.RootFolder{}, wrapNotFound(err)
	}
	return domain.RootFolder{ID: r.ID, Path: r.Path, AddedAt: time.UnixMilli(r.AddedAt)}, nil
}

// DeleteRootFolder removes a root folder registration (never touches disk).
func (d *DB) DeleteRootFolder(ctx context.Context, id int64) error {
	return d.Write.DeleteRootFolder(ctx, id)
}

// ---- files ----

// UpsertFile records a file on disk. itemID 0 stores it as unmatched.
func (d *DB) UpsertFile(ctx context.Context, itemID int64, path string, size int64) (int64, error) {
	p := sqlitegen.UpsertMediaFileParams{
		Path: path, Size: size, AddedAt: time.Now().UnixMilli(),
	}
	if itemID != 0 {
		p.MediaItemID = sql.NullInt64{Int64: itemID, Valid: true}
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
		out = append(out, f)
	}
	return out, nil
}

// DeleteFile removes a file record (never touches disk).
func (d *DB) DeleteFile(ctx context.Context, id int64) error {
	return d.Write.DeleteMediaFile(ctx, id)
}
