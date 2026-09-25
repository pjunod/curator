package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	runner "github.com/pjunod/monarr/internal/adapters/nzbd"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/probe"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type RecoverySettings struct {
	Enabled              bool   `json:"enabled"`
	LocalRoot            string `json:"local_root"`
	RemoteRoot           string `json:"remote_root"`
	ConsumerToken        string `json:"consumer_token,omitempty"`
	CredentialConfigured bool   `json:"credential_configured"`
}
type RecoveryFile struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type RunnerRecovery struct {
	ID             string         `json:"id"`
	Installation   string         `json:"installation"`
	Artifact       string         `json:"artifact"`
	Generation     string         `json:"generation"`
	State          string         `json:"state"`
	ManifestDigest string         `json:"manifest_digest"`
	Published      string         `json:"published"`
	Files          []RecoveryFile `json:"files"`
	ClientID       int64          `json:"client_id"`
	Error          string         `json:"error,omitempty"`
}
type RecoveryRequest struct {
	ClientID         int64    `json:"client_id"`
	RecoveryID       string   `json:"recovery_id"`
	MediaItemID      int64    `json:"media_item_id"`
	CopyID           int64    `json:"copy_id"`
	FileIDs          []string `json:"file_ids"`
	TargetGeneration string   `json:"target_generation"`
	AcceptUnverified bool     `json:"accept_unverified"`
}
type RecoveryPreviewFile struct {
	RecoveryFile
	Info     mediainfo.Info `json:"info"`
	Season   int            `json:"season"`
	Episodes []int          `json:"episodes"`
	Usable   bool           `json:"usable"`
	Reason   string         `json:"reason"`
}
type RecoveryPreview struct {
	Request   RecoveryRequest       `json:"request"`
	Recovery  RunnerRecovery        `json:"recovery"`
	Target    string                `json:"target"`
	Files     []RecoveryPreviewFile `json:"files"`
	CopyBytes int64                 `json:"copy_bytes"`
}
type RecoveryImport struct {
	ID       string          `json:"id"`
	State    string          `json:"state"`
	Request  RecoveryRequest `json:"request"`
	Recovery RunnerRecovery  `json:"recovery"`
	Error    string          `json:"error,omitempty"`
}

func (s *Service) RecoverySettings(ctx context.Context) (RecoverySettings, error) {
	raw, err := s.db.GetMeta(ctx, "recovery_settings")
	if errors.Is(err, sql.ErrNoRows) {
		return RecoverySettings{LocalRoot: "/recovery", RemoteRoot: "/processing/recovery/published"}, nil
	}
	if err != nil {
		return RecoverySettings{}, err
	}
	var out RecoverySettings
	err = json.Unmarshal([]byte(raw), &out)
	out.CredentialConfigured = out.ConsumerToken != ""
	return out, err
}
func (s *Service) SetRecoverySettings(ctx context.Context, cfg RecoverySettings) error {
	prior, err := s.RecoverySettings(ctx)
	if err != nil {
		return err
	}
	if cfg.ConsumerToken == "" {
		cfg.ConsumerToken = prior.ConsumerToken
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.db.SetMeta(ctx, "recovery_settings", string(raw))
}
func (s *Service) recoveryClient(ctx context.Context, id int64) (*runner.Client, error) {
	cfg, err := s.db.GetDownloadClient(ctx, id)
	if err != nil {
		return nil, err
	}
	if cfg.Type != "nzbd" {
		return nil, fmt.Errorf("recovery requires a native Runner client")
	}
	return runner.New(cfg), nil
}
func (s *Service) RunnerRecoveries(ctx context.Context) ([]RunnerRecovery, error) {
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return nil, err
	}
	var out []RunnerRecovery
	for _, cfg := range clients {
		if cfg.Type != "nzbd" {
			continue
		}
		var rows []RunnerRecovery
		err = runner.New(cfg).RecoveryRequest(ctx, http.MethodGet, "recoveries", "", nil, &rows)
		if err != nil {
			out = append(out, RunnerRecovery{ClientID: cfg.ID, Error: err.Error()})
			continue
		}
		for i := range rows {
			rows[i].ClientID = cfg.ID
		}
		out = append(out, rows...)
	}
	return out, nil
}
func (s *Service) recoveryPayload(ctx context.Context, r RunnerRecovery) (string, error) {
	cfg, err := s.RecoverySettings(ctx)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(cfg.RemoteRoot) || !filepath.IsAbs(cfg.LocalRoot) {
		return "", fmt.Errorf("configure absolute recovery mount prefixes")
	}
	remote := filepath.Clean(cfg.RemoteRoot)
	if filepath.Clean(r.Published) != filepath.Join(remote, r.ID) || strings.ContainsAny(r.ID, "/\\.") {
		return "", fmt.Errorf("published recovery does not match the configured exact prefix")
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if within(root.Path, cfg.LocalRoot) || within(cfg.LocalRoot, root.Path) {
			return "", fmt.Errorf("recovery mount overlaps library root")
		}
	}
	path := filepath.Join(cfg.LocalRoot, r.ID, "payload")
	if err = rejectSymlinks(path, false); err != nil {
		return "", err
	}
	return path, nil
}
func (s *Service) targetGeneration(ctx context.Context, itemID, copyID int64) (string, string, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return "", "", err
	}
	files, err := s.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		return "", "", err
	}
	dest := item.Path
	var copyData any
	if copyID != 0 {
		cp, e := s.db.GetMediaCopy(ctx, itemID, copyID)
		if e != nil {
			return "", "", e
		}
		copyData = cp
		if cp.Path != "" {
			dest = cp.Path
		}
	}
	if dest == "" {
		return "", "", ErrNoLibraryFolder
	}
	raw, err := json.Marshal([]any{item, copyData, files})
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), dest, nil
}
func (s *Service) PreviewRecovery(ctx context.Context, req RecoveryRequest) (RecoveryPreview, error) {
	client, err := s.recoveryClient(ctx, req.ClientID)
	if err != nil {
		return RecoveryPreview{}, err
	}
	var r RunnerRecovery
	if strings.ContainsAny(req.RecoveryID, "/\\.") {
		return RecoveryPreview{}, fmt.Errorf("invalid recovery id")
	}
	if err = client.RecoveryRequest(ctx, http.MethodGet, "recoveries/"+req.RecoveryID, "", nil, &r); err != nil {
		return RecoveryPreview{}, err
	}
	r.ClientID = req.ClientID
	root, err := s.recoveryPayload(ctx, r)
	if err != nil {
		return RecoveryPreview{}, err
	}
	generation, dest, err := s.targetGeneration(ctx, req.MediaItemID, req.CopyID)
	if err != nil {
		return RecoveryPreview{}, err
	}
	req.TargetGeneration = generation
	preview := RecoveryPreview{Request: req, Recovery: r, Target: dest}
	selected := map[string]bool{}
	for _, id := range req.FileIDs {
		if selected[id] {
			return preview, fmt.Errorf("duplicate recovery file selection")
		}
		selected[id] = true
	}
	for _, f := range r.Files {
		if len(req.FileIDs) > 0 && !selected[f.ID] {
			continue
		}
		delete(selected, f.ID)
		if filepath.IsAbs(f.Path) || !filepath.IsLocal(f.Path) {
			return preview, fmt.Errorf("unsafe manifest path")
		}
		path := filepath.Join(root, f.Path)
		hash, size, e := fileDigest(ctx, path)
		if e != nil {
			return preview, e
		}
		if hash != f.SHA256 || size != f.Bytes {
			return preview, fmt.Errorf("staged digest mismatch for %s", f.Path)
		}
		info, e := probe.File(path)
		p := parser.Parse(filepath.Base(path))
		row := RecoveryPreviewFile{RecoveryFile: f, Info: info, Season: p.Season, Episodes: p.Episodes, Usable: e == nil && info.Container != "" && !mediainfo.IsUnsupported(info.Container)}
		if e != nil {
			row.Reason = e.Error()
		}
		if why, cut := truncatedPayload(path); cut {
			row.Usable = false
			row.Reason = why
		}
		if !info.Measured() && row.Usable && !req.AcceptUnverified {
			row.Usable = false
			row.Reason = "recognized container with unverified media; explicit acceptance required"
		}
		preview.Files = append(preview.Files, row)
		preview.CopyBytes += f.Bytes
	}
	if len(selected) > 0 {
		return preview, fmt.Errorf("selection is not in the manifest")
	}
	if len(preview.Files) == 0 {
		return preview, fmt.Errorf("no recovery files selected")
	}
	if err = recoveryCapacity(dest, preview.CopyBytes); err != nil {
		return preview, err
	}
	return preview, nil
}
func (s *Service) QueueRecovery(ctx context.Context, req RecoveryRequest) (RecoveryImport, error) {
	preview, err := s.PreviewRecovery(ctx, req)
	if err != nil {
		return RecoveryImport{}, err
	}
	if req.TargetGeneration == "" || req.TargetGeneration != preview.Request.TargetGeneration {
		return RecoveryImport{}, fmt.Errorf("target changed; create a new preview")
	}
	for _, f := range preview.Files {
		if !f.Usable {
			return RecoveryImport{}, fmt.Errorf("%s: %s", f.Path, f.Reason)
		}
	}
	key := preview.Recovery.Installation + "/" + preview.Recovery.ID + "/" + preview.Recovery.ManifestDigest
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	r := RecoveryImport{ID: id, State: "queued", Request: req, Recovery: preview.Recovery}
	raw, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, id, r.State, string(raw), time.Now().UnixMilli())
	if err != nil {
		return r, err
	}
	existing, err := s.RecoveryImport(ctx, id)
	if err != nil {
		return r, err
	}
	want, _ := json.Marshal(req)
	got, _ := json.Marshal(existing.Request)
	if string(want) != string(got) {
		return r, fmt.Errorf("recovery already queued with different targets")
	}
	return existing, nil
}
func (s *Service) RecoveryImport(ctx context.Context, id string) (RecoveryImport, error) {
	var raw string
	err := s.db.R.QueryRowContext(ctx, `SELECT data FROM recovery_imports WHERE id=?`, id).Scan(&raw)
	var r RecoveryImport
	if err == nil {
		err = json.Unmarshal([]byte(raw), &r)
	}
	return r, err
}
func (s *Service) saveRecoveryImport(ctx context.Context, r RecoveryImport) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.W.ExecContext(ctx, `UPDATE recovery_imports SET state=?,data=?,updated_at=? WHERE id=?`, r.State, string(raw), time.Now().UnixMilli(), r.ID)
	return err
}
func (s *Service) runRecovery(ctx context.Context, r RecoveryImport) error {
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	ctx = context.WithValue(ctx, targetHeldKey{}, true)
	generation, _, err := s.targetGeneration(ctx, r.Request.MediaItemID, r.Request.CopyID)
	if err != nil {
		return err
	}
	if generation != r.Request.TargetGeneration {
		return fmt.Errorf("library target changed; review the recovery before retrying")
	}
	cfg, err := s.RecoverySettings(ctx)
	if err != nil {
		return err
	}
	client, err := s.recoveryClient(ctx, r.Request.ClientID)
	if err != nil {
		return err
	}
	if err = client.RecoveryRequest(ctx, http.MethodPost, "recoveries/"+r.Recovery.ID+"/claim", cfg.ConsumerToken, map[string]any{"import_id": r.ID, "manifest_digest": r.Recovery.ManifestDigest}, nil); err != nil {
		return err
	}
	root, err := s.recoveryPayload(ctx, r.Recovery)
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, id := range r.Request.FileIDs {
		selected[id] = true
	}
	var paths []string
	ids := map[string]string{}
	for _, f := range r.Recovery.Files {
		if len(r.Request.FileIDs) > 0 && !selected[f.ID] {
			continue
		}
		path := filepath.Join(root, f.Path)
		hash, size, e := fileDigest(ctx, path)
		if e != nil {
			return e
		}
		if hash != f.SHA256 || size != f.Bytes {
			return fmt.Errorf("recovery file changed before import")
		}
		paths = append(paths, path)
		ids[path] = f.ID
	}
	ctx = context.WithValue(ctx, recoveryPlacementKey{}, &recoveryPlacementContext{ImportID: r.ID, Files: ids})
	result, importErr := s.importDownloadFiles(ctx, sqlite.Download{MediaItemID: r.Request.MediaItemID, CopyID: r.Request.CopyID, ReleaseTitle: "recovery " + r.Recovery.ID}, root, paths, true)
	if result.Imported != len(paths) || importErr != nil {
		return fmt.Errorf("partial recovery remains held: %d/%d imported: %v", result.Imported, len(paths), importErr)
	}
	// Results originate inside the metadata transaction, never from a worker's
	// optimistic return value. Persist the delivery outbox before contacting Runner.
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT result FROM recovery_file_results WHERE import_id=? ORDER BY file_id`, r.ID)
	if err != nil {
		return err
	}
	var files []json.RawMessage
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		files = append(files, json.RawMessage(raw))
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(files) != len(paths) {
		return fmt.Errorf("durable file receipts incomplete")
	}
	receipt, err := json.Marshal(map[string]any{"import_id": r.ID, "manifest_digest": r.Recovery.ManifestDigest, "files": files})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_receipt_outbox(import_id,receipt,updated_at) VALUES(?,?,?) ON CONFLICT(import_id) DO NOTHING`, r.ID, string(receipt), time.Now().UnixMilli()); err != nil {
		return err
	}
	r.State = "receipt_pending"
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_imports SET state=?,data=?,updated_at=? WHERE id=?`, r.State, string(raw), time.Now().UnixMilli(), r.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) recoverySweep(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM recovery_imports WHERE state='queued' ORDER BY updated_at LIMIT 5`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		r, e := s.RecoveryImport(ctx, id)
		if e != nil {
			continue
		}
		if e = s.runRecovery(ctx, r); e != nil {
			r.State = "review"
			r.Error = e.Error()
			_ = s.saveRecoveryImport(ctx, r)
			s.log.Warn("recovery import needs attention", "id", id, "err", e)
		}
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT import_id,receipt FROM recovery_receipt_outbox WHERE delivered=0 ORDER BY updated_at LIMIT 25`)
	if err != nil {
		return
	}
	type pending struct{ id, raw string }
	var pendingRows []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.raw) == nil {
			pendingRows = append(pendingRows, p)
		}
	}
	_ = rows.Close()
	cfg, err := s.RecoverySettings(ctx)
	if err != nil {
		return
	}
	for _, p := range pendingRows {
		r, e := s.RecoveryImport(ctx, p.id)
		if e != nil {
			continue
		}
		client, e := s.recoveryClient(ctx, r.Request.ClientID)
		if e != nil {
			continue
		}
		e = client.RecoveryRequest(ctx, http.MethodPost, "recoveries/"+r.Recovery.ID+"/receipt", cfg.ConsumerToken, json.RawMessage(p.raw), nil)
		if e != nil {
			_, _ = s.db.W.ExecContext(ctx, `UPDATE recovery_receipt_outbox SET last_error=?,updated_at=? WHERE import_id=?`, e.Error(), time.Now().UnixMilli(), p.id)
			continue
		}
		_, e = s.db.W.ExecContext(ctx, `UPDATE recovery_receipt_outbox SET delivered=1,last_error='' WHERE import_id=?`, p.id)
		if e == nil {
			r.State = "imported"
			r.Error = ""
			_ = s.saveRecoveryImport(ctx, r)
		}
	}
}
func (s *Service) startRecoveryWorker(ctx context.Context) {
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			s.recoverySweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// RecoveryAdvisory reports prerequisites without disabling enable controls.
func (s *Service) RecoveryAdvisory(ctx context.Context) map[string]any {
	cfg, err := s.RecoverySettings(ctx)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	info, e := os.Stat(cfg.LocalRoot)
	return map[string]any{"credential_configured": cfg.ConsumerToken != "", "local_mount_available": e == nil && info.IsDir(), "read_only_mount": "verify in container configuration", "capacity": "source + staging + library + replacement backup may coexist"}
}
