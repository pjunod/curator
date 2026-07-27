package acquisition

import (
	"context"
	"testing"
)

// An idle Monarr must still talk to its download clients.
//
// This is a regression test for a bug that made the System page lie. The
// queue poll used to start from the active-downloads list and return early
// when it was empty, so an instance with nothing in flight never called its
// client at all. The contact clock froze, and the health check that reads it
// went WARNING at five minutes and ERROR at thirty — on a perfectly healthy
// setup whose only crime was having finished everything.
//
// The screenshot that produced this test: `client:nzbd_live ERROR —
// answering, but nothing has come through it for 48m`, beside a per-row Test
// button reading `✓ reachable`, on a client that was fine.
func TestAnIdleMonarrStillPollsItsClients(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := setup(t, nil, client)
	ctx := context.Background()

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if client.polls != 1 {
		t.Fatalf("Statuses calls with nothing in flight = %d, want 1 — "+
			"an idle poll is what keeps the contact clock honest", client.polls)
	}

	contacts := svc.Contacts()
	if len(contacts) != 1 {
		t.Fatalf("contacts recorded = %d, want 1", len(contacts))
	}
	for id, c := range contacts {
		if c.At.IsZero() {
			t.Errorf("client %d has no contact time after a successful poll — "+
				"the health check reads this and would report it stale forever", id)
		}
		if c.Error != "" {
			t.Errorf("client %d recorded an error on a successful poll: %q", id, c.Error)
		}
	}

	// And again: a second tick keeps it fresh rather than being skipped
	// because nothing changed.
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if client.polls != 2 {
		t.Fatalf("Statuses calls after two ticks = %d, want 2", client.polls)
	}
}

// A disabled client is not polled. The fix above widened the poll from
// "clients with active downloads" to "every enabled client", and the word
// doing the work there is *enabled* — a client someone switched off must not
// be contacted every thirty seconds, and must not appear to be in contact.
func TestADisabledClientIsNotPolled(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()

	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := clients[0]
	cfg.Enabled = false
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if client.polls != 0 {
		t.Fatalf("Statuses calls against a disabled client = %d, want 0", client.polls)
	}
}
