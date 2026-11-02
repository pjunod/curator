// app-http is a task-only client for an isolated qualification container's
// loopback native API. It never accepts a hostname or follows redirects.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer, transport http.RoundTripper) error {
	if len(args) != 2 || !strings.HasPrefix(args[1], "/api/v1/") {
		return fmt.Errorf("method and native API path required")
	}
	body, err := io.ReadAll(io.LimitReader(input, 1<<20))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, args[0], "http://127.0.0.1:7676"+args[1], bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	closeErr := response.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	var value any
	if len(data) > 0 {
		if err = json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid JSON response: %w", err)
		}
	}
	return json.NewEncoder(output).Encode(map[string]any{"status": response.StatusCode, "body": value, "seconds": time.Since(start).Seconds()})
}
