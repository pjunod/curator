package transmission

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestSessionHandshakeAddAndStatuses(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			w.Header().Set("X-Transmission-Session-Id", "sess-1")
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "torrent-add":
			_, _ = w.Write([]byte(`{"result":"success","arguments":{"torrent-added":{"hashString":"abc123"}}}`))
		case "torrent-get":
			_, _ = w.Write([]byte(`{"result":"success","arguments":{"torrents":[
				{"hashString":"abc123","name":"My.Release","percentDone":1,"status":6,"downloadDir":"/dl","errorString":""},
				{"hashString":"def","name":"Failing","percentDone":0.4,"status":4,"downloadDir":"/dl","errorString":"tracker error"}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"result":"success","arguments":{}}`))
		}
	}))
	defer srv.Close()

	c := New(ports.ClientConfig{URL: srv.URL, Username: "u", Password: "p"})
	h, err := c.Add(context.Background(), "http://x/t.torrent", "monarr")
	if err != nil || h != "abc123" {
		t.Fatalf("add = %q err %v", h, err)
	}
	sts, err := c.Statuses(context.Background())
	if err != nil || len(sts) != 2 {
		t.Fatalf("statuses = %+v err %v", sts, err)
	}
	if sts[0].State != ports.StateCompleted || sts[0].SavePath != "/dl/My.Release" {
		t.Errorf("completed = %+v", sts[0])
	}
	if sts[1].State != ports.StateFailed || sts[1].Message != "tracker error" {
		t.Errorf("failed = %+v", sts[1])
	}
	if err := c.Test(context.Background()); err != nil {
		t.Errorf("test: %v", err)
	}
}
