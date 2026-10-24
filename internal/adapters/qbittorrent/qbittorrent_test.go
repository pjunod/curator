package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestAddExplicitClientFailureIsDefiniteAdmissionRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			_, _ = w.Write([]byte("Fails."))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := New(ports.ClientConfig{Type: "qbittorrent", URL: srv.URL, Username: "user", Password: "pass"})
	handle, err := client.Add(context.Background(), "https://indexer.invalid/release.torrent", "monarr")
	if handle != "" || !errors.Is(err, ports.ErrAdmissionRejected) {
		t.Fatalf("Add() = %q, %v; want empty handle and definite rejection", handle, err)
	}
}

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
	inventory, complete, inventoryErr := c.Inventory(context.Background())
	if inventoryErr != nil || !complete || len(inventory) != len(sts) {
		t.Fatalf("complete inventory = %+v %v %v", inventory, complete, inventoryErr)
	}
	cancelled, cancelInventory := context.WithCancel(context.Background())
	cancelInventory()
	_, complete, inventoryErr = c.Inventory(cancelled)
	if inventoryErr == nil || complete {
		t.Fatal("outage asserted complete absence evidence")
	}

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

func TestQbitExactTransferTagRoundTripAndDuplicateAmbiguity(t *testing.T) {
	type submitted struct {
		URL      string
		Category string
		Rename   string
		Tag      string
	}
	var adds []submitted
	byURL := map[string]torrentInfo{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "tag-session", Path: "/"})
		_, _ = w.Write([]byte("Ok."))
	})
	auth := func(r *http.Request) bool {
		cookie, err := r.Cookie("SID")
		return err == nil && cookie.Value == "tag-session"
	}
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		request := submitted{URL: r.Form.Get("urls"), Category: r.Form.Get("category"), Rename: r.Form.Get("rename"), Tag: r.Form.Get("tags")}
		adds = append(adds, request)
		// Model the duplicate-add case conservatively: qBittorrent confirms the
		// request but leaves the existing torrent's original tags unchanged.
		// The caller must keep the second request unresolved, never match by name.
		if _, exists := byURL[request.URL]; !exists {
			byURL[request.URL] = torrentInfo{Hash: "hash-" + string(rune('a'+len(byURL))), Name: "Same title", Progress: 0.1, State: "downloading", Tags: request.Tag}
		}
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		rows := make([]torrentInfo, 0, len(byURL)+1)
		for _, row := range byURL {
			rows = append(rows, row)
		}
		rows = append(rows, torrentInfo{Hash: "untagged", Name: "Same title", Progress: 0.2, State: "downloading"})
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(ports.ClientConfig{Type: "qbittorrent", URL: srv.URL, Username: "admin", Password: "secret", Category: "monarr"})
	if !c.SupportsExactTransferCorrelation() {
		t.Fatal("qBittorrent should advertise the supported tagged correlation path")
	}

	firstURL := "magnet:?xt=urn:btih:shared"
	secondURL := "magnet:?xt=urn:btih:other"
	thirdURL := "magnet:?xt=urn:btih:opaque-transfer"
	for _, request := range []struct {
		url, transfer string
	}{{firstURL, "t-42-a3f9c1"}, {secondURL, "t-43-7b91de"}, {firstURL, "t-44-c410af"}, {thirdURL, "t-45-0123456789abcdef0123456789abcdef"}} {
		handle, err := c.AddWithOptions(context.Background(), request.url, "monarr", ports.AddOptions{Name: "Same title", Transfer: request.transfer})
		if err != nil {
			t.Fatalf("add %s: %v", request.transfer, err)
		}
		if handle != "" {
			t.Fatalf("handle = %q, qBittorrent add response has no native hash", handle)
		}
	}
	if len(adds) != 4 {
		t.Fatalf("add requests = %d, want 4", len(adds))
	}
	for i, transfer := range []string{"t-42-a3f9c1", "t-43-7b91de", "t-44-c410af", "t-45-0123456789abcdef0123456789abcdef"} {
		if adds[i].Category != "monarr" || adds[i].Rename != "Same title" || adds[i].Tag != transferTagPrefix+transfer {
			t.Errorf("add %d form = %+v", i, adds[i])
		}
	}

	statuses, err := c.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byTransfer := map[string]ports.DownloadStatus{}
	for _, status := range statuses {
		if status.Transfer != "" {
			byTransfer[status.Transfer] = status
		}
		if status.Name == "Same title" && status.Handle == "untagged" && status.Transfer != "" {
			t.Fatalf("untagged same-title torrent was correlated: %+v", status)
		}
	}
	if byTransfer["t-42-a3f9c1"].Handle != "hash-a" || byTransfer["t-43-7b91de"].Handle != "hash-b" || byTransfer["t-45-0123456789abcdef0123456789abcdef"].Handle != "hash-c" {
		t.Fatalf("exact tagged statuses = %+v", byTransfer)
	}
	if _, ok := byTransfer["t-44-c410af"]; ok {
		t.Fatal("duplicate add without an echoed unique tag must remain unbound")
	}
}

func TestQbitTransferTagParserFailsClosed(t *testing.T) {
	for _, test := range []struct{ tags, want string }{
		{transferTagPrefix + "t-1-abcdef", "t-1-abcdef"},
		{transferTagPrefix + "t-45-0123456789abcdef0123456789abcdef", "t-45-0123456789abcdef0123456789abcdef"},
		{"other," + transferTagPrefix + "t-2-abcdef", "t-2-abcdef"},
		{transferTagPrefix + "t-1-abcdef," + transferTagPrefix + "t-2-abcdef", ""},
		{transferTagPrefix + "bad.token", ""},
		{"other-tag", ""},
	} {
		if got := transferFromTags(test.tags); got != test.want {
			t.Errorf("transferFromTags(%q) = %q, want %q", test.tags, got, test.want)
		}
	}
	if validTransferToken("t-1,untrusted") {
		t.Fatal("comma-separated tags must not permit token injection")
	}
}
