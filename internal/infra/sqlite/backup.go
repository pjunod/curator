package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupKeep is how many backups the retention pass preserves.
const backupKeep = 7

// BackupInfo describes one on-disk backup file.
type BackupInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"sizeBytes"`
	CreatedAt time.Time `json:"createdAt"`
}

// Backup writes a consistent online snapshot with VACUUM INTO (safe under
// WAL, no locks held) into <dataDir>/backups and prunes old ones.
func (d *DB) Backup(ctx context.Context) (string, error) {
	dir := filepath.Join(filepath.Dir(d.Path), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "monarr-" + time.Now().UTC().Format("20060102-150405") + ".db"
	dest := filepath.Join(dir, name)
	// VACUUM INTO refuses to overwrite; the timestamped name prevents it.
	if _, err := d.W.ExecContext(ctx,
		fmt.Sprintf("VACUUM INTO %q", dest)); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}

	// Retention: newest backupKeep stay.
	backups, err := d.ListBackups()
	if err == nil && len(backups) > backupKeep {
		for _, b := range backups[backupKeep:] {
			_ = os.Remove(filepath.Join(dir, b.Name))
		}
	}
	return dest, nil
}

// ListBackups returns backups newest-first.
func (d *DB) ListBackups() ([]BackupInfo, error) {
	dir := filepath.Join(filepath.Dir(d.Path), "backups")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "monarr-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			Name: e.Name(), SizeBytes: info.Size(), CreatedAt: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}
