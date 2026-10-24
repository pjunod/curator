// Package qbittorrent implements ports.DownloadClient against the
// qBittorrent WebUI API v2.
package qbittorrent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/adapters/httpx"
	"github.com/pjunod/monarr/internal/ports"
)

// Client is a DownloadClient for one qBittorrent instance.
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client

	mu       sync.Mutex
	loggedIn bool
}

var _ ports.DownloadClient = (*Client)(nil)
var _ ports.ConfiguredAdder = (*Client)(nil)
var _ ports.ExactTransferCorrelator = (*Client)(nil)

const transferTagPrefix = "monarr-transfer-"

// New returns a Client; login happens lazily on first use.
func New(cfg ports.ClientConfig) *Client {
	cfg.URL = ports.NormalizeServiceURL(cfg.URL, ports.DefaultPortFor(cfg.Type))
	jar, _ := cookiejar.New(nil)
	return &Client{cfg: cfg, http: httpx.NewClientJar(30*time.Second, jar)}
}

func (c *Client) base() string { return strings.TrimRight(c.cfg.URL, "/") }

func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	form := url.Values{"username": {c.cfg.Username}, "password": {c.cfg.Password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qbittorrent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Ok") {
		return fmt.Errorf("qbittorrent: login rejected (%d %s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	c.loggedIn = true
	return nil
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values) ([]byte, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusForbidden {
		c.mu.Lock()
		c.loggedIn = false // session expired; next call re-logs
		c.mu.Unlock()
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qbittorrent: %s → %d", path, resp.StatusCode)
	}
	return body, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	full := c.base() + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qbittorrent: %s → %d", path, resp.StatusCode)
	}
	return body, nil
}

// Add implements ports.DownloadClient.
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	return c.AddWithOptions(ctx, downloadURL, category, ports.AddOptions{})
}

// AddWithOptions tags the torrent with the opaque transfer token at admission
// so a later info poll can bind the accepted request to its exact hash. The
// WebUI API returns no hash from add; callers persist the transfer and wait
// for the exact tag to appear. A duplicate add that does not echo its new tag
// therefore remains unresolved instead of falling back to title matching.
func (c *Client) AddWithOptions(ctx context.Context, downloadURL, category string, options ports.AddOptions) (ports.Handle, error) {
	form := url.Values{"urls": {downloadURL}}
	if category != "" {
		form.Set("category", category)
	}
	if options.Name != "" {
		form.Set("rename", options.Name)
	}
	if options.Transfer != "" {
		if !validTransferToken(options.Transfer) {
			return "", fmt.Errorf("qbittorrent: transfer token contains unsupported characters")
		}
		form.Set("tags", transferTagPrefix+options.Transfer)
	}
	body, err := c.postForm(ctx, "/api/v2/torrents/add", form)
	if err != nil {
		return "", err
	}
	if strings.Contains(string(body), "Fails") {
		return "", fmt.Errorf("qbittorrent: add failed: %w", ports.ErrAdmissionRejected)
	}
	return "", nil
}

// SupportsExactTransferCorrelation reports that AddWithOptions tags requests
// and Statuses returns the echoed token. The native handle remains unknown
// until qBittorrent exposes the torrent in its info listing.
func (c *Client) SupportsExactTransferCorrelation() bool { return true }

func validTransferToken(token string) bool {
	if len(token) == 0 || len(token) > 128 {
		return false
	}
	for _, r := range token {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func transferFromTags(tags string) string {
	var found string
	for _, tag := range strings.Split(tags, ",") {
		tag = strings.TrimSpace(tag)
		if !strings.HasPrefix(tag, transferTagPrefix) {
			continue
		}
		candidate := strings.TrimPrefix(tag, transferTagPrefix)
		if !validTransferToken(candidate) || found != "" {
			return ""
		}
		found = candidate
	}
	return found
}

type torrentInfo struct {
	BytesCompleted *int64   `json:"completed"`
	ConnectedSeeds *int     `json:"num_seeds"`
	Availability   *float64 `json:"availability"`
	Hash           string   `json:"hash"`
	Name           string   `json:"name"`
	Progress       float64  `json:"progress"`
	State          string   `json:"state"`
	SavePath       string   `json:"save_path"`
	// content_path: file for single-file torrents, folder otherwise.
	ContentPath string `json:"content_path"`
	Tags        string `json:"tags"`
}

// Statuses implements ports.DownloadClient for our category.
func (c *Client) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	body, err := c.get(ctx, "/api/v2/torrents/info", url.Values{"category": {c.cfg.Category}})
	if err != nil {
		return nil, err
	}
	var infos []torrentInfo
	if err := json.Unmarshal(body, &infos); err != nil {
		return nil, fmt.Errorf("qbittorrent: bad torrents/info JSON: %w", err)
	}
	out := make([]ports.DownloadStatus, 0, len(infos))
	for _, t := range infos {
		s := ports.DownloadStatus{
			BytesCompleted: t.BytesCompleted, ConnectedSeeds: t.ConnectedSeeds, Availability: t.Availability, OperationalState: t.State,
			Handle:   ports.Handle(t.Hash),
			Name:     t.Name,
			Transfer: transferFromTags(t.Tags),
			Progress: t.Progress,
			SavePath: t.ContentPath,
		}
		if s.SavePath == "" {
			s.SavePath = t.SavePath
		}
		switch t.State {
		case "uploading", "stalledUP", "pausedUP", "queuedUP", "checkingUP", "forcedUP", "stoppedUP":
			s.State = ports.StateCompleted
		case "error", "missingFiles":
			s.State = ports.StateFailed
			s.Message = t.State
		case "queuedDL", "pausedDL", "stoppedDL":
			s.State = ports.StateQueued
		default: // downloading, stalledDL, metaDL, checkingDL, forcedDL…
			s.State = ports.StateDownloading
		}
		if t.Progress >= 1 {
			s.State = ports.StateCompleted
		}
		out = append(out, s)
	}
	return out, nil
}

// Remove implements ports.DownloadClient.
func (c *Client) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	form := url.Values{"hashes": {string(h)}, "deleteFiles": {fmt.Sprint(deleteData)}}
	_, err := c.postForm(ctx, "/api/v2/torrents/delete", form)
	return err
}

// Test implements ports.DownloadClient: login + version probe.
func (c *Client) Test(ctx context.Context) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	_, err := c.get(ctx, "/api/v2/app/version", nil)
	return err
}

// Inventory includes the adapter's unbounded live and historical listing.
func (c *Client) Inventory(ctx context.Context) ([]ports.DownloadStatus, bool, error) {
	rows, err := c.Statuses(ctx)
	return rows, err == nil, err
}
