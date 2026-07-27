// Package sabnzbd implements ports.DownloadClient against the SABnzbd API.
package sabnzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/adapters/httpx"
	"github.com/monarr-media/monarr/internal/ports"
)

// Client is a DownloadClient for one SABnzbd instance. cfg.Password holds
// the API key (SAB has no username concept for API access).
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client
}

var _ ports.DownloadClient = (*Client)(nil)

// New returns a Client.
func New(cfg ports.ClientConfig) *Client {
	cfg.URL = ports.NormalizeServiceURL(cfg.URL, ports.DefaultPortFor(cfg.Type))
	return &Client{cfg: cfg, http: httpx.NewClient(30 * time.Second)}
}

func (c *Client) call(ctx context.Context, params url.Values, out any) error {
	params.Set("apikey", c.cfg.Password)
	params.Set("output", "json")
	full := strings.TrimRight(c.cfg.URL, "/") + "/api?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sabnzbd: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sabnzbd: status %d", resp.StatusCode)
	}
	var apiErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return fmt.Errorf("sabnzbd: %s", apiErr.Error)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("sabnzbd: bad JSON: %w", err)
		}
	}
	return nil
}

// Add implements ports.DownloadClient via mode=addurl.
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	var resp struct {
		Status bool     `json:"status"`
		IDs    []string `json:"nzo_ids"`
	}
	params := url.Values{"mode": {"addurl"}, "name": {downloadURL}, "cat": {category}}
	if err := c.call(ctx, params, &resp); err != nil {
		return "", err
	}
	if !resp.Status || len(resp.IDs) == 0 {
		return "", fmt.Errorf("sabnzbd: addurl rejected")
	}
	return ports.Handle(resp.IDs[0]), nil
}

// Statuses implements ports.DownloadClient: live queue + recent history.
func (c *Client) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	var out []ports.DownloadStatus

	var q struct {
		Queue struct {
			Slots []struct {
				ID         string `json:"nzo_id"`
				Filename   string `json:"filename"`
				Percentage string `json:"percentage"`
				Status     string `json:"status"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := c.call(ctx, url.Values{"mode": {"queue"}}, &q); err != nil {
		return nil, err
	}
	for _, s := range q.Queue.Slots {
		pct, _ := strconv.ParseFloat(s.Percentage, 64)
		st := ports.DownloadStatus{
			Handle: ports.Handle(s.ID), Name: s.Filename, Progress: pct / 100,
		}
		switch strings.ToLower(s.Status) {
		case "paused", "queued":
			st.State = ports.StateQueued
		default:
			st.State = ports.StateDownloading
		}
		out = append(out, st)
	}

	var h struct {
		History struct {
			Slots []struct {
				ID      string `json:"nzo_id"`
				Name    string `json:"name"`
				Status  string `json:"status"`
				Storage string `json:"storage"`
				Fail    string `json:"fail_message"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := c.call(ctx, url.Values{"mode": {"history"}, "limit": {"50"}}, &h); err != nil {
		return nil, err
	}
	for _, s := range h.History.Slots {
		st := ports.DownloadStatus{
			Handle: ports.Handle(s.ID), Name: s.Name, Progress: 1, SavePath: s.Storage,
		}
		switch strings.ToLower(s.Status) {
		case "completed":
			st.State = ports.StateCompleted
		case "failed":
			st.State = ports.StateFailed
			st.Message = s.Fail
		default:
			st.State = ports.StateDownloading // verifying/repairing/extracting
		}
		out = append(out, st)
	}
	return out, nil
}

// Remove implements ports.DownloadClient (queue and history).
func (c *Client) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	del := "0"
	if deleteData {
		del = "1"
	}
	_ = c.call(ctx, url.Values{"mode": {"queue"}, "name": {"delete"}, "value": {string(h)}, "del_files": {del}}, nil)
	return c.call(ctx, url.Values{"mode": {"history"}, "name": {"delete"}, "value": {string(h)}, "del_files": {del}}, nil)
}

// Test implements ports.DownloadClient via mode=version.
func (c *Client) Test(ctx context.Context) error {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.call(ctx, url.Values{"mode": {"version"}}, &v); err != nil {
		return err
	}
	if v.Version == "" {
		return fmt.Errorf("sabnzbd: no version in response")
	}
	return nil
}
