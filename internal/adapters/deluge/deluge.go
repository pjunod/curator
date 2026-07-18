// Package deluge implements ports.DownloadClient against the Deluge Web
// JSON-RPC API (Phase 5): cookie login, core.* methods over /json.
package deluge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

// Client speaks the Deluge web API. Password lives in cfg.Password.
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client
	id   atomic.Int64

	mu       sync.Mutex
	loggedIn bool
}

// New returns a Client.
func New(cfg ports.ClientConfig) *Client {
	cfg.URL = ports.NormalizeURL(cfg.URL)
	jar, _ := cookiejar.New(nil)
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second, Jar: jar}}
}

type rpcError struct {
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (c *Client) rpc(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{
		"method": method, "params": params, "id": c.id.Add(1),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.URL, "/")+"/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deluge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deluge: unexpected status %d", resp.StatusCode)
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("deluge: bad response: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("deluge: %s", rr.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(rr.Result, out)
	}
	return nil
}

// login authenticates once per client lifetime (cookie-jar backed).
func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	var ok bool
	if err := c.rpc(ctx, "auth.login", []any{c.cfg.Password}, &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("deluge: password rejected")
	}
	c.loggedIn = true
	return nil
}

// Add implements ports.DownloadClient.
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	if err := c.login(ctx); err != nil {
		return "", err
	}
	opts := map[string]any{}
	var hash string
	if err := c.rpc(ctx, "core.add_torrent_url", []any{downloadURL, opts}, &hash); err != nil {
		return "", err
	}
	if category != "" {
		_ = c.rpc(ctx, "label.set_torrent", []any{hash, category}, nil) // plugin may be absent
	}
	return ports.Handle(hash), nil
}

type torrentStatus struct {
	Name     string  `json:"name"`
	Progress float64 `json:"progress"` // 0..100
	State    string  `json:"state"`
	SavePath string  `json:"save_path"`
	Message  string  `json:"message"`
}

// Statuses implements ports.DownloadClient.
func (c *Client) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	var res map[string]torrentStatus
	err := c.rpc(ctx, "core.get_torrents_status",
		[]any{map[string]any{}, []string{"name", "progress", "state", "save_path", "message"}}, &res)
	if err != nil {
		return nil, err
	}
	out := make([]ports.DownloadStatus, 0, len(res))
	for hash, t := range res {
		st := ports.DownloadStatus{
			Handle: ports.Handle(hash), Name: t.Name, Progress: t.Progress / 100,
			SavePath: filepath.Join(t.SavePath, t.Name), Message: t.Message,
		}
		switch t.State {
		case "Error":
			st.State = ports.StateFailed
		case "Seeding":
			st.State = ports.StateCompleted
		case "Downloading":
			if t.Progress >= 100 {
				st.State = ports.StateCompleted
			} else {
				st.State = ports.StateDownloading
			}
		default: // Queued, Paused, Checking, Allocating…
			if t.Progress >= 100 {
				st.State = ports.StateCompleted
			} else {
				st.State = ports.StateQueued
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// Remove implements ports.DownloadClient.
func (c *Client) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	var ok bool
	return c.rpc(ctx, "core.remove_torrent", []any{string(h), deleteData}, &ok)
}

// Test implements ports.DownloadClient.
func (c *Client) Test(ctx context.Context) error {
	c.mu.Lock()
	c.loggedIn = false // force a fresh login
	c.mu.Unlock()
	return c.login(ctx)
}
