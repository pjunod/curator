package sabnzbd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func fakeSab(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("apikey") != "sabkey" {
			w.Write([]byte(`{"error":"API Key Incorrect"}`))
			return
		}
		switch q.Get("mode") {
		case "version":
			w.Write([]byte(`{"version":"4.3.0"}`))
		case "addurl":
			w.Write([]byte(`{"status":true,"nzo_ids":["SABnzbd_nzo_x1"]}`))
		case "queue":
			w.Write([]byte(`{"queue":{"slots":[
			 {"nzo_id":"SABnzbd_nzo_x1","filename":"Test.Show.S01E01","percentage":"55","status":"Downloading"}
			]}}`))
		case "history":
			w.Write([]byte(`{"history":{"slots":[
			 {"nzo_id":"SABnzbd_nzo_x2","name":"Done.Movie.2024","status":"Completed","storage":"/complete/Done.Movie.2024"},
			 {"nzo_id":"SABnzbd_nzo_x3","name":"Bad.One","status":"Failed","fail_message":"crc error"}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSabFlow(t *testing.T) {
	srv := fakeSab(t)
	c := New(ports.ClientConfig{Type: "sabnzbd", URL: srv.URL, Password: "sabkey", Category: "monarr"})
	ctx := context.Background()

	if err := c.Test(ctx); err != nil {
		t.Fatalf("test: %v", err)
	}
	h, err := c.Add(ctx, "http://idx/get/1.nzb", "monarr")
	if err != nil || h != "SABnzbd_nzo_x1" {
		t.Fatalf("add = %q err %v", h, err)
	}
	sts, err := c.Statuses(ctx)
	if err != nil || len(sts) != 3 {
		t.Fatalf("statuses = %v err %v", sts, err)
	}
	if sts[0].State != ports.StateDownloading || sts[0].Progress != 0.55 {
		t.Errorf("queue slot = %+v", sts[0])
	}
	if sts[1].State != ports.StateCompleted || sts[1].SavePath != "/complete/Done.Movie.2024" {
		t.Errorf("history slot = %+v", sts[1])
	}
	if sts[2].State != ports.StateFailed || sts[2].Message != "crc error" {
		t.Errorf("failed slot = %+v", sts[2])
	}
}

func TestSabBadKey(t *testing.T) {
	srv := fakeSab(t)
	c := New(ports.ClientConfig{URL: srv.URL, Password: "wrong"})
	if err := c.Test(context.Background()); err == nil {
		t.Error("bad api key should fail")
	}
}
