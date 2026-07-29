package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A successful import should leave nothing behind on the download client.
//
// It used to leave everything: monarr copied the payload into the library and
// then walked away, so every grab kept a second full copy for as long as the
// client held it. On a setup where the download disk and the library are
// different filesystems — the normal one — that is real bytes, not a hardlink,
// and it is invisible from inside monarr. A user found 923 GB of it.
func TestImportRemovesThePayload(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	payload := t.TempDir()
	src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(src, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)

	if err := svc.runImport(ctx, dl); err != nil {
		t.Fatalf("import: %v", err)
	}

	if len(client.removed) != 1 {
		t.Fatalf("Remove called %d times, want 1 — the payload is still on the client", len(client.removed))
	}
	if !client.removed[0].DeleteData {
		t.Error("removed the queue entry but not the data; the bytes are what fill a disk")
	}

	// And it is recorded, so the next sweep does not ask the client again.
	rows, err := db.ImportedWithPayload(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("%d download(s) still pending cleanup after a successful removal", len(rows))
	}
}

// A torrent client is still seeding, so its data is not monarr's to delete on
// a guess. The default has to reflect that, and the guard has to hold even
// though every other part of the flow is identical.
func TestTorrentPayloadIsLeftAlone(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupTorrent(t, client)
	ctx := context.Background()

	payload := t.TempDir()
	src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(src, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)

	if err := svc.runImport(ctx, dl); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(client.removed) != 0 {
		t.Errorf("deleted a seeding torrent's data: %+v", client.removed)
	}
}

// The sweep is what makes the setting mean "monarr should not be leaving these
// around" rather than "monarr should stop leaving NEW ones around". Somebody
// enabling it after a year of grabs wants the year of grabs collected too.
func TestCleanupSweepDrainsTheBacklog(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	// Three downloads that imported before the setting existed.
	for i := 0; i < 3; i++ {
		dl := insertImportable(t, db, itemID, t.TempDir())
		if err := db.UpdateDownloadState(ctx, dl.ID, "imported", 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := db.ImportedWithPayload(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending payloads, got %d", len(pending))
	}

	if err := svc.CleanupPayloads(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.removed) != 3 {
		t.Errorf("swept %d payloads, want 3", len(client.removed))
	}

	// Idempotent: a second pass has nothing left to do, which is what stops
	// the sweep asking the client to re-delete everything it has ever seen.
	left, err := db.ImportedWithPayload(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d payload(s) still marked pending after the sweep", len(left))
	}
	if err := svc.CleanupPayloads(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.removed) != 3 {
		t.Errorf("the second sweep re-removed payloads: %d calls total", len(client.removed))
	}
}

// ---- helpers ----

func setupUsenet(t *testing.T, c *fakeClient) (*Service, *sqlite.DB, int64) {
	return setupWithClientType(t, c, "nzbd")
}

func setupTorrent(t *testing.T, c *fakeClient) (*Service, *sqlite.DB, int64) {
	return setupWithClientType(t, c, "qbittorrent")
}

// setupWithClientType is setup() with the download client replaced by one of a
// chosen type, so the usenet/torrent split in the cleanup default is exercised
// against a real stored config rather than a hand-built struct.
func setupWithClientType(t *testing.T, c *fakeClient, clientType string) (*Service, *sqlite.DB, int64) {
	t.Helper()
	svc, db, itemID := setup(t, nil, c)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range clients {
		if err := db.DeleteDownloadClient(ctx, existing.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: clientType, Name: clientType, URL: "http://x", Category: "monarr",
		Enabled: true, RemoveCompleted: ports.ProtocolOfClient(clientType) == "usenet",
	}); err != nil {
		t.Fatal(err)
	}
	return svc, db, itemID
}

func insertImportable(t *testing.T, db *sqlite.DB, itemID int64, savePath string) sqlite.Download {
	t.Helper()
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("no client configured: %v", err)
	}
	id, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID:  itemID,
		WantableIDs:  []string{"episode:1:1:1"},
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-GRP",
		Indexer:      "idx",
		Protocol:     ports.ProtocolOfClient(clients[0].Type),
		Quality:      quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		Size:         1 << 30,
		ClientID:     clients[0].ID,
		Handle:       "h1",
		State:        "downloading",
		SavePath:     savePath,
		ImportPath:   savePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// InsertDownload does not carry the paths — those land later via
	// UpdateDownloadHandoff, once the client has reported where it put things.
	// runImport reads them off the struct it is handed, so set them here.
	dl.SavePath, dl.ImportPath = savePath, savePath
	return dl
}

var _ = domain.KindMovie
