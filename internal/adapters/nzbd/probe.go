package nzbd

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/pjunod/monarr/internal/ports"
)

// ProbeCapability is the /api/v1/status capability Runner advertises when it
// serves POST /api/v1/probe (Runner docs/INTEGRATION.md, ADR 0031 §6).
const ProbeCapability = "release-probe-v1"

// SupportsReleaseProbe implements ports.ReleaseProber.
func (c *Client) SupportsReleaseProbe(ctx context.Context) (bool, error) {
	if c.nativeTorrent {
		return false, nil // a torrent client has no NNTP articles to peek at
	}
	st, err := c.status(ctx)
	if err != nil {
		return false, err
	}
	for _, capability := range st.Capabilities {
		if capability == ProbeCapability {
			return true, nil
		}
	}
	return false, nil
}

// probeResponse is Runner's answer to POST /api/v1/probe.
type probeResponse struct {
	Status          string   `json:"status"` // "ok" | "unavailable"
	Reason          string   `json:"reason"`
	Detail          string   `json:"detail"`
	File            string   `json:"file"`
	FileSize        int64    `json:"file_size"`
	Path            []string `json:"path"`
	Articles        int      `json:"articles"`
	ArticlesMissing int      `json:"articles_missing"`
	Head            string   `json:"head"` // base64

	maxBytes int64
}

// responseLimit is the head base64-encoded (4/3) plus room for the fields.
func (r *probeResponse) responseLimit() int64 { return r.maxBytes/3*4 + 8 + 64<<10 }

// ProbeRelease implements ports.ReleaseProber. Runner fetches the NZB from the
// URL itself, exactly as it does for an add, so Curator never handles NZB
// bytes and the indexer sees the same request either way.
func (c *Client) ProbeRelease(ctx context.Context, downloadURL string, maxBytes int64) (ports.ReleaseHead, error) {
	q := url.Values{}
	q.Set("url", downloadURL)
	q.Set("bytes", strconv.FormatInt(maxBytes, 10))
	out := probeResponse{maxBytes: maxBytes}
	if err := c.do(ctx, http.MethodPost, "/api/v1/probe?"+q.Encode(), &out); err != nil {
		return ports.ReleaseHead{}, err
	}
	if out.Status != "ok" {
		reason := out.Reason
		if reason == "" {
			reason = ports.ProbeUnreadable
		}
		return ports.ReleaseHead{}, &ports.ProbeRefusal{Reason: reason, Detail: out.Detail}
	}
	head, err := base64.StdEncoding.DecodeString(out.Head)
	if err != nil {
		return ports.ReleaseHead{}, fmt.Errorf("nzbd: probe head is not base64: %w", err)
	}
	if int64(len(head)) > maxBytes {
		head = head[:maxBytes]
	}
	return ports.ReleaseHead{
		File: out.File, FileSize: out.FileSize, Path: out.Path,
		Articles: out.Articles, Missing: out.ArticlesMissing, Bytes: head,
	}, nil
}
