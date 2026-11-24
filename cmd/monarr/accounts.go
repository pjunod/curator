package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pjunod/monarr/internal/api"
	"github.com/pjunod/monarr/internal/infra/config"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"io"
	"os"
	"path/filepath"
)

// Secrets arrive on stdin, never in arguments, environment, logs or output.
func runAccounts(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 || args[0] != "reset-password" {
		return errors.New("usage: monarr accounts reset-password (JSON username/password on stdin; existing data directory required)")
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(io.LimitReader(in, 4097))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid account recovery input")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, sqlite.FileName)); err != nil {
		return errors.New("existing Curator database required")
	}
	db, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err = db.Migrate(ctx); err != nil {
		return err
	}
	if err = db.EnsureInitialHouseholdOwner(ctx); err != nil {
		return err
	}
	if err = api.RecoverLocalOwner(ctx, db, body.Username, body.Password); err != nil {
		return errors.New("local owner password could not be reset; verify the owner username and password length")
	}
	_, err = fmt.Fprintln(out, "Local owner password reset. Account history is preserved; its sessions and outstanding invitations are revoked. Sign in locally with the new password.")
	return err
}
