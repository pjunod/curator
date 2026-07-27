// Package transmission implements ports.DownloadClient against the
// Transmission RPC API (Phase 5), including the X-Transmission-Session-Id
// CSRF handshake.
package transmission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/adapters/httpx"
	"github.com/monarr-media/monarr/internal/ports"
)

// Client speaks Transmission RPC.
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client

	mu        sync.Mutex
	sessionID string
}

// New returns a Client.
func New(cfg ports.ClientConfig) *Client {
	cfg.URL = ports.NormalizeServiceURL(cfg.URL, ports.DefaultPortFor(cfg.Type))
	return &Client{cfg: cfg, http: httpx.NewClient(30 * time.Second)}
}

type rpcRequest struct {
	Method    string         `json:"method"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type rpcResponse struct {
	Result    string          `json:"result"`
	Arguments json.RawMessage `json:"arguments"`
}

func (c *Client) rpcURL() string {
	return strings.TrimRight(c.cfg.URL, "/") + "/transmission/rpc"
}

// call performs one RPC, transparently handling the 409 session handshake.
func (c *Client) call(ctx context.Context, method string, args map[string]any, out any) error {
	body, err := json.Marshal(rpcRequest{Method: method, Arguments: args})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.cfg.Username != "" {
			req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
		}
		c.mu.Lock()
		if c.sessionID != "" {
			req.Header.Set("X-Transmission-Session-Id", c.sessionID)
		}
		c.mu.Unlock()

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("transmission: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusConflict {
			c.mu.Lock()
			c.sessionID = resp.Header.Get("X-Transmission-Session-Id")
			c.mu.Unlock()
			continue // retry with the new session id
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("transmission: authentication rejected (401)")
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("transmission: unexpected status %d", resp.StatusCode)
		}
		var rr rpcResponse
		if err := json.Unmarshal(raw, &rr); err != nil {
			return fmt.Errorf("transmission: bad response: %w", err)
		}
		if rr.Result != "success" {
			return fmt.Errorf("transmission: %s", rr.Result)
		}
		if out != nil {
			return json.Unmarshal(rr.Arguments, out)
		}
		return nil
	}
	return fmt.Errorf("transmission: session handshake failed")
}

// Add implements ports.DownloadClient.
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	args := map[string]any{"filename": downloadURL}
	if category != "" {
		args["labels"] = []string{category}
	}
	var res struct {
		Added struct {
			Hash string `json:"hashString"`
		} `json:"torrent-added"`
		Duplicate struct {
			Hash string `json:"hashString"`
		} `json:"torrent-duplicate"`
	}
	if err := c.call(ctx, "torrent-add", args, &res); err != nil {
		return "", err
	}
	if res.Added.Hash != "" {
		return ports.Handle(res.Added.Hash), nil
	}
	return ports.Handle(res.Duplicate.Hash), nil
}

// Statuses implements ports.DownloadClient.
func (c *Client) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	var res struct {
		Torrents []struct {
			Hash        string  `json:"hashString"`
			Name        string  `json:"name"`
			PercentDone float64 `json:"percentDone"`
			Status      int     `json:"status"`
			DownloadDir string  `json:"downloadDir"`
			ErrorString string  `json:"errorString"`
		} `json:"torrents"`
	}
	err := c.call(ctx, "torrent-get", map[string]any{
		"fields": []string{"hashString", "name", "percentDone", "status", "downloadDir", "errorString"},
	}, &res)
	if err != nil {
		return nil, err
	}
	out := make([]ports.DownloadStatus, 0, len(res.Torrents))
	for _, t := range res.Torrents {
		st := ports.DownloadStatus{
			Handle: ports.Handle(t.Hash), Name: t.Name, Progress: t.PercentDone,
			SavePath: filepath.Join(t.DownloadDir, t.Name), Message: t.ErrorString,
		}
		switch {
		case t.ErrorString != "":
			st.State = ports.StateFailed
		case t.PercentDone >= 1:
			st.State = ports.StateCompleted
		case t.Status == 4: // downloading
			st.State = ports.StateDownloading
		default: // stopped/checking/queued/seeding pre-complete
			st.State = ports.StateQueued
		}
		out = append(out, st)
	}
	return out, nil
}

// Remove implements ports.DownloadClient.
func (c *Client) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	return c.call(ctx, "torrent-remove", map[string]any{
		"ids": []string{string(h)}, "delete-local-data": deleteData,
	}, nil)
}

// Test implements ports.DownloadClient.
func (c *Client) Test(ctx context.Context) error {
	return c.call(ctx, "session-get", nil, nil)
}
