package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/ports"
)

// plurxDevices is GET /api/v1/playback/devices (plurx docs/API.md).
type plurxDevices struct {
	Devices []struct {
		ID         string  `json:"id"`
		User       string  `json:"user"`
		Device     *string `json:"device"`
		Client     *string `json:"client"`
		LastSeenAt int64   `json:"last_seen_at"`
		HDR        struct {
			DolbyVision bool  `json:"dolby_vision"`
			FEL         bool  `json:"fel"`
			Profile5    bool  `json:"profile5"`
			HDR10       bool  `json:"hdr10"`
			HLG         bool  `json:"hlg"`
			HDR10Plus   *bool `json:"hdr10plus"`
		} `json:"hdr"`
	} `json:"devices"`
}

// PlaybackDevices implements ports.PlaybackDeviceSource: the household's
// signed-in plurx devices and what each decodes. It needs the key to hold
// plurx's `playback:read` scope as well as `scan:trigger`.
func (p *Plurx) PlaybackDevices(ctx context.Context) ([]ports.PlaybackDevice, error) {
	if p.URL == "" || p.APIKey == "" {
		return nil, fmt.Errorf("plurx: url and apiKey required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(p.URL, "/")+"/api/v1/playback/devices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach plurx: %s", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the plurx key lacks the playback:read scope (403); add it to the key in plurx Settings → API keys")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("this plurx does not report playback devices yet (404); it needs a release with GET /api/v1/playback/devices")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("plurx returned %d: %s", resp.StatusCode, summarize(raw))
	}
	var body plurxDevices
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("plurx devices: %w", err)
	}
	out := make([]ports.PlaybackDevice, 0, len(body.Devices))
	for _, d := range body.Devices {
		dev := ports.PlaybackDevice{ID: d.ID, User: d.User, LastSeen: d.LastSeenAt}
		if d.Device != nil {
			dev.Device = *d.Device
		}
		if d.Client != nil {
			dev.Client = *d.Client
		}
		dev.Caps = hdr.DeviceCaps{
			Name: dev.Device, DolbyVision: d.HDR.DolbyVision, FEL: d.HDR.FEL,
			Profile5: d.HDR.Profile5, HDR10: d.HDR.HDR10, HLG: d.HDR.HLG,
			// Unknown is read as "renders the static HDR10 base", which is
			// what a device that never claimed HDR10+ does.
			HDR10Plus: d.HDR.HDR10Plus != nil && *d.HDR.HDR10Plus,
		}
		out = append(out, dev)
	}
	return out, nil
}
