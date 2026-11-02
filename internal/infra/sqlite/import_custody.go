package sqlite

import "context"

// ImportSourceCustodyPaths is complete, across downloads and generations.
// Only unresolved or explicitly retained custody participates; successful
// ordinary committed placement receipts do not block cleanup indefinitely.
func (d *DB) ImportSourceCustodyPaths(ctx context.Context) ([]string, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT json_extract(data,'$.source') FROM import_placements WHERE state IN ('prepared','retained')
 UNION SELECT source FROM import_attempt_stages WHERE state='held'
 UNION SELECT source FROM import_publication_authorizations WHERE outcome='pending' OR resolution IN ('retained_for_review','operator_adopted')`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			return nil, err
		}
		if path != "" {
			out = append(out, path)
		}
	}
	return out, rows.Err()
}
