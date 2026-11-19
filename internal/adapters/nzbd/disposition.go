package nzbd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/pjunod/monarr/internal/ports"
)

func (c *Client) DisposeJob(ctx context.Context, jobID string, body ports.RunnerDispositionRequest) (ports.RunnerDisposition, error) {
	var out ports.RunnerDisposition
	var status struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/status", &status); err != nil {
		return out, err
	}
	capable := false
	for _, v := range status.Capabilities {
		capable = capable || v == "job-disposition-v1"
	}
	if !capable {
		return out, fmt.Errorf("update Runner to support verified job deletion")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.URL, "/")+"/api/v1/jobs/"+url.PathEscape(jobID)+"/disposition", bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nzbd-Client", clientHeader())
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, apiError(resp, string(b))
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return out, err
	}
	if out.JobID != jobID || out.RequestID != body.RequestID || out.Transfer != body.Transfer || out.RunnerInstanceID != body.RunnerInstanceID || out.Generation != body.Generation {
		return out, fmt.Errorf("Runner deletion response identity changed")
	}
	if out.State != "pending" && out.State != "blocked" && out.State != "verified" {
		return out, fmt.Errorf("Runner deletion returned an unknown state")
	}
	return out, nil
}
