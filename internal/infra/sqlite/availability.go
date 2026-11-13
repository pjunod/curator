package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

const presenceFreshFor = 13 * time.Hour

type availabilityRecord struct {
	id                int64
	kind              domain.MediaKind
	ids               domain.ExternalIDs
	offloaded         int64
	recorded, present int
	unallocated       int
	checked           int64
	proof, busy       bool
	active            bool
	seasons           []domain.SeasonAvailability
}

// LookupAvailability reads one consistent local snapshot. Provider results and
// observations are never cached together; no filesystem operation occurs here.
func (d *DB) LookupAvailability(ctx context.Context, targets []domain.AvailabilityTarget) ([]domain.Availability, error) {
	if len(targets) > 100 {
		return nil, fmt.Errorf("at most 100 availability targets")
	}
	out := make([]domain.Availability, len(targets))
	clauses := []string{}
	args := []any{}
	for _, t := range targets {
		if !domain.ValidKind(t.Kind) {
			continue
		}
		ids := []string{}
		if t.LibraryItemID > 0 {
			ids = append(ids, "m.id=?")
		}
		vals := []any{}
		if t.LibraryItemID > 0 {
			vals = append(vals, t.LibraryItemID)
		}
		if t.IDs.TMDB > 0 {
			ids = append(ids, "m.tmdb_id=?")
			vals = append(vals, t.IDs.TMDB)
		}
		if t.IDs.TVDB > 0 {
			ids = append(ids, "m.tvdb_id=?")
			vals = append(vals, t.IDs.TVDB)
		}
		for _, v := range []struct{ col, value string }{{"imdb_id", t.IDs.IMDB}, {"olid", t.IDs.OLID}, {"isbn13", t.IDs.ISBN13}, {"asin", t.IDs.ASIN}} {
			if v.value != "" {
				ids = append(ids, "lower(m."+v.col+")=lower(?)")
				vals = append(vals, v.value)
			}
		}
		if len(ids) > 0 {
			clauses = append(clauses, "(m.kind=? AND ("+strings.Join(ids, " OR ")+"))")
			args = append(args, string(t.Kind))
			args = append(args, vals...)
		}
	}
	if len(clauses) == 0 {
		for i := range out {
			out[i] = domain.Availability{Catalog: "unknown", Presence: "unknown", Acquisition: "unknown", Reason: "identity_unavailable"}
		}
		return out, nil
	}
	tx, err := d.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.kind,m.tmdb_id,m.tvdb_id,m.imdb_id,m.olid,m.isbn13,m.asin,m.offloaded_at,
 (SELECT COUNT(*) FROM media_files f WHERE f.media_item_id=m.id AND COALESCE(f.copy_id,0)=0),
 (SELECT COUNT(*) FROM media_files f JOIN json_each(COALESCE(p.present_file_ids,'[]')) x ON x.value=f.id WHERE f.media_item_id=m.id AND COALESCE(f.copy_id,0)=0),
 (SELECT COUNT(*) FROM media_files f JOIN json_each(COALESCE(p.present_file_ids,'[]')) x ON x.value=f.id WHERE f.media_item_id=m.id AND COALESCE(f.copy_id,0)=0 AND NOT EXISTS(SELECT 1 FROM media_file_episodes fe JOIN episodes e ON e.id=fe.episode_id WHERE fe.media_file_id=f.id AND e.media_item_id=m.id)),
 COALESCE(p.checked_at,0),COALESCE(p.valid=1 AND p.presence_fingerprint=`+presenceFingerprintSQL+` AND p.shared_revision=sr.revision AND p.scope_path=m.path AND p.root_id=m.root_folder_id AND p.root_path=rf.path AND pr.identity=p.root_identity AND p.root_generation=pr.generation,0),
 EXISTS(SELECT 1 FROM offload_operations o WHERE o.media_item_id=m.id AND o.state NOT IN ('complete','cancelled','failed')) OR
 EXISTS(SELECT 1 FROM rematch_operations o WHERE (o.source_item_id=m.id OR o.target_item_id=m.id) AND o.state NOT IN ('complete','cancelled','failed')) OR
 EXISTS(SELECT 1 FROM replacement_operations o WHERE o.media_item_id=m.id AND o.state NOT IN ('complete','cancelled','failed')),
 EXISTS(SELECT 1 FROM downloads d WHERE d.media_item_id=m.id AND COALESCE(d.copy_id,0)=0 AND d.superseded=0 AND d.state IN ('planned','submitting','grabbed','downloading','held','downloaded','awaiting_import','importing') AND NOT EXISTS(SELECT 1 FROM import_attempt_controls c WHERE c.download_id=d.id AND c.generation_key=d.generation_key AND c.outcome='cancelled'))
 FROM media_items m
 LEFT JOIN library_presence_observations p ON p.media_item_id=m.id AND p.copy_id=0
 LEFT JOIN lifecycle_shared_revision sr ON sr.id=1
 LEFT JOIN root_folders rf ON rf.id=m.root_folder_id
 LEFT JOIN library_presence_roots pr ON pr.root_id=p.root_id AND pr.root_path=p.root_path
 WHERE `+strings.Join(clauses, " OR "), args...)
	if err != nil {
		return nil, err
	}
	records := []availabilityRecord{}
	byID := map[int64]int{}
	for rows.Next() {
		var r availabilityRecord
		if err = rows.Scan(&r.id, &r.kind, &r.ids.TMDB, &r.ids.TVDB, &r.ids.IMDB, &r.ids.OLID, &r.ids.ISBN13, &r.ids.ASIN, &r.offloaded, &r.recorded, &r.present, &r.unallocated, &r.checked, &r.proof, &r.busy, &r.active); err != nil {
			_ = rows.Close()
			return nil, err
		}
		byID[r.id] = len(records)
		records = append(records, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(records) > 0 {
		marks := []string{}
		seasonArgs := []any{time.Now().UTC().Format("2006-01-02")}
		for _, r := range records {
			marks = append(marks, "?")
			seasonArgs = append(seasonArgs, r.id)
		}
		seasons, err := tx.QueryContext(ctx, `SELECT e.media_item_id,e.season_number,
 SUM(CASE WHEN date(e.air_date) IS NOT NULL AND e.air_date<=?1 THEN 1 ELSE 0 END),
 COUNT(DISTINCT CASE WHEN date(e.air_date) IS NOT NULL AND e.air_date<=?1 AND EXISTS(SELECT 1 FROM media_file_episodes fe JOIN media_files f ON f.id=fe.media_file_id JOIN json_each(COALESCE(p.present_file_ids,'[]')) x ON x.value=f.id WHERE fe.episode_id=e.id AND f.media_item_id=e.media_item_id AND COALESCE(f.copy_id,0)=0) THEN e.id END),
 SUM(CASE WHEN date(e.air_date) IS NOT NULL AND e.air_date<=?1 AND e.offloaded_at>0 THEN 1 ELSE 0 END),
 SUM(CASE WHEN date(e.air_date) IS NULL THEN 1 ELSE 0 END)
 FROM episodes e LEFT JOIN library_presence_observations p ON p.media_item_id=e.media_item_id AND p.copy_id=0
 WHERE e.season_number>0 AND e.media_item_id IN (`+strings.Join(marks, ",")+`) GROUP BY e.media_item_id,e.season_number ORDER BY e.media_item_id,e.season_number`, seasonArgs...)
		if err != nil {
			return nil, err
		}
		for seasons.Next() {
			var id int64
			var season domain.SeasonAvailability
			if err = seasons.Scan(&id, &season.Number, &season.Expected, &season.Present, &season.Offloaded, &season.UnknownDates); err != nil {
				_ = seasons.Close()
				return nil, err
			}
			i := byID[id]
			records[i].seasons = append(records[i].seasons, season)
		}
		err = seasons.Err()
		_ = seasons.Close()
		if err != nil {
			return nil, err
		}
	}
	now := time.Now()
	for i, t := range targets {
		if !domain.ValidKind(t.Kind) {
			out[i] = domain.Availability{Catalog: "unknown", Presence: "unknown", Acquisition: "unknown", Reason: "identity_unavailable"}
			continue
		}
		matches := []availabilityRecord{}
		conflict := false
		for _, r := range records {
			if r.kind == t.Kind && (availabilityIDOverlap(t.IDs, r.ids) || t.LibraryItemID > 0 && t.LibraryItemID == r.id) {
				matches = append(matches, r)
				conflict = conflict || availabilityIDsConflict(t.IDs, r.ids) || t.LibraryItemID > 0 && t.LibraryItemID != r.id
			}
		}
		if len(matches) > 1 || conflict {
			out[i] = domain.Availability{Catalog: "ambiguous", Presence: "unknown", Acquisition: "unknown", Reason: "identity_conflict"}
			continue
		}
		if len(matches) == 0 {
			catalog := "absent"
			if !availabilityHasID(t.IDs) || t.LibraryItemID > 0 {
				catalog = "unknown"
			}
			out[i] = domain.Availability{Catalog: catalog, Presence: "unknown", Acquisition: "none", Reason: "not_in_curator"}
			continue
		}
		r := matches[0]
		a := domain.Availability{Catalog: "present", Presence: "unknown", LibraryItemID: r.id, Acquisition: "none", Reason: "unverified", Seasons: r.seasons}
		if r.active {
			a.Acquisition = "active"
		}
		if r.checked > 0 {
			v := time.UnixMilli(r.checked)
			a.CheckedAt = &v
		}
		fresh := r.proof && r.checked > 0 && !now.Before(time.UnixMilli(r.checked)) && now.Sub(time.UnixMilli(r.checked)) <= presenceFreshFor && !r.busy
		unknownDates := 0
		if r.kind == domain.KindSeries {
			for _, sn := range r.seasons {
				a.Expected += sn.Expected
				a.Present += sn.Present
				a.Offloaded += sn.Offloaded
				unknownDates += sn.UnknownDates
			}
		} else {
			a.Expected = 1
			if r.present > 0 {
				a.Present = 1
			}
			if r.offloaded > 0 {
				a.Offloaded = 1
			}
		}
		switch {
		case r.busy:
			a.Reason = "library_operation_in_progress"
		case !fresh:
			a.Reason = "verification_required"
		case r.kind == domain.KindSeries && r.unallocated > 0:
			a.Reason = "unallocated_episode_files"
		case a.Expected == 0 || unknownDates > 0:
			a.Reason = "episode_scope_incomplete"
		case a.Present == a.Expected:
			a.Presence = "present"
			a.Reason = ""
		case a.Present > 0:
			a.Presence = "partial"
			a.Reason = ""
		case a.Offloaded == a.Expected:
			a.Presence = "offloaded"
			a.Reason = ""
		default:
			a.Presence = "missing"
			a.Reason = ""
		}
		if !fresh {
			a.Present = 0
			for j := range a.Seasons {
				a.Seasons[j].Present = 0
			}
		}
		out[i] = a
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func availabilityHasID(id domain.ExternalIDs) bool {
	return id.TMDB > 0 || id.TVDB > 0 || id.IMDB != "" || id.OLID != "" || id.ISBN13 != "" || id.ASIN != ""
}
func availabilityIDOverlap(a, b domain.ExternalIDs) bool {
	return a.TMDB > 0 && a.TMDB == b.TMDB || a.TVDB > 0 && a.TVDB == b.TVDB || a.IMDB != "" && strings.EqualFold(a.IMDB, b.IMDB) || a.OLID != "" && strings.EqualFold(a.OLID, b.OLID) || a.ISBN13 != "" && a.ISBN13 == b.ISBN13 || a.ASIN != "" && strings.EqualFold(a.ASIN, b.ASIN)
}

func availabilityIDsConflict(a, b domain.ExternalIDs) bool {
	if domain.ExternalIDsConflict(a, b) {
		return true
	}
	for _, pair := range [][2]string{{a.OLID, b.OLID}, {a.ISBN13, b.ISBN13}, {a.ASIN, b.ASIN}} {
		if pair[0] != "" && pair[1] != "" && !strings.EqualFold(pair[0], pair[1]) {
			return true
		}
	}
	return false
}
