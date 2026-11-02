package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type DownloadStorageReference struct {
	ID, ClientID                               int64
	GenerationKey, ImportPath, SavePath, State string
}

func (d *DB) DownloadStorageReferences(ctx context.Context) ([]DownloadStorageReference, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id,client_id,generation_key,import_path,save_path,state FROM downloads ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DownloadStorageReference
	for rows.Next() {
		var r DownloadStorageReference
		if err := rows.Scan(&r.ID, &r.ClientID, &r.GenerationKey, &r.ImportPath, &r.SavePath, &r.State); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func canonicalStorageReviewPath(path string) string {
	if path == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonicalStorageReviewPath(parent), filepath.Base(path))
}
func storageReviewWithin(root, path string) bool {
	return root != "" && path != "" && (path == root || strings.HasPrefix(path, root+string(filepath.Separator)))
}
func (d *DB) storageChangeFences(ctx context.Context) (map[string][]string, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT generation_key,root FROM storage_change_review_fences`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var generation, root string
		if err := rows.Scan(&generation, &root); err != nil {
			return nil, err
		}
		out[generation] = append(out[generation], root)
	}
	return out, rows.Err()
}
func storageReviewFor(fences map[string][]string, generation, path string) bool {
	path = canonicalStorageReviewPath(path)
	for _, root := range fences[generation] {
		if storageReviewWithin(root, path) {
			return true
		}
	}
	return false
}
func (d *DB) StorageChangeReview(ctx context.Context, generation, path string) (bool, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT root FROM storage_change_review_fences WHERE generation_key=?`, generation)
	if err != nil {
		return true, err
	}
	defer func() { _ = rows.Close() }()
	path = canonicalStorageReviewPath(path)
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return true, err
		}
		if storageReviewWithin(root, path) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// AcceptCompletedStorageChange persists the new observation, root-specific
// historical fences and audit together. The service reobserves the live mount
// while holding mutation locks immediately before this transaction.
func (d *DB) AcceptCompletedStorageChange(ctx context.Context, root, expected, observed, reviewToken, report string, generations, orphanPaths []string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT report FROM completed_scans WHERE root=?`, root).Scan(&raw); err != nil {
		return err
	}
	var prior struct {
		StorageIdentity string `json:"storageIdentity"`
	}
	if err := json.Unmarshal([]byte(raw), &prior); err != nil {
		return err
	}
	if prior.StorageIdentity != expected || expected == observed || expected == "" || observed == "" {
		return fmt.Errorf("storage review changed; refresh the comparison")
	}
	for _, generation := range generations {
		if generation == "" {
			return fmt.Errorf("storage change fence requires exact generation")
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO storage_change_review_fences(generation_key,root) VALUES(?,?)`, generation, root); err != nil {
			return err
		}
	}
	for _, path := range orphanPaths {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, "storage-change:"+reviewToken, path); err != nil {
			return err
		}
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO storage_change_acceptances(review_token,root,expected_identity,observed_identity,affected_generations,accepted_at) VALUES(?,?,?,?,?,?)`, reviewToken, root, expected, observed, len(generations), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE completed_scans SET report=? WHERE root=?`, report, root); err != nil {
		return err
	}
	audit, _ := json.Marshal(map[string]any{"root": root, "expectedIdentity": expected, "observedIdentity": observed, "affectedGenerations": len(generations), "existingCustodyRequiresReview": true})
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_events(ts,type,media_item_id,release_title,data) VALUES(?,'storage_change_accepted',0,?,?)`, now, root, string(audit)); err != nil {
		return err
	}
	return tx.Commit()
}
