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

	"github.com/monarr-media/monarr/internal/ports"
)

// Client is a DownloadClient for one qBittorrent instance.
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client

	mu       sync.Mutex
	loggedIn bool
}

var _ ports.DownloadClient = (*Client)(nil)

// New returns a Client; login happens lazily on first use.
func New(cfg ports.ClientConfig) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second, Jar: jar}}
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

// Add implements ports.DownloadClient: hands the torrent URL/magnet to the
// client under our category. qBittorrent identifies torrents by hash, which
// we discover on the next Statuses poll; Add returns an empty handle and
// the tracker reconciles by category+name.
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	form := url.Values{"urls": {downloadURL}, "category": {category}}
	body, err := c.postForm(ctx, "/api/v2/torrents/add", form)
	if err != nil {
		return "", err
	}
	if strings.Contains(string(body), "Fails") {
		return "", fmt.Errorf("qbittorrent: add failed")
	}
	return "", nil
}

type torrentInfo struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	Progress float64 `json:"progress"`
	State    string  `json:"state"`
	SavePath string  `json:"save_path"`
	// content_path: file for single-file torrents, folder otherwise.
	ContentPath string `json:"content_path"`
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
			Handle:   ports.Handle(t.Hash),
			Name:     t.Name,
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
