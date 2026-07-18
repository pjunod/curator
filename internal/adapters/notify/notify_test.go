package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monarr-media/monarr/internal/ports"
)

func TestWebhookPostsNotification(t *testing.T) {
	var got ports.Notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	n := New(ports.NotifierConfig{Type: "webhook", Settings: map[string]string{"url": srv.URL}})
	err := n.Send(context.Background(), ports.Notification{
		Event: "import", Title: "Import completed", Body: "Some.Release",
		Fields: map[string]string{"files": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Event != "import" || got.Body != "Some.Release" || got.Fields["files"] != "2" {
		t.Errorf("payload = %+v", got)
	}
}

func TestDiscordEmbedShape(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(ports.NotifierConfig{Type: "discord", Settings: map[string]string{"url": srv.URL}})
	if err := n.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if raw["username"] != "Monarr" {
		t.Errorf("username = %v", raw["username"])
	}
	embeds, ok := raw["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds = %v", raw["embeds"])
	}
}

func TestPlexAndJellyfinRefresh(t *testing.T) {
	var plexPath, plexToken, jfPath, jfToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			plexPath, plexToken = r.URL.Path, r.URL.Query().Get("X-Plex-Token")
		case http.MethodPost:
			jfPath, jfToken = r.URL.Path, r.Header.Get("X-Emby-Token")
		}
	}))
	defer srv.Close()

	plex := New(ports.NotifierConfig{Type: "plex",
		Settings: map[string]string{"url": srv.URL, "token": "tok1"}})
	if err := plex.Send(context.Background(), ports.Notification{}); err != nil {
		t.Fatal(err)
	}
	if plexPath != "/library/sections/all/refresh" || plexToken != "tok1" {
		t.Errorf("plex call = %s token %s", plexPath, plexToken)
	}

	jf := New(ports.NotifierConfig{Type: "jellyfin",
		Settings: map[string]string{"url": srv.URL, "apiKey": "key1"}})
	if err := jf.Send(context.Background(), ports.Notification{}); err != nil {
		t.Fatal(err)
	}
	if jfPath != "/Library/Refresh" || jfToken != "key1" {
		t.Errorf("jellyfin call = %s token %s", jfPath, jfToken)
	}
}

func TestUnknownTypeFailsLoudly(t *testing.T) {
	n := New(ports.NotifierConfig{Type: "carrier-pigeon"})
	if err := n.Test(context.Background()); err == nil {
		t.Error("unknown type should error")
	}
}
