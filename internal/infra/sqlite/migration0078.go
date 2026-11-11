package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// retireV1RecordVersion is migration 0078_retire_v1_record.sql.
const retireV1RecordVersion = 78

// publicationsVersion is migration 0072_pipeline_v2.sql, which creates the
// publications table (and, from the old receipts, v1_record rows in it).
const publicationsVersion = 72

// ErrLiveV1Records refuses migration 0078 while a publication that is not
// removed still depends on Runner's v1 delivery record for its manifest.
var ErrLiveV1Records = errors.New("migration 0078 refused: publications still on a v1_record manifest")

// preflightRetireV1Record is the count check IMPL_CURATOR section 8 asks for
// before the v1_record source goes: a publication whose manifest is only the
// stored v1 record cannot be imported or cleaned once Curator stops reading
// it, so the upgrade stops and names them. They are resolved by the operator's
// review batch (rewrite plan M7) on the previous build, or by the import
// finishing.
func (d *DB) preflightRetireV1Record(ctx context.Context) error {
	version, ok := d.appliedVersion(ctx)
	if !ok || version >= retireV1RecordVersion || version < publicationsVersion {
		return nil // fresh, already retired, or no publications table yet
	}
	return d.refuseLiveV1Records(ctx)
}

// appliedVersion is the highest applied goose version, ok=false for a
// database goose has not touched.
func (d *DB) appliedVersion(ctx context.Context) (int64, bool) {
	var version int64
	if err := d.W.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied=1`).Scan(&version); err != nil {
		return 0, false
	}
	return version, true
}

func (d *DB) refuseLiveV1Records(ctx context.Context) error {
	rows, err := d.W.QueryContext(ctx, `SELECT id, state, path FROM publications WHERE manifest_source = 'v1_record' AND state NOT IN ('removed') ORDER BY id`)
	if err != nil {
		return fmt.Errorf("sqlite: migration 0078 precheck: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var named []string
	total := 0
	for rows.Next() {
		var id int64
		var state, path string
		if err := rows.Scan(&id, &state, &path); err != nil {
			return err
		}
		total++
		if len(named) < 5 {
			named = append(named, fmt.Sprintf("#%d %s %s", id, state, path))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d not yet removed (%s); let their import or review batch finish on the previous build, then upgrade",
		ErrLiveV1Records, total, strings.Join(named, "; "))
}
