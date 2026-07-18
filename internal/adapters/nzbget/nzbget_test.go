package nzbget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/ports"
)

func TestAppendAndStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "nzb" || p != "get" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "append":
			_, _ = w.Write([]byte(`{"result": 42}`))
		case "listgroups":
			_, _ = w.Write([]byte(`{"result":[{"NZBID":42,"NZBName":"Queued.Thing","FileSizeMB":100,"RemainingSizeMB":25,"Status":"DOWNLOADING"}]}`))
		case "history":
			_, _ = w.Write([]byte(`{"result":[
				{"NZBID":41,"Name":"Done.Thing","Status":"SUCCESS/ALL","DestDir":"/complete/Done.Thing"},
				{"NZBID":40,"Name":"Broken.Thing","Status":"FAILURE/PAR","DestDir":""}
			]}`))
		case "version":
			_, _ = w.Write([]byte(`{"result":"21.1"}`))
		default:
			_, _ = w.Write([]byte(`{"result": true}`))
		}
	}))
	defer srv.Close()

	c := New(ports.ClientConfig{URL: srv.URL, Username: "nzb", Password: "get"})
	h, err := c.Add(context.Background(), "http://x/file.nzb", "monarr")
	if err != nil || h != "42" {
		t.Fatalf("append = %q err %v", h, err)
	}
	sts, err := c.Statuses(context.Background())
	if err != nil || len(sts) != 3 {
		t.Fatalf("statuses = %+v err %v", sts, err)
	}
	if sts[0].State != ports.StateDownloading || sts[0].Progress != 0.75 {
		t.Errorf("queued = %+v", sts[0])
	}
	if sts[1].State != ports.StateCompleted || sts[1].SavePath != "/complete/Done.Thing" {
		t.Errorf("done = %+v", sts[1])
	}
	if sts[2].State != ports.StateFailed {
		t.Errorf("failed = %+v", sts[2])
	}
	if err := c.Test(context.Background()); err != nil {
		t.Errorf("test: %v", err)
	}

	bad := New(ports.ClientConfig{URL: srv.URL, Username: "nzb", Password: "wrong"})
	if err := bad.Test(context.Background()); err == nil {
		t.Error("bad auth should fail")
	}
}

func TestBareHostURLNormalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"21.1"}`))
	}))
	defer srv.Close()

	// A user pasting "192.168.4.7:6789" instead of "http://…" must not get
	// `unsupported protocol scheme ""`.
	bare := strings.TrimPrefix(srv.URL, "http://")
	c := New(ports.ClientConfig{URL: bare})
	if err := c.Test(context.Background()); err != nil {
		t.Fatalf("bare host: %v", err)
	}
}
