package acquisition

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// taggingClient implements the optional ports.TaggedAdder and records what
// it was handed, so the assertions are about what actually crossed the
// boundary rather than about what Monarr stored afterwards.
type taggingClient struct {
	fakeClient
	gotTransfer string
	gotURL      string
	gotCategory string
	addErr      error
}

func (c *taggingClient) AddTagged(ctx context.Context, url, cat, transfer string) (ports.Handle, error) {
	c.gotTransfer, c.gotURL, c.gotCategory = transfer, url, cat
	if c.addErr != nil {
		return "", c.addErr
	}
	return "h-tagged", nil
}

var transferShape = regexp.MustCompile(`^t-\d+-[0-9a-f]{6}$`)

// The transfer id has to reach the client ON the add — a client cannot be
// told afterwards what to have called the download — and the same id has
// to be on Monarr's row, or the two halves of the trace never join up.
func TestGrabSendsTheTransferIDToATaggingClient(t *testing.T) {
	client := &taggingClient{}
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-X", DownloadURL: "https://idx/x.nzb",
		Indexer: "idx", Protocol: "torrent", Size: 1,
	})
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}

	if !transferShape.MatchString(client.gotTransfer) {
		t.Fatalf("client got transfer %q, want t-<id>-<6 hex>", client.gotTransfer)
	}
	if !strings.HasPrefix(client.gotTransfer, "t-"+itoa(id)+"-") {
		t.Errorf("transfer %q does not name download %d", client.gotTransfer, id)
	}
	if client.gotURL != "https://idx/x.nzb" {
		t.Errorf("url = %q", client.gotURL)
	}
	if client.gotCategory != "monarr" {
		t.Errorf("category = %q, want the client's configured category", client.gotCategory)
	}

	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.Transfer != client.gotTransfer {
		t.Errorf("row transfer %q != what the client was sent %q — the two halves of the trace must be the same id",
			dl.Transfer, client.gotTransfer)
	}
	if dl.Handle != "h-tagged" {
		t.Errorf("handle = %q, want the tagged add's handle", dl.Handle)
	}
	// The id belongs in the first line of the trace, because that is where
	// someone starts reading when a transfer goes wrong.
	if len(dl.Handoff) == 0 || !strings.Contains(dl.Handoff[0].Detail, client.gotTransfer) {
		t.Errorf("handoff trace opens with %v, want it to name the transfer id", dl.Handoff)
	}
}

// Two downloads must never share an id: it is the key someone greps for.
func TestTransferIDsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newTransferID(int64(i%5) + 1) // ids repeat; the suffix must not
		if seen[id] {
			t.Fatalf("duplicate transfer id %q", id)
		}
		seen[id] = true
		if !transferShape.MatchString(id) {
			t.Fatalf("malformed transfer id %q", id)
		}
	}
}

// A client that cannot carry a tag is added exactly as before. Monarr
// still records the id on its own row, so its half of the trace is
// threaded even when the client's half cannot be.
func TestGrabFallsBackToAPlainAddForUntaggableClients(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-Y", DownloadURL: "https://idx/y.torrent",
		Indexer: "idx", Protocol: "torrent", Size: 1,
	})
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	if len(client.added) != 1 || client.added[0] != "https://idx/y.torrent" {
		t.Fatalf("plain Add not used: %v", client.added)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !transferShape.MatchString(dl.Transfer) {
		t.Errorf("row transfer = %q, want an id even when the client cannot hold one", dl.Transfer)
	}
	if dl.Handle != "h1" {
		t.Errorf("handle = %q", dl.Handle)
	}
}

// Inserting the row before the add is what makes the id possible, and it
// creates one failure the old order did not have: a client that refuses
// the download would leave a row describing something nobody has. It must
// be cleaned up, or the queue fills with ghosts after an outage.
func TestGrabLeavesNoRowWhenTheClientRefuses(t *testing.T) {
	client := &taggingClient{addErr: errors.New("client says no")}
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()

	before, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-Z", DownloadURL: "https://idx/z.nzb",
		Indexer: "idx", Protocol: "torrent", Size: 1,
	}); err == nil {
		t.Fatal("want the client's error to reach the caller")
	}
	after, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a rejected grab left %d row(s) behind", len(after)-len(before))
	}
}

// The type CHECK is a rebuilt table in migration 0020, and a CHECK that
// still refuses 'nzbd' fails at the moment an operator saves the client —
// with a SQL constraint error, in a settings form. Cheap to assert, and
// it is exactly the class of thing that only shows up in production.
func TestANzbdClientCanBeStored(t *testing.T) {
	_, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id, err := db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: "nzbd", Name: "nzbd", URL: "http://nzbd:6789",
		Password: "token", Category: "tv", Enabled: true,
	})
	if err != nil {
		t.Fatalf("storing an nzbd client: %v", err)
	}
	got, err := db.GetDownloadClient(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "nzbd" || got.Password != "token" {
		t.Errorf("round-tripped as %+v", got)
	}
}

// nzbd is a usenet client. Getting this wrong routes every usenet grab to
// a torrent client (or to none) and the failure looks like "no indexer
// results", which is a long way from the cause.
func TestNzbdIsAUsenetClient(t *testing.T) {
	for _, c := range []struct {
		typ  string
		want string
	}{
		{"nzbd", "usenet"}, {"nzbget", "usenet"}, {"sabnzbd", "usenet"},
		{"qbittorrent", "torrent"}, {"transmission", "torrent"}, {"deluge", "torrent"},
	} {
		if got := protocolOfClient(c.typ); got != c.want {
			t.Errorf("protocolOfClient(%q) = %q, want %q", c.typ, got, c.want)
		}
	}
}
