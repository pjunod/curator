package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/config"
	"github.com/pjunod/monarr/internal/infra/logging"
)

// The JSON format is the one an operator points a log shipper at, so its
// output has to actually parse — a text handler wired to the "json" setting
// would only be noticed by whatever downstream tool silently drops the lines.
func TestTailJSONFormatEmitsParseableRecords(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, config.Config{LogLevel: "info", LogFormat: "json"})
	log.Info("queue started", "workers", 2)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("json format did not emit JSON (%v): %s", err, buf.String())
	}
	if rec["msg"] != "queue started" {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["workers"] != float64(2) {
		t.Errorf("structured attribute lost: %v", rec["workers"])
	}
}

// Anything that is not "json" is text — that is the documented default and
// what an operator reading `docker logs` gets.
func TestTailTextFormatIsTheDefault(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, config.Config{LogLevel: "info", LogFormat: "text"})
	log.Info("queue started", "workers", 2)

	out := buf.String()
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("text format emitted JSON: %s", out)
	}
	if !strings.Contains(out, "workers=2") || !strings.Contains(out, "queue started") {
		t.Errorf("text record = %q", out)
	}
}

// The level is the whole point of the setting: a warn-level install must not
// pay for debug lines it will never print.
func TestTailLevelFiltersQuieterRecords(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, config.Config{LogLevel: "warn", LogFormat: "text"})
	log.Debug("noisy")
	log.Info("routine")
	log.Warn("something is wrong")

	out := buf.String()
	if strings.Contains(out, "noisy") || strings.Contains(out, "routine") {
		t.Errorf("records below the configured level were emitted: %q", out)
	}
	if !strings.Contains(out, "something is wrong") {
		t.Errorf("the warn record was dropped: %q", out)
	}
}

// Config is validated at load, so an unparseable level should be impossible
// here — but this builds the logger that would report the problem, so it
// falls back to info rather than returning an error nobody could log.
func TestTailUnparseableLevelFallsBackToInfoRatherThanFailing(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, config.Config{LogLevel: "loud", LogFormat: "text"})
	log.Debug("below info")
	log.Info("at info")

	out := buf.String()
	if strings.Contains(out, "below info") {
		t.Errorf("the fallback level is not info: %q", out)
	}
	if !strings.Contains(out, "at info") {
		t.Errorf("nothing was logged at all after a bad level: %q", out)
	}
}
