package deluge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestLoginAddStatuses(t *testing.T) {
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "auth.login":
			logins++
			ok := req.Params[0] == "hunter2"
			_ = json.NewEncoder(w).Encode(map[string]any{"result": ok, "error": nil})
		case "core.add_torrent_url":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "hash9", "error": nil})
		case "core.get_torrents_status":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{
				"hash9": map[string]any{"name": "Thing", "progress": 100.0, "state": "Seeding", "save_path": "/dl"},
			}, "error": nil})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": nil})
		}
	}))
	defer srv.Close()

	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})
	h, err := c.Add(context.Background(), "http://x/t.torrent", "")
	if err != nil || h != "hash9" {
		t.Fatalf("add = %q err %v", h, err)
	}
	sts, err := c.Statuses(context.Background())
	if err != nil || len(sts) != 1 || sts[0].State != ports.StateCompleted || sts[0].SavePath != "/dl/Thing" {
		t.Fatalf("statuses = %+v err %v", sts, err)
	}
	if logins != 1 {
		t.Errorf("logins = %d, want 1 (session reuse)", logins)
	}

	bad := New(ports.ClientConfig{URL: srv.URL, Password: "wrong"})
	if err := bad.Test(context.Background()); err == nil {
		t.Error("wrong password should fail")
	}
}
