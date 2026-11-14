package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

var ErrHouseholdVerification = errors.New("a current primary library check is required")

type HouseholdEvidence struct {
	Fingerprint                                          string
	ItemRevision, SharedRevision, RootID, RootGeneration int64
	RootPath, RootIdentity, Path                         string
	CheckedAt                                            time.Time
	PresentMovie                                         bool
	PresentEpisodes                                      map[int64]bool
}
type scopeQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readHouseholdEvidence(ctx context.Context, q scopeQuerier, id int64) (HouseholdEvidence, error) {
	v := HouseholdEvidence{PresentEpisodes: map[int64]bool{}}
	var kind domain.MediaKind
	var checked int64
	var valid, busy bool
	var ids string
	err := q.QueryRowContext(ctx, `SELECT m.kind,`+presenceFingerprintSQL+`,COALESCE((SELECT revision FROM lifecycle_item_revision WHERE media_item_id=m.id),0),s.revision,m.root_folder_id,rf.path,pin.identity,pin.generation,m.path,COALESCE(p.checked_at,0),COALESCE(p.present_file_ids,'[]'),COALESCE(p.valid=1 AND p.presence_fingerprint=`+presenceFingerprintSQL+` AND p.shared_revision=s.revision AND p.root_id=m.root_folder_id AND p.root_path=rf.path AND p.root_identity=pin.identity AND p.root_generation=pin.generation AND p.scope_path=m.path,0),
 EXISTS(SELECT 1 FROM rematch_operations o WHERE (o.source_item_id=m.id OR o.target_item_id=m.id) AND o.state NOT IN('complete','cancelled','failed')) OR EXISTS(SELECT 1 FROM offload_operations o WHERE o.media_item_id=m.id AND o.state NOT IN('complete','cancelled','failed')) OR EXISTS(SELECT 1 FROM replacement_operations o WHERE o.media_item_id=m.id AND o.state NOT IN('complete','cancelled','failed'))
 FROM media_items m JOIN root_folders rf ON rf.id=m.root_folder_id JOIN library_presence_roots pin ON pin.root_id=rf.id AND pin.root_path=rf.path LEFT JOIN library_presence_observations p ON p.media_item_id=m.id AND p.copy_id=0 CROSS JOIN lifecycle_shared_revision s WHERE m.id=? AND s.id=1`, id).Scan(&kind, &v.Fingerprint, &v.ItemRevision, &v.SharedRevision, &v.RootID, &v.RootPath, &v.RootIdentity, &v.RootGeneration, &v.Path, &checked, &ids, &valid, &busy)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrHouseholdVerification
	}
	if err != nil {
		return v, err
	}
	v.CheckedAt = time.UnixMilli(checked)
	now := time.Now()
	if !valid || busy || checked <= 0 || now.Before(v.CheckedAt) || now.Sub(v.CheckedAt) > 2*time.Minute {
		return v, ErrHouseholdVerification
	}
	var files []int64
	if err = json.Unmarshal([]byte(ids), &files); err != nil {
		return v, err
	}
	v.PresentMovie = len(files) > 0
	if kind == domain.KindSeries {
		var unallocated bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_files f JOIN json_each(?) x ON x.value=f.id WHERE f.media_item_id=? AND COALESCE(f.copy_id,0)=0 AND NOT EXISTS(SELECT 1 FROM media_file_episodes fe JOIN episodes e ON e.id=fe.episode_id WHERE fe.media_file_id=f.id AND e.media_item_id=f.media_item_id))`, ids, id).Scan(&unallocated); err != nil {
			return v, err
		}
		if unallocated {
			return v, fmt.Errorf("%w: allocate unidentified episode files before approving", ErrHouseholdVerification)
		}
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT e.id FROM episodes e JOIN media_file_episodes fe ON fe.episode_id=e.id JOIN media_files f ON f.id=fe.media_file_id JOIN json_each(?) x ON x.value=f.id WHERE e.media_item_id=? AND f.media_item_id=e.media_item_id AND COALESCE(f.copy_id,0)=0`, ids, id)
		if err != nil {
			return v, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var episode int64
			if err = rows.Scan(&episode); err != nil {
				return v, err
			}
			v.PresentEpisodes[episode] = true
		}
		if err = rows.Err(); err != nil {
			return v, err
		}
	}
	return v, nil
}
func (d *DB) HouseholdEvidence(ctx context.Context, id int64) (HouseholdEvidence, error) {
	tx, err := d.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return HouseholdEvidence{}, err
	}
	defer func() { _ = tx.Rollback() }()
	v, err := readHouseholdEvidence(ctx, tx, id)
	if err != nil {
		return v, err
	}
	return v, tx.Commit()
}
