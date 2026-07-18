// Package notify implements ports.Notifier for the Phase 3 notification
// targets: generic webhooks, Discord, and the Plex/Jellyfin library-refresh
// pokes. All four are small HTTP calls; one package keeps the zoo in a
// single cage.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// New returns the Notifier for a stored config. Unknown types return a
// notifier whose calls fail loudly (config rot should be visible, not
// silent).
func New(cfg ports.NotifierConfig) ports.Notifier {
	u := ports.NormalizeURL(cfg.Settings["url"])
	switch cfg.Type {
	case "webhook":
		return &Webhook{URL: u}
	case "discord":
		return &Discord{URL: u}
	case "plex":
		return &Plex{URL: u, Token: cfg.Settings["token"]}
	case "jellyfin":
		return &Jellyfin{URL: u, APIKey: cfg.Settings["apiKey"]}
	}
	return errNotifier{cfg.Type}
}

type errNotifier struct{ kind string }

func (e errNotifier) Send(context.Context, ports.Notification) error {
	return fmt.Errorf("unknown notifier type %q", e.kind)
}
func (e errNotifier) Test(context.Context) error {
	return fmt.Errorf("unknown notifier type %q", e.kind)
}

func post(ctx context.Context, urlStr string, body any, header http.Header) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: %s returned %d", req.URL.Host, resp.StatusCode)
	}
	return nil
}

// ---- webhook ----

// Webhook POSTs the raw notification as JSON — the integration escape hatch.
type Webhook struct{ URL string }

// Send implements ports.Notifier.
func (w *Webhook) Send(ctx context.Context, n ports.Notification) error {
	if w.URL == "" {
		return fmt.Errorf("webhook: url not configured")
	}
	return post(ctx, w.URL, n, nil)
}

// Test implements ports.Notifier.
func (w *Webhook) Test(ctx context.Context) error {
	return w.Send(ctx, ports.Notification{
		Event: "test", Title: "Monarr", Body: "Test notification — configuration works.",
	})
}

// ---- discord ----

// Discord posts an embed to a Discord webhook URL.
type Discord struct{ URL string }

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// Send implements ports.Notifier.
func (d *Discord) Send(ctx context.Context, n ports.Notification) error {
	if d.URL == "" {
		return fmt.Errorf("discord: webhook url not configured")
	}
	embed := discordEmbed{Title: n.Title, Description: n.Body}
	for k, v := range n.Fields {
		embed.Fields = append(embed.Fields, discordEmbedField{Name: k, Value: v, Inline: true})
	}
	return post(ctx, d.URL, map[string]any{
		"username": "Monarr",
		"embeds":   []discordEmbed{embed},
	}, nil)
}

// Test implements ports.Notifier.
func (d *Discord) Test(ctx context.Context) error {
	return d.Send(ctx, ports.Notification{
		Event: "test", Title: "Monarr", Body: "Test notification — configuration works.",
	})
}

// ---- plex ----

// Plex pokes a Plex server to rescan its libraries after imports.
type Plex struct {
	URL   string
	Token string
}

func (p *Plex) refreshURL() (string, error) {
	if p.URL == "" || p.Token == "" {
		return "", fmt.Errorf("plex: url and token required")
	}
	u := strings.TrimRight(p.URL, "/") + "/library/sections/all/refresh"
	return u + "?X-Plex-Token=" + url.QueryEscape(p.Token), nil
}

// Send implements ports.Notifier: any event triggers a library refresh
// (the dispatcher only routes import events here by default).
func (p *Plex) Send(ctx context.Context, _ ports.Notification) error {
	target, err := p.refreshURL()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("plex: refresh returned %d", resp.StatusCode)
	}
	return nil
}

// Test implements ports.Notifier.
func (p *Plex) Test(ctx context.Context) error { return p.Send(ctx, ports.Notification{}) }

// ---- jellyfin ----

// Jellyfin pokes a Jellyfin/Emby server to rescan its libraries.
type Jellyfin struct {
	URL    string
	APIKey string
}

// Send implements ports.Notifier.
func (j *Jellyfin) Send(ctx context.Context, _ ports.Notification) error {
	if j.URL == "" || j.APIKey == "" {
		return fmt.Errorf("jellyfin: url and apiKey required")
	}
	target := strings.TrimRight(j.URL, "/") + "/Library/Refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", j.APIKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin: refresh returned %d", resp.StatusCode)
	}
	return nil
}

// Test implements ports.Notifier.
func (j *Jellyfin) Test(ctx context.Context) error { return j.Send(ctx, ports.Notification{}) }
