package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

type HouseholdApproval struct {
	RequestID, RequestRevision int64
	Item                       domain.MediaItem
	New                        bool
	LibraryRevision            int64
	Evidence                   HouseholdEvidence
	Policy                     domain.HouseholdPolicy
	Missing                    []domain.RequestEpisode
	PrivateNote                string
}

func sameRequestEpisode(a, b domain.RequestEpisode) bool {
	return a.Season == b.Season && a.Episode == b.Episode
}
func (d *DB) ApproveHouseholdRequest(ctx context.Context, a domain.Account, digest string, plan HouseholdApproval) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, true); err != nil {
		return err
	}
	request, err := scanHouseholdRequest(tx.QueryRowContext(ctx, `SELECT `+householdRequestColumns+` FROM household_requests r WHERE id=?3`, a.ID, 1, plan.RequestID))
	if err != nil {
		return err
	}
	if request.Revision != plan.RequestRevision || (request.State != "pending" && request.State != "needs_review") || request.SubscriberCount == 0 {
		return ErrRequestChanged
	}
	if busy, e := householdRequestHasWork(ctx, tx, request.ID); e != nil {
		return e
	} else if busy {
		return ErrRequestChanged
	}
	policy := plan.Policy
	hash, e := householdProfileFingerprint(ctx, tx, policy.ProfileID)
	if e != nil {
		return e
	}
	if hash != policy.ProfileFingerprint {
		return ErrRequestChanged
	}
	var rootPath, rootIdentity string
	var generation, shared int64
	if err = tx.QueryRowContext(ctx, `SELECT r.path,p.identity,p.generation,s.revision FROM root_folders r JOIN library_presence_roots p ON p.root_id=r.id AND p.root_path=r.path CROSS JOIN lifecycle_shared_revision s WHERE r.id=? AND s.id=1`, policy.RootID).Scan(&rootPath, &rootIdentity, &generation, &shared); err != nil {
		return err
	}
	if rootPath != policy.RootPath || rootIdentity != policy.RootIdentity || generation != policy.RootGeneration || shared != policy.SharedRevision {
		return ErrRequestChanged
	}
	item := plan.Item
	if !plan.New {
		var profile, root int64
		var path string
		if err = tx.QueryRowContext(ctx, `SELECT quality_profile_id,COALESCE(root_folder_id,0),path FROM media_items WHERE id=?`, item.ID).Scan(&profile, &root, &path); err != nil {
			return err
		}
		if profile != policy.ProfileID || root != policy.RootID || path != policy.Path {
			return ErrRequestChanged
		}
	}
	if item.Kind != request.Kind || item.IDs.TMDB != request.TMDBID || domain.ExternalIDsConflict(item.IDs, request.IDs) {
		return ErrRequestChanged
	}
	if plan.New {
		var revision int64
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM lifecycle_library_revision WHERE id=1`).Scan(&revision); err != nil {
			return err
		}
		if revision != plan.LibraryRevision || item.ID != 0 || item.Monitored || item.RootFolderID != policy.RootID || item.QualityProfileID != policy.ProfileID || item.Path != policy.Path {
			return ErrRequestChanged
		}
		item.ID, err = d.createMediaItemTx(ctx, tx, item)
		if err != nil {
			return err
		}
	} else {
		evidence, e := readHouseholdEvidence(ctx, tx, item.ID)
		if e != nil {
			return e
		}
		if evidence.Fingerprint != plan.Evidence.Fingerprint || evidence.ItemRevision != plan.Evidence.ItemRevision || evidence.SharedRevision != plan.Evidence.SharedRevision {
			return ErrRequestChanged
		}
		if collision := checkCreateIdentity(ctx, d.Write.WithTx(tx), item); !errors.Is(collision, ErrDuplicate) {
			return ErrRequestChanged
		}
	}
	policy.ItemID = item.ID
	type leaf struct {
		coordinate         domain.RequestEpisode
		id                 int64
		missing, offloaded bool
		offloadEpoch       int64
	}
	leaves := []leaf{}
	scope := request.Scope.Episodes
	if request.Kind == domain.KindMovie {
		scope = []domain.RequestEpisode{{}}
	}
	for _, coordinate := range scope {
		current := leaf{coordinate: coordinate}
		if request.Kind == domain.KindSeries {
			var air string
			var offloaded int64
			if err = tx.QueryRowContext(ctx, `SELECT id,air_date,offloaded_at FROM episodes WHERE media_item_id=? AND season_number=? AND episode_number=?`, item.ID, coordinate.Season, coordinate.Episode).Scan(&current.id, &air, &offloaded); err != nil {
				return err
			}
			date, e := time.Parse("2006-01-02", air)
			if e != nil || date.After(time.Now().UTC()) || coordinate.Season <= 0 || coordinate.Episode <= 0 {
				return ErrRequestChanged
			}
			current.offloaded = offloaded > 0
			current.offloadEpoch = offloaded
		} else {
			var offloaded int64
			if err = tx.QueryRowContext(ctx, `SELECT offloaded_at FROM media_items WHERE id=?`, item.ID).Scan(&offloaded); err != nil {
				return err
			}
			current.offloaded = offloaded > 0
			current.offloadEpoch = offloaded
		}
		for _, missing := range plan.Missing {
			if sameRequestEpisode(missing, coordinate) {
				current.missing = true
				break
			}
		}
		if !plan.New {
			present := plan.Evidence.PresentMovie
			if request.Kind == domain.KindSeries {
				present = plan.Evidence.PresentEpisodes[current.id]
			}
			if current.missing == present {
				return ErrRequestChanged
			}
		}
		if plan.New && !current.missing {
			return ErrRequestChanged
		}
		if current.missing && current.offloaded && !policy.Reacquire {
			return ErrRequestChanged
		}
		if current.missing {
			var claimed int64
			err = tx.QueryRowContext(ctx, `SELECT request_id FROM household_scope_claims WHERE kind=? AND tmdb_id=? AND copy_id=0 AND season=? AND episode=?`, string(request.Kind), request.TMDBID, coordinate.Season, coordinate.Episode).Scan(&claimed)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil && claimed != request.ID {
				return ErrRequestOverlap
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO household_scope_claims(kind,tmdb_id,copy_id,season,episode,request_id) VALUES(?,?,0,?,?,?) ON CONFLICT DO NOTHING`, string(request.Kind), request.TMDBID, coordinate.Season, coordinate.Episode, request.ID); err != nil {
				return err
			}
		}
		if !current.missing {
			if _, err = tx.ExecContext(ctx, `DELETE FROM household_scope_claims WHERE request_id=? AND kind=? AND tmdb_id=? AND copy_id=0 AND season=? AND episode=?`, request.ID, string(request.Kind), request.TMDBID, coordinate.Season, coordinate.Episode); err != nil {
				return err
			}
		}
		leaves = append(leaves, current)
	}
	// Keep offload flags until verified primary bytes return. Approval cannot
	// re-enable ordinary monitoring or additional-copy acquisition.
	now := time.Now().UnixMilli()
	missing := 0
	for _, leaf := range leaves {
		state := "fulfilled"
		if leaf.missing {
			state = "approved"
			missing++
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO household_request_targets(request_id,media_item_id,episode_id,season,episode,state,created_at,offload_epoch) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(request_id,season,episode) DO UPDATE SET media_item_id=excluded.media_item_id,episode_id=excluded.episode_id,state=excluded.state,approval_generation=household_request_targets.approval_generation+1,download_id=NULL,download_generation='',next_check_at=0,offload_epoch=excluded.offload_epoch`, request.ID, item.ID, leaf.id, leaf.coordinate.Season, leaf.coordinate.Episode, state, now, leaf.offloadEpoch)
		if err != nil {
			return err
		}
	}
	state := "approved"
	if missing == 0 {
		state = "fulfilled"
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_requests SET state=?,approved_by=?,policy_json=?,library_item_id=?,revision=revision+1,updated_at=?,public_note='' WHERE id=?`, state, a.ID, string(data), item.ID, now, request.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_request_notes(request_id,account_id,note) VALUES(?,?,?) ON CONFLICT(request_id,account_id) DO UPDATE SET note=excluded.note`, request.ID, a.ID, plan.PrivateNote); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_request_decisions(request_id,revision,account_id,decision,policy_json,created_at) SELECT id,revision,?,'approved',policy_json,? FROM household_requests WHERE id=?`, a.ID, now, request.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) HouseholdPendingTargets(ctx context.Context) ([]domain.HouseholdTarget, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT t.id,t.request_id,t.media_item_id,t.episode_id,t.season,t.episode,t.state,t.approval_generation,COALESCE(t.download_id,0),t.download_generation,t.offload_epoch FROM household_request_targets t JOIN household_requests r ON r.id=t.request_id WHERE (r.state IN('approved','acquiring') OR r.state='needs_review' AND t.state='admitted') AND t.state IN('approved','admitted') AND t.next_check_at<=? ORDER BY t.next_check_at,t.id LIMIT 100`, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.HouseholdTarget{}
	for rows.Next() {
		var v domain.HouseholdTarget
		if err = rows.Scan(&v.ID, &v.RequestID, &v.MediaItemID, &v.EpisodeID, &v.Season, &v.Episode, &v.State, &v.ApprovalGeneration, &v.DownloadID, &v.DownloadGeneration, &v.OffloadEpoch); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (d *DB) HouseholdTarget(ctx context.Context, id int64) (domain.HouseholdTarget, error) {
	var v domain.HouseholdTarget
	err := d.R.QueryRowContext(ctx, `SELECT id,request_id,media_item_id,episode_id,season,episode,state,approval_generation,COALESCE(download_id,0),download_generation,offload_epoch FROM household_request_targets WHERE id=?`, id).Scan(&v.ID, &v.RequestID, &v.MediaItemID, &v.EpisodeID, &v.Season, &v.Episode, &v.State, &v.ApprovalGeneration, &v.DownloadID, &v.DownloadGeneration, &v.OffloadEpoch)
	if err == sql.ErrNoRows {
		return v, ErrNotFound
	}
	return v, err
}

// Reserve a fair retry position before any potentially blocking external work.
// A crash or per-job timeout leaves this durable deadline intact.
func (d *DB) ScheduleHouseholdTargetRetry(ctx context.Context, id, generation int64, at time.Time) error {
	result, err := d.W.ExecContext(ctx, `UPDATE household_request_targets SET next_check_at=? WHERE id=? AND approval_generation=? AND state IN('approved','admitted')`, at.UnixMilli(), id, generation)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRequestChanged
	}
	return nil
}
