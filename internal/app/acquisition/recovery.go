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
	"strconv"
	"strings"
	"time"

	runner "github.com/pjunod/monarr/internal/adapters/nzbd"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/naming"
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
type RecoveryEpisodeTarget struct {
	Season   int   `json:"season"`
	Episodes []int `json:"episodes"`
}
type RecoveryRequest struct {
	PreviewID        string                           `json:"preview_id,omitempty"`
	EpisodeTargets   map[string]RecoveryEpisodeTarget `json:"episode_targets,omitempty"`
	ClientID         int64                            `json:"client_id"`
	RecoveryID       string                           `json:"recovery_id"`
	MediaItemID      int64                            `json:"media_item_id"`
	CopyID           int64                            `json:"copy_id"`
	FileIDs          []string                         `json:"file_ids"`
	TargetGeneration string                           `json:"target_generation"`
	AcceptUnverified bool                             `json:"accept_unverified"`
}
type RecoveryPreviewFile struct {
	Destination string   `json:"destination"`
	Existing    []string `json:"existing"`
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
type RecoveryTargetSnapshot struct {
	Revision int64             `json:"revision"`
	Metadata string            `json:"metadata"`
	Files    map[string]string `json:"files"`
}
type RecoveryImport struct {
	Destinations     map[string]string      `json:"destinations"`
	CurrentFile      string                 `json:"current_file,omitempty"`
	CopiedBytes      int64                  `json:"copied_bytes,omitempty"`
	TotalBytes       int64                  `json:"total_bytes,omitempty"`
	AcceptedRevision int64                  `json:"accepted_revision"`
	TargetSnapshot   RecoveryTargetSnapshot `json:"target_snapshot"`
	ID               string                 `json:"id"`
	State            string                 `json:"state"`
	Request          RecoveryRequest        `json:"request"`
	Recovery         RunnerRecovery         `json:"recovery"`
	Error            string                 `json:"error,omitempty"`
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
		for offset := 0; offset < 10000; offset += 100 {
			var rows []RunnerRecovery
			err = runner.New(cfg).RecoveryRequest(ctx, http.MethodGet, "recoveries?offset="+strconv.Itoa(offset), "", nil, &rows)
			if err != nil {
				out = append(out, RunnerRecovery{ClientID: cfg.ID, Error: err.Error()})
				break
			}
			for i := range rows {
				rows[i].ClientID = cfg.ID
			}
			out = append(out, rows...)
			if len(rows) < 100 {
				break
			}
			if offset == 9900 {
				out = append(out, RunnerRecovery{ClientID: cfg.ID, Error: "More than 10,000 active recoveries; narrow the Runner inventory before refreshing"})
			}
		}
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
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return "", err
	}
	for _, client := range clients {
		for _, mapping := range client.PathMappings {
			if mapping.Local != "" && (within(mapping.Local, cfg.LocalRoot) || within(cfg.LocalRoot, mapping.Local)) {
				return "", fmt.Errorf("recovery mount overlaps a completed-download path mapping")
			}
		}
	}
	path := filepath.Join(cfg.LocalRoot, r.ID, "payload")
	if err = rejectSymlinks(path, false); err != nil {
		return "", err
	}
	return path, nil
}
func (s *Service) targetSnapshot(ctx context.Context, itemID, copyID int64) (RecoveryTargetSnapshot, string, error) {
	snapshot := RecoveryTargetSnapshot{Files: map[string]string{}}
	revision, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return snapshot, "", err
	}
	snapshot.Revision = revision
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return snapshot, "", err
	}
	dest, profileID := item.Path, item.QualityProfileID
	rootFolderID := item.RootFolderID
	var copyData any
	if copyID != 0 {
		cp, e := s.db.GetMediaCopy(ctx, itemID, copyID)
		if e != nil {
			return snapshot, "", e
		}
		copyData = cp
		if cp.RootFolderID != 0 {
			rootFolderID = cp.RootFolderID
		}
		profileID = cp.QualityProfileID
		if cp.Path != "" {
			dest = cp.Path
		}
	}
	if dest == "" {
		return snapshot, "", ErrNoLibraryFolder
	}
	profile, err := s.db.GetProfile(ctx, profileID)
	if err != nil {
		return snapshot, "", err
	}
	for i := range item.Seasons {
		for j := range item.Seasons[i].Episodes {
			item.Seasons[i].Episodes[j].HasFile = false
		}
	}
	// Bind the configured library volume, including its filesystem identity.
	roots, e := s.db.ListRootFolders(ctx)
	if e != nil {
		return snapshot, "", e
	}
	var rootIdentity any
	for _, root := range roots {
		if root.ID == rootFolderID {
			if e = rejectSymlinks(root.Path, false); e != nil {
				return snapshot, "", e
			}
			info, e := os.Stat(root.Path)
			if e != nil {
				return snapshot, "", e
			}
			if !info.IsDir() {
				return snapshot, "", fmt.Errorf("library root is not a directory")
			}
			rootIdentity = []any{root.Path, recoveryDirectoryIdentity(info)}
		}
	}
	metadata, err := json.Marshal([]any{rootIdentity, item.ID, item.Kind, item.Title, item.Year, item.Runtime, item.Path, item.RootFolderID, profile, copyData, item.Seasons})
	if err != nil {
		return snapshot, "", err
	}
	snapshot.Metadata = fmt.Sprintf("%x", sha256.Sum256(metadata))
	files, err := s.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		return snapshot, "", err
	}
	for _, f := range files {
		if f.CopyID != copyID {
			continue
		}
		var disk any = "missing"
		if info, e := os.Lstat(f.Path); e == nil {
			disk = []any{info.Size(), info.ModTime().UnixNano(), info.Mode().String()}
		} else if !errors.Is(e, os.ErrNotExist) {
			return snapshot, "", e
		}
		raw, e := json.Marshal([]any{f, disk})
		if e != nil {
			return snapshot, "", e
		}
		snapshot.Files[f.Path] = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	after, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return snapshot, "", err
	}
	if after != revision {
		return snapshot, "", fmt.Errorf("library changed while building preview; retry")
	}
	return snapshot, dest, nil
}
func (s *Service) targetGeneration(ctx context.Context, itemID, copyID int64) (string, string, error) {
	snapshot, dest, err := s.targetSnapshot(ctx, itemID, copyID)
	if err != nil {
		return "", "", err
	}
	raw, err := json.Marshal(snapshot)
	return fmt.Sprintf("%x", sha256.Sum256(raw)), dest, err
}
func (s *Service) validateRecoveryTarget(ctx context.Context, r RecoveryImport) error {
	persisted, err := s.RecoveryImport(ctx, r.ID)
	if err != nil {
		return err
	}
	revision, err := s.db.LibraryRevision(ctx)
	if err != nil {
		return err
	}
	if revision != persisted.AcceptedRevision {
		return fmt.Errorf("library changed since preview; new preview required")
	}

	current, _, err := s.targetSnapshot(ctx, r.Request.MediaItemID, r.Request.CopyID)
	if err != nil {
		return err
	}
	expected := RecoveryTargetSnapshot{Revision: current.Revision, Metadata: r.TargetSnapshot.Metadata, Files: map[string]string{}}
	for path, fingerprint := range r.TargetSnapshot.Files {
		expected.Files[path] = fingerprint
	}
	if current.Metadata != expected.Metadata {
		return fmt.Errorf("library metadata or profile changed; new preview required")
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE json_extract(data,'$.recovery_import')=?`, r.ID)
	if err != nil {
		return err
	}
	var placements []sqlite.Placement
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		var p sqlite.Placement
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			_ = rows.Close()
			return err
		}
		placements = append(placements, p)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, p := range placements {
		hash, _, e := fileDigest(ctx, p.Target)
		if e != nil || hash != p.SHA256 {
			continue
		}
		delete(current.Files, p.Target)
		delete(expected.Files, p.Target)
		for _, old := range p.Superseded {
			if _, exists := current.Files[old.Path]; !exists {
				delete(expected.Files, old.Path)
			}
		}
	}
	a, _ := json.Marshal(current)
	b, _ := json.Marshal(expected)
	if string(a) != string(b) {
		return fmt.Errorf("library files changed; new preview required")
	}
	return nil
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
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return preview, err
	}

	selected := map[string]bool{}
	for _, id := range req.FileIDs {
		if selected[id] {
			return preview, fmt.Errorf("duplicate recovery file selection")
		}
		selected[id] = true
	}
	if len(req.FileIDs) > 0 && len(req.FileIDs) != len(r.Files) {
		return preview, fmt.Errorf("import the whole staged handoff; choose a smaller selection when staging in Runner")
	}
	if item.Kind == domain.KindMovie && len(r.Files) != 1 {
		return preview, fmt.Errorf("a movie handoff must contain one feature file; stage alternate versions separately")
	}
	destinations, episodes := map[string]bool{}, map[string]bool{}
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
		info, e := probe.NativeFile(path)
		p := parser.Parse(filepath.Base(path))
		if target, ok := req.EpisodeTargets[f.ID]; ok {
			p.Season = target.Season
			p.Episodes = target.Episodes
		}
		if len(p.Episodes) > 0 {
			for _, ep := range p.Episodes {
				if _, e := s.db.GetEpisodeID(ctx, req.MediaItemID, p.Season, ep); e != nil {
					return preview, fmt.Errorf("unknown episode S%02dE%02d", p.Season, ep)
				}
			}
		}

		namedPath := path
		if info.Container == mediainfo.ContainerMKV || info.Container == mediainfo.ContainerMP4 {
			namedPath = strings.TrimSuffix(path, filepath.Ext(path)) + "." + info.Container
		}
		row := RecoveryPreviewFile{RecoveryFile: f, Info: info, Season: p.Season, Episodes: p.Episodes, Usable: e == nil && info.Container != "" && !mediainfo.IsUnsupported(info.Container)}
		if item.Kind == domain.KindMovie {
			row.Destination = movieDestination(dest, item, namedPath, p.Quality)
		} else if item.Kind == domain.KindSeries && len(p.Episodes) > 0 {
			title := ""
			for _, season := range item.Seasons {
				if season.Number == p.Season {
					for _, ep := range season.Episodes {
						if ep.EpisodeNumber == p.Episodes[0] {
							title = ep.Title
						}
					}
				}
			}
			row.Destination = episodeDestination(dest, item, namedPath, p.Quality, p.Season, p.Episodes, title)
		} else if item.Kind == domain.KindBook {
			row.Destination = filepath.Join(dest, naming.BookFileName(item.Author, item.Title)+strings.ToLower(filepath.Ext(path)))
		}
		existing, e := s.db.ListFilesForItem(ctx, item.ID)
		if e != nil {
			return preview, e
		}
		for _, file := range existing {
			if file.CopyID == req.CopyID {
				row.Existing = append(row.Existing, file.Path+" ("+strconv.FormatInt(file.Size, 10)+" bytes)")
			}
		}

		if item.Kind == domain.KindSeries && len(p.Episodes) == 0 {
			row.Usable = false
			row.Reason = "choose the episode mapping before import"
		}

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
		if row.Destination != "" {
			canonical := strings.ToLower(filepath.Clean(row.Destination))
			if destinations[canonical] {
				return preview, fmt.Errorf("multiple files target the same library path: %s", row.Destination)
			}
			destinations[canonical] = true
		}
		if item.Kind == domain.KindSeries {
			for _, ep := range p.Episodes {
				key := fmt.Sprintf("%d:%d", p.Season, ep)
				if episodes[key] {
					return preview, fmt.Errorf("multiple files target episode S%02dE%02d", p.Season, ep)
				}
				episodes[key] = true
			}
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
	preview, err := s.acceptedRecoveryPreview(ctx, req)
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
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	snapshot, _, err := s.targetSnapshot(ctx, req.MediaItemID, req.CopyID)
	if err != nil {
		return RecoveryImport{}, err
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return RecoveryImport{}, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(rawSnapshot)) != req.TargetGeneration {
		return RecoveryImport{}, fmt.Errorf("library changed while inspecting recovery; preview again")
	}
	destinations := map[string]string{}
	for _, f := range preview.Files {
		destinations[f.ID] = f.Destination
	}
	r := RecoveryImport{Destinations: destinations, ID: id, State: "queued", Request: req, Recovery: preview.Recovery, TargetSnapshot: snapshot, AcceptedRevision: snapshot.Revision}
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
	if existing.State == "review" {
		oldRequest, newRequest := existing.Request, req
		oldRequest.TargetGeneration = ""
		oldRequest.PreviewID = ""
		newRequest.TargetGeneration = ""
		newRequest.PreviewID = ""
		oldRaw, _ := json.Marshal(oldRequest)
		newRaw, _ := json.Marshal(newRequest)
		if string(oldRaw) == string(newRaw) {
			existing.Destinations = destinations
			existing.Request = req
			existing.TargetSnapshot = snapshot
			existing.AcceptedRevision = snapshot.Revision
			existing.State = "queued"
			existing.Error = ""
			return existing, s.saveRecoveryImport(ctx, existing)
		}
	}
	compareRequest, compareExisting := req, existing.Request
	compareRequest.PreviewID, compareExisting.PreviewID = "", ""
	want, _ := json.Marshal(compareRequest)
	got, _ := json.Marshal(compareExisting)
	if string(want) != string(got) {
		return r, fmt.Errorf("recovery already queued with different targets")
	}
	return existing, nil
}
func (s *Service) RecoveryImport(ctx context.Context, id string) (RecoveryImport, error) {
	var raw string
	var state string
	var deliveryError sql.NullString
	err := s.db.R.QueryRowContext(ctx, `SELECT data,state,(SELECT last_error FROM recovery_receipt_outbox WHERE import_id=recovery_imports.id) FROM recovery_imports WHERE id=?`, id).Scan(&raw, &state, &deliveryError)
	var r RecoveryImport
	if err == nil {
		err = json.Unmarshal([]byte(raw), &r)
		r.State = state
		if state == "receipt_pending" && deliveryError.Valid {
			r.Error = deliveryError.String
		}
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
	if err := s.validateRecoveryTarget(ctx, r); err != nil {
		return err
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
	// Heartbeats are informational. Runner never expires a silent claim.
	ctx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func(heartbeatCtx context.Context) {
		defer close(heartbeatDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				var remote RunnerRecovery
				e := client.RecoveryRequest(heartbeatCtx, http.MethodPost, "recoveries/"+r.Recovery.ID+"/claim", cfg.ConsumerToken, map[string]any{"import_id": r.ID, "manifest_digest": r.Recovery.ManifestDigest}, &remote)
				if e != nil {
					if client.RecoveryRequest(heartbeatCtx, http.MethodGet, "recoveries/"+r.Recovery.ID, "", nil, &remote) == nil && remote.State == "cancel_pending" {
						_ = s.CancelRecoveryImport(heartbeatCtx, r.ID)
						return
					}
				}
			}
		}
	}(ctx)
	defer func() { stopHeartbeat(); <-heartbeatDone }()
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
	destinations := map[string]string{}
	targets := map[string]RecoveryEpisodeTarget{}
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
		destinations[path] = r.Destinations[f.ID]
		if target, ok := r.Request.EpisodeTargets[f.ID]; ok {
			targets[path] = target
		}
	}
	lastProgress := time.Time{}
	progress := func(path string, done, total int64) {
		if done != total && time.Since(lastProgress) < time.Second {
			return
		}
		lastProgress = time.Now()
		_, _ = s.db.W.ExecContext(ctx, `UPDATE recovery_imports SET data=json_set(data,'$.current_file',?,'$.copied_bytes',?,'$.total_bytes',?),updated_at=? WHERE id=?`, filepath.Base(path), done, total, time.Now().UnixMilli(), r.ID)
	}
	ctx = context.WithValue(ctx, recoveryPlacementKey{}, &recoveryPlacementContext{Destinations: destinations, Progress: progress, ImportID: r.ID, Files: ids, EpisodeTargets: targets, Validate: func() error { return s.validateRecoveryTarget(ctx, r) }})
	result, importErr := s.importDownloadFiles(ctx, sqlite.Download{MediaItemID: r.Request.MediaItemID, CopyID: r.Request.CopyID, ReleaseTitle: "recovery " + r.Recovery.ID}, root, paths, true)
	if result.Imported != len(paths) || importErr != nil {
		return fmt.Errorf("partial recovery remains held: %d/%d imported: %v", result.Imported, len(paths), importErr)
	}
	if err = s.verifyRecoveryPublications(ctx, r); err != nil {
		return err
	}
	// Results originate inside the metadata transaction, never from a worker's
	// optimistic return value. Persist the delivery outbox before contacting Runner.
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var currentState string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM recovery_imports WHERE id=?`, r.ID).Scan(&currentState); err != nil {
		return err
	}
	if currentState == "cancel_pending" {
		return context.Canceled
	}
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
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM recovery_imports WHERE state IN ('queued','importing','cancel_pending') ORDER BY updated_at LIMIT 5`)
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
		if r.State == "cancel_pending" {
			s.ackRecoveryCancel(ctx, r)
			continue
		}
		worker, cancel := context.WithCancel(ctx)
		s.recoveryMu.Lock()
		if s.recoveryCancels == nil {
			s.recoveryCancels = map[string]context.CancelFunc{}
		}
		s.recoveryCancels[id] = cancel
		s.recoveryMu.Unlock()
		// Conditional update prevents a cancellation between admission and start
		// from being overwritten by an optimistic in-memory state.
		result, e := s.db.W.ExecContext(ctx, `UPDATE recovery_imports SET state='importing' WHERE id=? AND state IN ('queued','importing')`, id)
		started := false
		if e == nil {
			n, _ := result.RowsAffected()
			started = n == 1
		}
		if started {
			e = s.runRecovery(worker, r)
		}
		cancel()
		s.recoveryMu.Lock()
		delete(s.recoveryCancels, id)
		s.recoveryMu.Unlock()
		if ctx.Err() != nil {
			return
		}
		latest, readErr := s.RecoveryImport(ctx, id)
		var state string
		_ = s.db.R.QueryRowContext(ctx, `SELECT state FROM recovery_imports WHERE id=?`, id).Scan(&state)
		if state == "cancel_pending" {
			s.ackRecoveryCancel(ctx, r)
			continue
		}
		if e != nil && readErr == nil {
			latest.State = "review"
			latest.Error = e.Error()
			_ = s.saveRecoveryImport(ctx, latest)
			s.log.Warn("recovery import needs attention", "id", id, "err", e)
		}
	}
	s.deliverRecoveryReceipts(ctx)
}
func (s *Service) deliverRecoveryReceipts(ctx context.Context) {
	s.recoveryReceiptMu.Lock()
	defer s.recoveryReceiptMu.Unlock()
	rows, err := s.db.R.QueryContext(ctx, `SELECT import_id,receipt FROM recovery_receipt_outbox WHERE delivered=0 ORDER BY updated_at LIMIT 25`)
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
		var remote RunnerRecovery
		e = client.RecoveryRequest(ctx, http.MethodPost, "recoveries/"+r.Recovery.ID+"/receipt", cfg.ConsumerToken, json.RawMessage(p.raw), &remote)
		if e != nil {
			_, _ = s.db.W.ExecContext(ctx, `UPDATE recovery_receipt_outbox SET last_error=?,updated_at=? WHERE import_id=?`, e.Error(), time.Now().UnixMilli(), p.id)
			continue
		}
		if remote.State != "imported" {
			_, _ = s.db.W.ExecContext(ctx, `UPDATE recovery_receipt_outbox SET last_error=?,updated_at=? WHERE import_id=?`, "Runner receipt state is "+remote.State+"; source remains held", time.Now().UnixMilli(), p.id)
			continue
		}
		// Delivery acknowledgement and local completion are one transaction;
		// a crash cannot strand an import behind a delivered outbox row.
		r.State = "imported"
		r.Error = ""
		raw, e := json.Marshal(r)
		if e != nil {
			continue
		}
		tx, e := s.db.W.BeginTx(ctx, nil)
		if e != nil {
			continue
		}
		if _, e = tx.ExecContext(ctx, `UPDATE recovery_receipt_outbox SET delivered=1,last_error='' WHERE import_id=?`, p.id); e == nil {
			_, e = tx.ExecContext(ctx, `UPDATE recovery_imports SET state='imported',data=?,updated_at=? WHERE id=?`, string(raw), time.Now().UnixMilli(), p.id)
		}
		if e == nil {
			e = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if e != nil {
			s.log.Warn("receipt acknowledgement remains pending", "id", p.id, "err", e)
		}

	}
}
func (s *Service) startRecoveryWorker(ctx context.Context) {
	s.startRecoveryPreviewWorker(ctx)
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			s.deliverRecoveryReceipts(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

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

func (s *Service) CancelRecoveryImport(ctx context.Context, id string) error {
	result, err := s.db.W.ExecContext(ctx, `UPDATE recovery_imports SET state='cancel_pending',updated_at=? WHERE id=? AND state IN ('queued','importing','review')`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("import already committed or cancellation pending")
	}
	s.recoveryMu.Lock()
	cancel := s.recoveryCancels[id]
	s.recoveryMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
func (s *Service) ackRecoveryCancel(ctx context.Context, r RecoveryImport) {
	s.importTargetMu.Lock()
	err := s.rollbackRecoveryPlacements(ctx, r.ID)
	s.importTargetMu.Unlock()
	if err != nil {
		r.State, r.Error = "cancel_pending", "Cancellation reconciliation: "+err.Error()
		_ = s.saveRecoveryImport(ctx, r)
		return
	}
	cfg, err := s.RecoverySettings(ctx)
	if err != nil {
		return
	}
	client, err := s.recoveryClient(ctx, r.Request.ClientID)
	if err != nil {
		return
	}
	var remote RunnerRecovery
	if err = client.RecoveryRequest(ctx, http.MethodPost, "recoveries/"+r.Recovery.ID+"/cancel", cfg.ConsumerToken, nil, &remote); err != nil {
		return
	}
	if remote.State == "cancel_pending" {
		if err = client.RecoveryRequest(ctx, http.MethodPost, "recoveries/"+r.Recovery.ID+"/cancel-ack", cfg.ConsumerToken, nil, nil); err != nil {
			return
		}
	}
	r.State = "cancelled"
	r.Error = "Worker stopped; source remains on review hold"
	_ = s.saveRecoveryImport(ctx, r)
}
func (s *Service) RecoveryImports(ctx context.Context) ([]RecoveryImport, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT data,state FROM recovery_imports ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RecoveryImport{}
	for rows.Next() {
		var raw, state string
		if err = rows.Scan(&raw, &state); err != nil {
			return nil, err
		}
		var r RecoveryImport
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		r.State = state
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecoveryMetrics contains bounded, low-cardinality lifecycle counts.
func (s *Service) RecoveryMetrics(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT state,count(*) FROM recovery_imports GROUP BY state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var count int64
		if err = rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[state] = count
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var pending int64
	if err = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_receipt_outbox WHERE delivered=0`).Scan(&pending); err != nil {
		return nil, err
	}
	out["receipt_outbox_pending"] = pending
	return out, nil
}
