// Package nzbget implements ports.DownloadClient against the NZBGet
// JSON-RPC API (Phase 5). Usenet protocol; credentials via basic auth.
package nzbget

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

// Client speaks NZBGet JSON-RPC at {url}/jsonrpc.
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client
	id   atomic.Int64
}

// New returns a Client.
func New(cfg ports.ClientConfig) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) rpc(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{
		"method": method, "params": params, "id": c.id.Add(1), "jsonrpc": "2.0",
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.URL, "/")+"/jsonrpc", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Username != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("nzbget: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("nzbget: authentication rejected (401)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nzbget: unexpected status %d", resp.StatusCode)
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("nzbget: bad response: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("nzbget: %s", rr.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(rr.Result, out)
	}
	return nil
}

// Add implements ports.DownloadClient: append-by-URL (NZBGet ≥ v13).
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	var id int64
	err := c.rpc(ctx, "append", []any{
		"",          // NZBFilename (derived server-side)
		downloadURL, // Content: URL form
		category,    // Category
		0,           // Priority
		false,       // AddToTop
		false,       // AddPaused
		"",          // DupeKey
		0,           // DupeScore
		"SCORE",     // DupeMode
	}, &id)
	if err != nil {
		return "", err
	}
	if id <= 0 {
		return "", fmt.Errorf("nzbget: append rejected (id %d)", id)
	}
	return ports.Handle(strconv.FormatInt(id, 10)), nil
}

// Statuses implements ports.DownloadClient: queue (listgroups) + history.
func (c *Client) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	var groups []struct {
		NZBID           int64  `json:"NZBID"`
		NZBName         string `json:"NZBName"`
		FileSizeMB      int64  `json:"FileSizeMB"`
		RemainingSizeMB int64  `json:"RemainingSizeMB"`
		Status          string `json:"Status"`
	}
	if err := c.rpc(ctx, "listgroups", []any{0}, &groups); err != nil {
		return nil, err
	}
	out := make([]ports.DownloadStatus, 0, len(groups))
	for _, g := range groups {
		progress := 0.0
		if g.FileSizeMB > 0 {
			progress = float64(g.FileSizeMB-g.RemainingSizeMB) / float64(g.FileSizeMB)
		}
		st := ports.DownloadStatus{
			Handle: ports.Handle(strconv.FormatInt(g.NZBID, 10)),
			Name:   g.NZBName, Progress: progress, State: ports.StateDownloading,
		}
		if strings.HasPrefix(g.Status, "QUEUED") || strings.HasPrefix(g.Status, "PAUSED") {
			st.State = ports.StateQueued
		}
		out = append(out, st)
	}

	var hist []struct {
		NZBID   int64  `json:"NZBID"`
		Name    string `json:"Name"`
		Status  string `json:"Status"` // "SUCCESS/ALL", "FAILURE/…", …
		DestDir string `json:"DestDir"`
	}
	if err := c.rpc(ctx, "history", []any{false}, &hist); err != nil {
		return nil, err
	}
	for _, h := range hist {
		st := ports.DownloadStatus{
			Handle: ports.Handle(strconv.FormatInt(h.NZBID, 10)),
			Name:   h.Name, Progress: 1, SavePath: h.DestDir,
		}
		if strings.HasPrefix(h.Status, "SUCCESS") {
			st.State = ports.StateCompleted
		} else {
			st.State = ports.StateFailed
			st.Message = h.Status
		}
		out = append(out, st)
	}
	return out, nil
}

// Remove implements ports.DownloadClient.
func (c *Client) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	id, _ := strconv.ParseInt(string(h), 10, 64)
	var ok bool
	// Queue first; fall through to history delete if not queued.
	if err := c.rpc(ctx, "editqueue", []any{"GroupDelete", 0, "", []int64{id}}, &ok); err == nil && ok {
		return nil
	}
	return c.rpc(ctx, "editqueue", []any{"HistoryDelete", 0, "", []int64{id}}, &ok)
}

// Test implements ports.DownloadClient.
func (c *Client) Test(ctx context.Context) error {
	var version string
	return c.rpc(ctx, "version", nil, &version)
}
