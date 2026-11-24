package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"strings"
	"testing"
	"time"
)

func TestLocalOwnerRecoveryUsesStdinAndRevokesOldSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MONARR_DATA_DIR", dir)
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name, hash := "owner", "old-hash"
	required := true
	if err = db.ConfigureLocalOwner(ctx, &name, &hash, &required); err != nil {
		t.Fatal(err)
	}
	a, err := db.AccountByUsername(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateAccountSession(ctx, "old-session", a, "fixture", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	password := "a-new-local-owner-password"
	input, _ := json.Marshal(map[string]string{"username": name, "password": password})
	var out bytes.Buffer
	if err = runAccounts(ctx, []string{"reset-password"}, bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), password) {
		t.Fatal("recovery printed credential")
	}
	if _, err = db.AccountSession(ctx, "old-session"); err == nil {
		t.Fatal("old session survived host recovery")
	}
	fresh, err := db.AccountByID(ctx, a.ID)
	if err != nil || fresh.PasswordHash == hash || fresh.ID != a.ID {
		t.Fatal("recovery replaced account or kept old password", err)
	}
	if err = runAccounts(ctx, []string{"reset-password"}, strings.NewReader(`{"username":"owner","password":"short"}`), &out); err == nil {
		t.Fatal("short recovery password accepted")
	}
}
