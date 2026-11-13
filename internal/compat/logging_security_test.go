package compat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/compat"
)

func TestUnknownRouteLogsParameterNamesWithoutCredentials(t *testing.T) {
	var out bytes.Buffer
	personality := compat.NewSonarr(compat.Deps{Log: slog.New(slog.NewJSONHandler(&out, nil)), APIKey: func(context.Context) string { return "secret-api-key" }})
	req := httptest.NewRequest("GET", "/api/v3/unsupported?apikey=secret-api-key&password=secret-password&title=secret-title&title=second-title", nil)
	rr := httptest.NewRecorder()
	personality.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d", rr.Code)
	}
	for _, secret := range []string{"secret-api-key", "secret-password", "secret-title", "second-title"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("query value leaked: %q", out.String())
		}
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record["queryKeys"], []any{"apikey", "password", "title"}) {
		t.Fatalf("missing diagnostic keys: %v", record)
	}
}
