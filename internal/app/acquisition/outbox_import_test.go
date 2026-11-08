package acquisition

import (
	"os"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2 section 4.1: the publication's completing transaction
// writes one sub-delivery per (notifier, publication, directory), keyed
// <transfer>/<generation>/<dir-hash>, carrying the placed files with their
// bytes and mtime_ns, and starts import_to_cinema_visible. A chat notifier
// gets one row of its own. Nothing goes over the bus to be dropped.
func TestPublicationImportWritesOneSubDeliveryPerDirectory(t *testing.T) {
	f := setupV2(t, nil)
	cinema, err := f.db.AddNotifier(f.ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", OnImport: true, Enabled: true,
		Settings: map[string]string{"url": "http://cinema", "apiKey": "plx_x"}})
	if err != nil {
		t.Fatal(err)
	}
	hook, err := f.db.AddNotifier(f.ctx, ports.NotifierConfig{Type: "webhook", Name: "hook", OnImport: true, Enabled: true,
		Settings: map[string]string{"url": "http://hook"}})
	if err != nil {
		t.Fatal(err)
	}
	dl := f.grab(1, epOne)
	pub := f.publish(epOne+".runner-5d0c", "5d0c1b1e2f3a4b5c", map[string]string{epOne + ".mkv": "video"})
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 3"))
	if got := f.row(dl.ID); got.State != "imported" {
		t.Fatalf("state = %s (%s)", got.State, got.Error)
	}
	p, _, err := f.db.PublicationByPath(f.ctx, pub.Path)
	if err != nil {
		t.Fatal(err)
	}

	subs, err := f.db.ListDeliveries(f.ctx, cinema, 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("cinema rows = %+v, %v", subs, err)
	}
	s := subs[0]
	if s.PublicationID != p.ID || s.DownloadID != dl.ID || s.Attempt != 1 || s.Status != "pending" {
		t.Errorf("sub-delivery = %+v", s)
	}
	if want := SubDeliveryKey(dl.Transfer, "5d0c1b1e2f3a4b5c", s.Directory); s.IdempotencyKey != want ||
		!strings.HasPrefix(s.IdempotencyKey, dl.Transfer+"/5d0c1b1e2f3a4b5c/") || len(strings.Split(s.IdempotencyKey, "/")[2]) != 16 {
		t.Errorf("key = %q, want %q", s.IdempotencyKey, want)
	}
	if len(s.ExpectedFiles) != 1 {
		t.Fatalf("expected files = %+v", s.ExpectedFiles)
	}
	fi, err := os.Stat(s.ExpectedFiles[0].Path)
	if err != nil {
		t.Fatalf("expected file is not the placed library file: %v", err)
	}
	if e := s.ExpectedFiles[0]; e.Bytes != fi.Size() || e.MtimeNs != fi.ModTime().UnixNano() || !strings.HasPrefix(e.Path, s.Directory+"/") {
		t.Errorf("expected = %+v, file has %d bytes mtime_ns %d", e, fi.Size(), fi.ModTime().UnixNano())
	}
	if !strings.Contains(s.Payload, `"dirs":["`+s.Directory+`"]`) {
		t.Errorf("payload does not name exactly its one directory: %s", s.Payload)
	}

	chat, err := f.db.ListDeliveries(f.ctx, hook, 10)
	if err != nil || len(chat) != 1 || chat[0].IdempotencyKey != "" || chat[0].Directory != "" {
		t.Fatalf("chat rows = %+v, %v", chat, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM pipeline_timings WHERE kind=? AND transfer=? AND ended_at=0`, pipeline.ImportToCinemaVisible, dl.Transfer); n != 1 {
		t.Errorf("import_to_cinema_visible waiting = %d, want started at imported", n)
	}
}

// The key is stable per directory and distinct across directories.
func TestSubDeliveryKeyIsPerDirectory(t *testing.T) {
	a := SubDeliveryKey("t-1-x", "g", "/media/tv/Show/Season 01")
	b := SubDeliveryKey("t-1-x", "g", "/media/tv/Show/Season 02")
	if a == b || a != SubDeliveryKey("t-1-x", "g", "/media/tv/Show/Season 01") {
		t.Errorf("keys %q %q", a, b)
	}
}
