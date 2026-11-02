package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pjunod/monarr/internal/infra/config"
)

// runMaintenance is the distroless image's HTTP client. Running it with docker
// exec binds control requests to the captured old container's network namespace.
// Credentials and owner secrets arrive on stdin, never in command arguments.
func runMaintenance(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: monarr maintenance status|drain|abort (JSON credentials/owner on stdin)")
	}
	var input struct {
		APIKey     string `json:"apiKey"`
		OwnerToken string `json:"ownerToken"`
		Generation string `json:"generation"`
	}
	if err := json.NewDecoder(io.LimitReader(in, 8193)).Decode(&input); err != nil && err != io.EOF {
		return fmt.Errorf("invalid maintenance client input")
	}
	method := http.MethodGet
	var body any
	switch args[0] {
	case "status":
	case "drain":
		method = http.MethodPost
		body = map[string]string{"ownerToken": input.OwnerToken}
	case "abort":
		method = http.MethodDelete
		body = map[string]string{"ownerToken": input.OwnerToken, "generation": input.Generation}
	default:
		return fmt.Errorf("unknown maintenance operation")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	host := cfg.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+net.JoinHostPort(host, strconv.Itoa(cfg.Port))+"/api/v1/system/deployment-drain", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if input.APIKey != "" {
		req.Header.Set("X-Api-Key", input.APIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("maintenance request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("maintenance request returned HTTP %d: %s", response.StatusCode, data)
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid maintenance status response")
	}
	_, err = out.Write(append(data, '\n'))
	return err
}
