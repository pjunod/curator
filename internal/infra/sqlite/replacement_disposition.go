package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrParentDispositionRequested = errors.New("originating attempt was deleted before replacement admission")

// Claim and Delete use the same SQLite writer transaction. An unacknowledged
// child belongs to the parent's pending obligations, including a remote Add
// whose response was lost. A child admitted first keeps its separate identity.
func bindReplacementAdmission(ctx context.Context, tx *sql.Tx, parentID int64, parentGen string, childID int64, childGen string) error {
	var revoked bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM download_dispositions WHERE download_id=? AND generation_key=?)`, parentID, parentGen).Scan(&revoked); e != nil {
		return e
	}
	if revoked {
		return ErrParentDispositionRequested
	}
	var matches bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM downloads p,downloads c WHERE p.id=? AND p.generation_key=? AND c.id=? AND c.generation_key=?)`, parentID, parentGen, childID, childGen).Scan(&matches); e != nil {
		return e
	}
	if !matches {
		return ErrParentDispositionRequested
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO replacement_admissions(parent_id,parent_generation,child_id,child_generation) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, parentID, parentGen, childID, childGen); e != nil {
		return e
	}
	if e := tx.QueryRowContext(ctx, `SELECT parent_id=? AND parent_generation=? AND child_generation=? FROM replacement_admissions WHERE child_id=? AND child_generation=?`, parentID, parentGen, childGen, childID, childGen).Scan(&matches); e != nil {
		return e
	}
	if !matches {
		return fmt.Errorf("replacement admission identity changed")
	}
	return nil
}
func (d *DB) BindReplacementAdmission(ctx context.Context, parentID int64, parentGen string, childID int64, childGen string) error {
	tx, e := d.W.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if e = bindReplacementAdmission(ctx, tx, parentID, parentGen, childID, childGen); e != nil {
		return e
	}
	return tx.Commit()
}
func (d *DB) CompleteReplacementAdmission(ctx context.Context, childID int64, childGen string) error {
	_, e := d.W.ExecContext(ctx, `UPDATE replacement_admissions SET phase=CASE WHEN EXISTS(SELECT 1 FROM download_dispositions x WHERE x.download_id=parent_id AND x.generation_key=parent_generation) THEN 'cancel_pending' ELSE 'admitted' END WHERE child_id=? AND child_generation=? AND phase='admitting'`, childID, childGen)
	return e
}

func (d *DB) RecordReplacementOrigin(ctx context.Context, parentID int64, parentGen string, childID int64, childGen string) error {
	if e := d.BindReplacementAdmission(ctx, parentID, parentGen, childID, childGen); e != nil {
		return e
	}
	_, e := d.W.ExecContext(ctx, `UPDATE downloads SET handoff_log=json_insert(handoff_log,'$[#]',json_object('step','replacement_linked','at',?,'detail',?)) WHERE id=? AND generation_key=? AND NOT EXISTS(SELECT 1 FROM json_each(handoff_log) h WHERE json_extract(h.value,'$.step')='replacement_linked')`, time.Now().UnixMilli(), fmt.Sprintf("Replacement for acquisition attempt %d", parentID), childID, childGen)
	return e
}
