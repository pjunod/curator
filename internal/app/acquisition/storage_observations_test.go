package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

type storageObservingClient struct {
	*fakeClient
	volumes []ports.StorageVolumeObservation
	err     error
	probes  int
}

func (c *storageObservingClient) StorageVolumes(context.Context) ([]ports.StorageVolumeObservation, error) {
	c.probes++
	return c.volumes, c.err
}

func TestRunnerStorageObservationsSurviveProbeFailureAndRecover(t *testing.T) {
	ctx := context.Background()
	client := &storageObservingClient{fakeClient: &fakeClient{}}
	svc, _, _ := setupUsenet(t, client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	var zero uint64
	total := uint64(100)
	client.volumes = []ports.StorageVolumeObservation{
		{Name: "complete", Label: "downloads", Path: "/runner/complete", Current: true, FreeBytes: &zero, TotalBytes: &total},
		{Name: "incomplete", Path: "/runner/incomplete", FreeBytes: &zero, TotalBytes: &total},
		{Name: "scratch", Path: "/runner/scratch", Current: true, TotalBytes: &total},
	}
	if err := svc.refreshRunnerStorage(ctx); err != nil {
		t.Fatal(err)
	}
	measured, err := svc.StorageObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []StorageObservation{
		{Owner: "Runner", Role: "downloads", Name: "nzbd", Path: "/runner/complete", Status: "available", FreeBytes: &zero, TotalBytes: &total},
		{Owner: "Runner", Role: "incomplete", Name: "nzbd", Path: "/runner/incomplete", Status: "unavailable", Error: "Runner did not have a current measurement for this volume."},
		{Owner: "Runner", Role: "scratch", Name: "nzbd", Path: "/runner/scratch", Status: "unknown"},
	}
	if len(measured.Roots) != len(want) {
		t.Fatalf("got %d roots, want %d: %+v", len(measured.Roots), len(want), measured.Roots)
	}
	for i, root := range measured.Roots {
		if root.ObservedAt == nil || root.ObservedAt.IsZero() {
			t.Fatalf("root %q is missing its observation time", root.Path)
		}
		want[i].ObservedAt = root.ObservedAt
		if !reflect.DeepEqual(root, want[i]) {
			t.Errorf("root %d = %+v, want %+v", i, root, want[i])
		}
	}

	client.err = errors.New("connection refused")
	if err := svc.refreshRunnerStorage(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := svc.StorageObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		want[i].Status = "unavailable"
		want[i].FreeBytes, want[i].TotalBytes = nil, nil
		want[i].Error = "Could not read storage from this Runner connection."
	}
	if !reflect.DeepEqual(failed.Roots, want) {
		t.Errorf("roots after probe failure = %+v, want %+v", failed.Roots, want)
	}

	available := uint64(75)
	client.err = nil
	client.volumes = []ports.StorageVolumeObservation{
		{Name: "complete", Path: "/runner/new-complete", Current: true, FreeBytes: &available, TotalBytes: &total},
	}
	if err := svc.refreshRunnerStorage(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.StorageObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Roots) != 1 {
		t.Fatalf("got %d roots after recovery, want 1: %+v", len(recovered.Roots), recovered.Roots)
	}
	root := recovered.Roots[0]
	wantRoot := StorageObservation{Owner: "Runner", Role: "complete", Name: "nzbd", Path: "/runner/new-complete",
		Status: "available", ObservedAt: root.ObservedAt, FreeBytes: &available, TotalBytes: &total}
	if root.ObservedAt == nil || root.ObservedAt.Before(*want[0].ObservedAt) || !reflect.DeepEqual(root, wantRoot) {
		t.Errorf("root after recovery = %+v, want %+v with time at least %v", root, wantRoot, want[0].ObservedAt)
	}
	if client.probes != 3 {
		t.Errorf("got %d probes for three refreshes and three reads, want 3", client.probes)
	}
}

func TestRunnerStorageObservationsExcludeDisabledAndNonNativeClients(t *testing.T) {
	ctx := context.Background()
	base := &fakeClient{}
	svc, db, _ := setup(t, nil, base)
	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacy := clients[0]
	var runners []int64
	for _, cfg := range []ports.ClientConfig{
		{Type: "nzbd", Name: "disabled Runner", URL: "http://disabled"},
		{Type: "runner-torrent", Name: "Runner without observer", URL: "http://torrent", Enabled: true},
	} {
		id, err := db.AddDownloadClient(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		runners = append(runners, id)
	}
	old, err := json.Marshal(map[int64][]StorageObservation{
		legacy.ID:  {{Owner: "Runner", Name: legacy.Name, Path: "/legacy", Status: "available"}},
		runners[0]: {{Owner: "Runner", Name: "disabled Runner", Path: "/disabled", Status: "available"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, runnerStorageObservationsKey, string(old)); err != nil {
		t.Fatal(err)
	}
	svc.newClient = func(cfg ports.ClientConfig) ports.DownloadClient {
		if cfg.ID != runners[1] {
			t.Fatalf("excluded client entered storage probing: %+v", cfg)
		}
		return base
	}

	want := []StorageObservation{{Owner: "Runner", Role: "downloads", Name: "Runner without observer", Status: "unknown"}}
	before, err := svc.StorageObservations(ctx)
	if err != nil || !reflect.DeepEqual(before.Roots, want) {
		t.Fatalf("observations with excluded cache entries = %+v, err = %v, want %+v", before.Roots, err, want)
	}
	if err := svc.refreshRunnerStorage(ctx); err != nil {
		t.Fatal(err)
	}
	cache, err := svc.readRunnerStorage(ctx)
	if err != nil || len(cache) != 0 {
		t.Errorf("cache after refresh = %+v, err = %v, want empty cache", cache, err)
	}
}

func TestRunnerStorageObservationsReportFirstProbeFailureWithoutMeasurement(t *testing.T) {
	ctx := context.Background()
	client := &storageObservingClient{fakeClient: &fakeClient{}, err: errors.New("connection refused")}
	svc, _, _ := setupWithClientType(t, client.fakeClient, "runner-torrent")
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	localRoot := canonicalCompletedPath(t.TempDir())
	if err := svc.SetCompletedRoots(ctx, localRoot); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanStorageObservations(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := svc.StorageObservations(ctx)
	if err != nil || len(got.Roots) != 2 {
		t.Fatalf("observations after first probe failure = %+v, err = %v, want one Curator root and one Runner root", got.Roots, err)
	}
	local, remote := got.Roots[0], got.Roots[1]
	if local.Owner != "Curator" || local.Path != localRoot || local.Status != "available" || local.FreeBytes == nil || local.ObservedAt == nil {
		t.Errorf("Curator root = %+v, want measured local root at %q", local, localRoot)
	}
	want := StorageObservation{Owner: "Runner", Role: "downloads", Name: "runner-torrent", Status: "unavailable",
		Error: "Could not read storage from this Runner connection."}
	if !reflect.DeepEqual(remote, want) {
		t.Errorf("Runner root after first probe failure = %+v, want %+v", remote, want)
	}
}

func TestRunnerStorageObservationsReturnMalformedCacheError(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := setupUsenet(t, &fakeClient{})
	const malformed = `{"1":`
	if err := db.SetMeta(ctx, runnerStorageObservationsKey, malformed); err != nil {
		t.Fatal(err)
	}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient {
		t.Fatal("storage probe ran despite unreadable cache")
		return nil
	}
	var syntax *json.SyntaxError
	if _, err := svc.StorageObservations(ctx); !errors.As(err, &syntax) {
		t.Errorf("read error = %v, want JSON syntax error", err)
	}
	if err := svc.refreshRunnerStorage(ctx); !errors.As(err, &syntax) {
		t.Errorf("refresh error = %v, want JSON syntax error", err)
	}
	if got, err := db.GetMeta(ctx, runnerStorageObservationsKey); err != nil || got != malformed {
		t.Errorf("persisted storage cache = %q, err = %v, want %q", got, err, malformed)
	}
}
