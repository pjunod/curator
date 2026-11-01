package nzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pjunod/monarr/internal/ports"
)

const NativeTorrentCapability = "native-torrent-deliveries-v1"

// NewTorrent selects Runner's native managed torrent acquisition API. New keeps
// its existing Usenet meaning; no qBittorrent compatibility fallback is used.
func NewTorrent(cfg ports.ClientConfig) *Client {
	cfg.Type = "runner-torrent"
	client := New(cfg)
	client.nativeTorrent = true
	return client
}
func (c *Client) requireNativeTorrentCapabilities(status statusDto) error {
	if !c.nativeTorrent {
		return nil
	}
	native, deliveries := false, false
	for _, capability := range status.Capabilities {
		native = native || capability == NativeTorrentCapability
		deliveries = deliveries || capability == "deliveries-v1"
	}
	if !native || !deliveries {
		return fmt.Errorf("Runner native torrent acquisition is unsupported by this server: enable its native torrent service with %s and deliveries-v1, or choose a supported download client", NativeTorrentCapability)
	}
	return nil
}
func (c *Client) addNativeTorrent(ctx context.Context, source, category string, options ports.AddOptions) (ports.Handle, error) {
	status, err := c.status(ctx)
	if err != nil {
		return "", err
	}
	if err := c.requireNativeTorrentCapabilities(status); err != nil {
		return "", err
	}
	if strings.TrimSpace(options.Transfer) == "" || !options.DeliveryExpected {
		return "", fmt.Errorf("Runner native torrent acquisition requires a durable request token and negotiated Delivery admission")
	}
	uri, err := url.Parse(source)
	if err != nil {
		return "", fmt.Errorf("Runner native torrent source: %w", err)
	}
	sourceType := "url"
	switch strings.ToLower(uri.Scheme) {
	case "magnet":
		sourceType = "magnet"
	case "http", "https":
		if uri.Host == "" {
			return "", fmt.Errorf("Runner native torrent source requires an absolute HTTP URL")
		}
	default:
		return "", fmt.Errorf("Runner native torrent source must be a magnet or HTTP torrent URL")
	}
	params := map[string]string{TransferParam: options.Transfer, DeliveryParam: "1"}
	if options.ManualAdditional {
		params[ManualAdditionalParam] = "1"
	}
	payload := struct {
		Source struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		} `json:"source"`
		Category string            `json:"category"`
		Priority int               `json:"priority"`
		Paused   bool              `json:"paused"`
		Params   map[string]string `json:"params"`
	}{Category: category, Priority: options.Priority, Params: params}
	payload.Source.Type, payload.Source.URI = sourceType, source
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := c.doBody(ctx, http.MethodPost, "/api/v1/jobs", strings.NewReader(string(raw)), &result); err != nil {
		return "", err
	}
	if result.ID <= 0 {
		return "", fmt.Errorf("Runner native torrent admission omitted its acquisition job ID")
	}
	return ports.Handle(strconv.FormatInt(result.ID, 10)), nil
}

// Explicit native rows never enter Usenet's legacy title fallback. Missing old
// role fields retain their existing Usenet interpretation unless kind proves
// the row is a torrent.
func (c *Client) acceptsTorrentRole(kind, role string) bool {
	if c.nativeTorrent {
		return role == "acquisition"
	}
	return kind != "torrent" && role == ""
}
