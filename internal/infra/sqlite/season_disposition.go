package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SeasonReplacementOrigin identifies the exact failed attempt that requested
// a season comparison. Independent season searches have no origin.
type SeasonReplacementOrigin struct {
	DownloadID    int64  `json:"downloadId"`
	GenerationKey string `json:"generationKey"`
}

func (d *DB) CheckSeasonReplacementOrigin(ctx context.Context, origin *SeasonReplacementOrigin) error {
	return checkSeasonReplacementOrigin(ctx, d.R, origin)
}

func checkSeasonReplacementOrigin(ctx context.Context, q querier, origin *SeasonReplacementOrigin) error {
	if origin == nil {
		return nil
	}
	if origin.DownloadID <= 0 || origin.GenerationKey == "" {
		return ErrParentDispositionRequested
	}
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM downloads d WHERE id=? AND generation_key=? AND NOT EXISTS (
			SELECT 1 FROM download_dispositions x WHERE x.download_id=d.id AND x.generation_key=d.generation_key
		)
	)`, origin.DownloadID, origin.GenerationKey).Scan(&allowed)
	if err == nil && !allowed {
		return ErrParentDispositionRequested
	}
	return err
}

// claimReplacementPlanSubmission joins the admission claim and its plan's
// origin binding. If admission wins, Delete sees the child in flight. If
// Delete wins, the pending row is retired without a client call.
func claimReplacementPlanSubmission(ctx context.Context, tx *sql.Tx, id int64, now int64) (bool, error) {
	var childGeneration string
	var origin SeasonReplacementOrigin
	err := tx.QueryRowContext(ctx, `SELECT d.generation_key,p.replacement_origin_id,p.replacement_origin_generation
		FROM downloads d JOIN acquisition_plans p ON p.id=d.plan_id
		WHERE d.id=? AND d.state='planned' AND d.submission_phase='pending' AND d.superseded=0
		AND p.state IN ('admitted','dispatching','active')`, id).Scan(&childGeneration, &origin.DownloadID, &origin.GenerationKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if origin.DownloadID == 0 {
		return true, nil
	}
	err = bindReplacementAdmission(ctx, tx, origin.DownloadID, origin.GenerationKey, id, childGeneration)
	if !errors.Is(err, ErrParentDispositionRequested) {
		return err == nil, err
	}
	// The exact origin might have expired from ordinary history. Handle
	// this as well as explicit deletion at the same final admission gate.
	_, err = tx.ExecContext(ctx, `UPDATE downloads SET state='failed',submission_phase='rejected',
		superseded=1,reserved_episodes='[]',wantables='[]',error='replacement origin no longer authorizes admission',updated_at=?
		WHERE id=? AND generation_key=? AND state='planned' AND submission_phase='pending'`,
		now, id, childGeneration)
	return false, err
}

func (d *DB) CloseRevokedSeasonPlanIntents(ctx context.Context, planID int64) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads AS child SET
		state='failed',submission_phase='rejected',superseded=1,reserved_episodes='[]',wantables='[]',
		error='replacement origin no longer authorizes admission',updated_at=?
		WHERE child.plan_id=? AND child.state='planned' AND child.submission_phase='pending' AND EXISTS (
			SELECT 1 FROM acquisition_plans p WHERE p.id=child.plan_id AND p.replacement_origin_id>0 AND NOT EXISTS (
				SELECT 1 FROM downloads origin WHERE origin.id=p.replacement_origin_id AND origin.generation_key=p.replacement_origin_generation
				AND NOT EXISTS (SELECT 1 FROM download_dispositions x WHERE x.download_id=origin.id AND x.generation_key=origin.generation_key)
			)
		)`, time.Now().UnixMilli(), planID)
	return err
}
