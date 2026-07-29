package qbittorrent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func fakeQbit(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	var adds int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("username") != "admin" || r.Form.Get("password") != "secret" {
			w.Write([]byte("Fails."))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session1", Path: "/"})
		w.Write([]byte("Ok."))
	})
	auth := func(r *http.Request) bool {
		c, err := r.Cookie("SID")
		return err == nil && c.Value == "session1"
	}
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("v5.0.0"))
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		adds++
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("category") != "monarr" {
			w.Write([]byte("[]"))
			return
		}
		w.Write([]byte(`[
		 {"hash":"aaa","name":"Test.Show.S01E01.1080p","progress":0.42,"state":"downloading","save_path":"/dl","content_path":"/dl/Test.Show.S01E01.1080p"},
		 {"hash":"bbb","name":"Done.Movie.2024.1080p","progress":1,"state":"stalledUP","save_path":"/dl","content_path":"/dl/Done.Movie.2024.1080p.mkv"},
		 {"hash":"ccc","name":"Broken","progress":0.1,"state":"error","save_path":"/dl","content_path":""}
		]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &adds
}

func TestQbitFlow(t *testing.T) {
	srv, adds := fakeQbit(t)
	c := New(ports.ClientConfig{Type: "qbittorrent", URL: srv.URL,
		Username: "admin", Password: "secret", Category: "monarr"})
	ctx := context.Background()

	if err := c.Test(ctx); err != nil {
		t.Fatalf("test: %v", err)
	}
	if _, err := c.Add(ctx, "magnet:?xt=urn:btih:abc", "monarr"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if *adds != 1 {
		t.Error("add should hit the API once")
	}

	sts, err := c.Statuses(ctx)
	if err != nil || len(sts) != 3 {
		t.Fatalf("statuses = %v err %v", sts, err)
	}
	byHandle := map[ports.Handle]ports.DownloadStatus{}
	for _, s := range sts {
		byHandle[s.Handle] = s
	}
	if s := byHandle["aaa"]; s.State != ports.StateDownloading || s.Progress != 0.42 {
		t.Errorf("aaa = %+v", s)
	}
	if s := byHandle["bbb"]; s.State != ports.StateCompleted || !strings.HasSuffix(s.SavePath, ".mkv") {
		t.Errorf("bbb = %+v", s)
	}
	if s := byHandle["ccc"]; s.State != ports.StateFailed {
		t.Errorf("ccc = %+v", s)
	}
}

func TestQbitBadLogin(t *testing.T) {
	srv, _ := fakeQbit(t)
	c := New(ports.ClientConfig{URL: srv.URL, Username: "admin", Password: "wrong"})
	if err := c.Test(context.Background()); err == nil {
		t.Error("bad login should fail")
	}
}
