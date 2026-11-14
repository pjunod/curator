package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pjunod/monarr/internal/domain"
	"strings"
	"sync"
	"testing"
	"time"
)

func householdOwner(t *testing.T, db *DB) (domain.Account, string) {
	t.Helper()
	name, hash := "owner", "local-owner-hash"
	required := true
	if err := db.ConfigureLocalOwner(context.Background(), &name, &hash, &required); err != nil {
		t.Fatal(err)
	}
	a, err := db.AccountByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	digest := "owner-session"
	if err = db.CreateAccountSession(context.Background(), digest, a, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return a, digest
}
func householdMember(t *testing.T, db *DB, owner domain.Account, digest, name string) (domain.Account, string) {
	t.Helper()
	ctx := context.Background()
	invite := "invite-" + name
	if err := db.CreateHouseholdInvite(ctx, owner, digest, invite, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, err := db.AcceptHouseholdInvite(ctx, invite, name, name, "member-hash")
	if err != nil {
		t.Fatal(err)
	}
	session := "session-" + name
	if err = db.CreateAccountSession(ctx, session, a, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return a, session
}
func TestHouseholdAccountsRemainLocalAndDurable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	member, session := householdMember(t, db, owner, digest, "person")
	if member.Role != "member" || member.ID == owner.ID {
		t.Fatal("invitation did not create independent local member")
	}
	if err := db.EnsureInitialHouseholdOwner(ctx); err != nil {
		t.Fatal(err)
	}
	accounts, err := db.ListAccounts(ctx)
	if err != nil || len(accounts) != 2 {
		t.Fatal("bootstrap duplicated or lost accounts", err)
	}
	raw, _ := json.Marshal(accounts)
	for _, secret := range []string{"password_hash", "PasswordHash", "local-owner-hash", "member-hash", "credential_generation"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("account DTO exposed credentials", secret)
		}
	}
	// A second database handle is a restarted process, not an in-memory token map.
	restarted, err := Open(strings.TrimSuffix(db.Path, "/"+FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	active, err := restarted.AccountSession(ctx, session)
	if err != nil || active.Account.ID != member.ID || active.Method != "local" {
		t.Fatal("local session did not survive restart", err)
	}
	if err = db.UpdateAccount(ctx, owner, digest, member.ID, member.Revision, "member", "suspended", member.DisplayName, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.AccountSession(ctx, session); !errors.Is(err, ErrNotFound) {
		t.Fatal("suspension did not revoke persisted session", err)
	}
	if err = db.UpdateAccount(ctx, owner, digest, member.ID, member.Revision, "member", "active", "stale edit", ""); !errors.Is(err, ErrAccountConflict) {
		t.Fatal("stale edit undid suspension", err)
	}
}
func TestHouseholdLastOwnerAndCredentialFences(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	if err := db.UpdateAccount(ctx, owner, digest, owner.ID, owner.Revision, "member", "active", owner.DisplayName, ""); !errors.Is(err, ErrLastOwner) {
		t.Fatal("last owner demoted", err)
	}
	second, _ := householdMember(t, db, owner, digest, "second")
	if err := db.UpdateAccount(ctx, owner, digest, second.ID, second.Revision, "owner", "active", second.DisplayName, ""); err != nil {
		t.Fatal(err)
	}
	second, err := db.AccountByID(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateAccountSession(ctx, "second-owner", second, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		out <- db.UpdateAccount(ctx, owner, digest, owner.ID, owner.Revision, "member", "active", owner.DisplayName, "")
	}()
	go func() {
		defer wg.Done()
		out <- db.UpdateAccount(ctx, second, "second-owner", second.ID, second.Revision, "member", "active", second.DisplayName, "")
	}()
	wg.Wait()
	close(out)
	success := 0
	for err := range out {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrLastOwner) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("concurrent demotions did not preserve one owner", success)
	}
}
func TestHostRecoveryPreservesHistoryAndInvalidatesInvites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	if err := db.CreateHouseholdInvite(ctx, owner, digest, "outstanding", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	id, err := db.SubmitHouseholdRequest(ctx, owner, digest, domain.HouseholdRequest{Kind: domain.KindMovie, TMDBID: 550, IDs: domain.ExternalIDs{TMDB: 550}, Title: "Saved history", Fingerprint: "history"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ResetLocalOwnerPassword(ctx, owner.Username, "new-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AccountSession(ctx, digest); !errors.Is(err, ErrNotFound) {
		t.Fatal("old session survived reset", err)
	}
	if _, err = db.AcceptHouseholdInvite(ctx, "outstanding", "intruder", "Intruder", "hash"); !errors.Is(err, ErrAccountConflict) {
		t.Fatal("old invitation survived reset", err)
	}
	if err = db.CreateAccountSession(ctx, "stale", owner, "test", time.Now().Add(time.Hour)); !errors.Is(err, ErrAccountConflict) {
		t.Fatal("old credential proof minted a session", err)
	}
	fresh, err := db.AccountByID(ctx, owner.ID)
	if err != nil || fresh.PasswordHash != "new-hash" || fresh.ID != owner.ID {
		t.Fatal("local recovery replaced the identity", err)
	}
	request, err := db.HouseholdRequest(ctx, id, owner.ID, true)
	if err != nil || !request.Subscribed || request.Title != "Saved history" {
		t.Fatal("recovery lost history", err)
	}
	off := false
	if err = db.ConfigureLocalOwner(ctx, nil, nil, &off); err == nil {
		t.Fatal("household recovery reopened anonymous owner access")
	}
}
func TestHouseholdInvitationConsumesExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	if err := db.CreateHouseholdInvite(ctx, owner, digest, "one-use", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func() { _, err := db.AcceptHouseholdInvite(ctx, "one-use", name, name, "hash"); results <- err }()
	}
	success := 0
	for range 2 {
		if err := <-results; err == nil {
			success++
		} else if !errors.Is(err, ErrAccountConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("invitation did not have one committed winner", success)
	}
}
