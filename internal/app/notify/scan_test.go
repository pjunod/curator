package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestScanDeliveryRechecksOwnershipAndProbesOnlyCurrentFiles(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.RemovedPaths = true
	path := e.place("film/keep.mkv", "current clip")
	changed := e.place("film/changed.mkv", "prior clip")
	dir := filepath.Dir(path)
	item, err := e.db.CreateMediaItem(e.ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Film", Path: dir, IDs: domain.ExternalIDs{TMDB: 201}})
	if err != nil {
		t.Fatal(err)
	}
	var expected []ports.ExpectedFile
	for _, p := range []string{path, changed} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.UpsertFile(e.ctx, item, 0, p, info.Size()); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, ports.ExpectedFile{Path: p, Bytes: info.Size(), MtimeNs: info.ModTime().UnixNano()})
	}
	payload, err := json.Marshal(ports.Notification{Event: ports.ScanLinkedEvent, Import: &ports.ImportInfo{
		Kind: "movie", MediaItemID: item, TmdbID: 201, Dirs: []string{dir},
		Paths: []string{path, changed}, ExpectedFiles: map[string][]ports.ExpectedFile{dir: expected},
	}})
	if err != nil {
		t.Fatal(err)
	}
	row := sqlite.OutboxWrite{NotifierID: 1, MediaItemID: item, Directory: dir, IdempotencyKey: "scan/batch/dir",
		ExpectedFiles: expected, Event: ports.ScanLinkedEvent, Payload: string(payload)}
	if err := e.db.EnqueueDeliveries(e.ctx, []sqlite.OutboxWrite{row}); err != nil {
		t.Fatal(err)
	}
	e.place("film/changed.mkv", "different clip after the scan")
	e.clock = time.Now().Add(time.Second)
	e.d.deliverDue(e.ctx)
	requests := e.cinema.Requests()
	if len(requests) != 1 || !requests[0].ProbeOnly || len(requests[0].Expected) != 1 || requests[0].Expected[0].Path != path {
		t.Fatalf("Cinema must probe only the current file, got %+v", requests)
	}
	deliveries, err := e.db.ListDeliveries(e.ctx, 1, 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != "ok" {
		t.Fatalf("current file did not settle successfully: %v, err %v", deliveries, err)
	}

	row.IdempotencyKey = "scan/stale/dir"
	if err := e.db.EnqueueDeliveries(e.ctx, []sqlite.OutboxWrite{row}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.DeleteMediaItem(e.ctx, item); err != nil {
		t.Fatal(err)
	}
	e.d.deliverDue(e.ctx)
	deliveries, err = e.db.ListDeliveries(e.ctx, 1, 10)
	if err != nil || len(deliveries) != 2 || deliveries[0].Status != "failed" || len(e.cinema.Requests()) != 1 {
		t.Fatalf("lost ownership still posted or kept retrying: %v, err %v", deliveries, err)
	}
}

func TestScanDeliveryToOlderCinemaDoesNotWalkDirectory(t *testing.T) {
	e := newOutboxEnv(t)
	path := e.place("film/new.mkv", "current clip")
	dir := filepath.Dir(path)
	item, err := e.db.CreateMediaItem(e.ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Film", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.UpsertFile(e.ctx, item, 0, path, info.Size()); err != nil {
		t.Fatal(err)
	}
	files := []ports.ExpectedFile{{Path: path, Bytes: info.Size(), MtimeNs: info.ModTime().UnixNano()}}
	payload, err := json.Marshal(ports.Notification{Event: ports.ScanLinkedEvent, Import: &ports.ImportInfo{
		Kind: "movie", MediaItemID: item, Dirs: []string{dir}, Paths: []string{path},
		ExpectedFiles: map[string][]ports.ExpectedFile{dir: files},
	}})
	if err != nil {
		t.Fatal(err)
	}
	row := sqlite.OutboxWrite{NotifierID: 1, MediaItemID: item, Directory: dir, IdempotencyKey: "scan/batch/dir",
		ExpectedFiles: files, Event: ports.ScanLinkedEvent, Payload: string(payload)}
	if err := e.db.EnqueueDeliveries(e.ctx, []sqlite.OutboxWrite{row}); err != nil {
		t.Fatal(err)
	}
	e.clock = time.Now().Add(time.Second)
	e.d.deliverDue(e.ctx)
	deliveries, err := e.db.ListDeliveries(e.ctx, 1, 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != "ok" || deliveries[0].VisibleAt != 0 ||
		!strings.Contains(deliveries[0].Result, "does not support exact file probes") || len(e.cinema.Requests()) != 0 {
		t.Fatalf("old Cinema should receive only a feature probe and report a warning: %v, err %v", deliveries, err)
	}
}
