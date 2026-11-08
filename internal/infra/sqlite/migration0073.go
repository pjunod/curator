package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// dropV1Version is migration 0073_drop_v1.sql.
const dropV1Version = 73

// ErrLiveV1Dependents refuses migration 0073 while a row it would drop is
// still somebody's pending work.
var ErrLiveV1Dependents = errors.New("migration 0073 refused: live v1 dependents")

// preflightDropV1 is the count check plan M4.3 asks for before the v1 tables
// go. The one live dependent a drop could lose is an operator's explicit
// deletion request that has not finished: refuse, name it, and let the
// operator finish or cancel it on the previous build. Everything else 0073
// drops is either migrated (the deliveries cursor, the storage-change path
// fences, the 0040 cohort folded into historical publications) or has had no
// reader since M2 (admission-poll pacing).
func (d *DB) preflightDropV1(ctx context.Context) error {
	var version int64
	if err := d.W.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied=1`).Scan(&version); err != nil {
		return nil // a fresh database has no goose table yet: nothing to check
	}
	if version >= dropV1Version || version < 61 {
		return nil // already dropped, or older than the batches table (0061)
	}
	query := `SELECT DISTINCT b.batch_id FROM runner_delivery_explicit_delete_batches b
		JOIN runner_delivery_explicit_delete_files f ON f.batch_id=b.batch_id WHERE f.history_recorded=0`
	if version >= 65 { // 0065 added cancellation
		query += ` AND b.abandoned_at=0`
	}
	return d.preflightBatches(ctx, query)
}

func (d *DB) preflightBatches(ctx context.Context, query string) error {
	rows, err := d.W.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("sqlite: migration 0073 precheck: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d pending explicit deletion request(s) (%s); finish or cancel them in Storage on the previous build, then upgrade",
		ErrLiveV1Dependents, len(ids), strings.Join(ids, ", "))
}
