// Package docs holds the tests that keep the documentation honest: the index
// lists every file, every relative link resolves, and the literals
// docs/integration.md quotes are the ones the code uses.
package docs

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/pjunod/monarr/internal/domain/backoff"
)

const root = "../.."

var (
	fence      = regexp.MustCompile("(?s)```.*?```")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	mdLink     = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	heading    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
)

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// markdownFiles is every document whose links are checked: the front page,
// the status ledger's pointers into docs/, and everything under docs/ except
// the renderings (captured HTML, not documentation).
func markdownFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"README.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() && filepath.Base(p) == "renderings" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// slug is GitHub's anchor for a heading.
func slug(h string) string {
	h = strings.ToLower(inlineCodeText(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r == ' ' || r == '-':
			b.WriteByte('-')
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func inlineCodeText(s string) string { return strings.ReplaceAll(s, "`", "") }

func anchors(body string) map[string]bool {
	out := map[string]bool{}
	body = fence.ReplaceAllString(body, "")
	for _, m := range heading.FindAllStringSubmatch(body, -1) {
		out[slug(m[1])] = true
	}
	return out
}

func TestEveryRelativeLinkResolves(t *testing.T) {
	for _, file := range markdownFiles(t) {
		body := inlineCode.ReplaceAllString(fence.ReplaceAllString(read(t, file), ""), "")
		for _, m := range mdLink.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			path, frag, _ := strings.Cut(target, "#")
			resolved := filepath.ToSlash(file)
			if path != "" {
				resolved = filepath.ToSlash(filepath.Join(filepath.Dir(file), path))
			}
			info, err := os.Stat(filepath.Join(root, resolved))
			if err != nil {
				t.Errorf("%s: link %q does not resolve (%s)", file, target, resolved)
				continue
			}
			if frag != "" && !info.IsDir() && strings.HasSuffix(resolved, ".md") {
				if !anchors(read(t, resolved))[frag] {
					t.Errorf("%s: link %q: %s has no heading #%s", file, target, resolved, frag)
				}
			}
		}
	}
}

func TestIndexListsEveryDocsFile(t *testing.T) {
	index := read(t, "docs/README.md")
	listed := func(rel string) bool { return strings.Contains(index, "]("+rel+")") }
	adrIndex := read(t, "docs/adr/README.md")
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(root, "docs"), p)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == "README.md":
		case strings.HasPrefix(rel, "renderings/"):
			// captured pages: the index lists the directory
			if !listed("renderings/") {
				t.Errorf("docs/README.md does not list renderings/")
			}
		case strings.HasPrefix(rel, "adr/"):
			name := strings.TrimPrefix(rel, "adr/")
			if name == "README.md" {
				if !listed("adr/README.md") {
					t.Errorf("docs/README.md does not list adr/README.md")
				}
			} else if !strings.Contains(adrIndex, "]("+name+")") {
				t.Errorf("docs/adr/README.md does not list %s", name)
			}
		default:
			if !listed(rel) {
				t.Errorf("docs/README.md does not list %s", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIndexListsOnlyFilesThatExist(t *testing.T) {
	// The link test already fails on a dangling entry; this one names the
	// failure for the index specifically.
	for _, m := range mdLink.FindAllStringSubmatch(read(t, "docs/README.md"), -1) {
		path, _, _ := strings.Cut(m[1], "#")
		if path == "" || strings.HasPrefix(path, "http") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "docs", path)); err != nil {
			t.Errorf("docs/README.md lists %s, which does not exist", path)
		}
	}
}

// dur prints a rung the way the page does: 5s, 30s, 2m, 1h.
func dur(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
}

// TestIntegrationLiteralsMatchTheCode checks the literals the integration page
// quotes against the source that owns them. Each row must appear in the page
// and in the named source file.
func TestIntegrationLiteralsMatchTheCode(t *testing.T) {
	page := read(t, "docs/integration.md")
	for _, ev := range []string{"job_pp_finished", "job_status_changed", "job_control_changed", "job_publication_ready", "job_pp_stage", "tick", "reset", "lagged", "job_deleted"} {
		src := read(t, "internal/adapters/nzbd/events.go")
		if !strings.Contains(src, `"`+ev+`"`) {
			t.Errorf("events.go no longer handles %q; update docs/integration.md", ev)
		}
		if !strings.Contains(page, "`"+ev+"`") {
			t.Errorf("docs/integration.md does not mention event %q", ev)
		}
	}
	if !strings.Contains(page, "`job_finished` is **deliberately ignored**") {
		t.Errorf("docs/integration.md lost the job_finished note")
	}

	// Ladders: the page prints each as `a → b → c`.
	for name, l := range map[string]backoff.Ladder{"outbox": backoff.Outbox, "retry": backoff.Retry} {
		var rungs []string
		for _, d := range l {
			rungs = append(rungs, dur(d))
		}
		if want := "`" + strings.Join(rungs, " → ") + "`"; !strings.Contains(page, want) {
			t.Errorf("docs/integration.md does not print the %s ladder as %s", name, want)
		}
	}

	for _, c := range []struct{ literal, file, needle string }{
		{"20 s", "internal/app/notify/deliver.go", "sendTimeout = 20 * time.Second"},
		{"| Deliveries in flight per notifier | 4 |", "internal/app/notify/deliver.go", "laneWidth = 4"},
		{"30 minutes", "internal/app/notify/deliver.go", "WarningAfter = 30 * time.Minute"},
		{"10 minutes of successful polls", "internal/app/acquisition/reducer.go", "goneGrace = 10 * time.Minute"},
		{"6 hours", "internal/app/acquisition/reducer.go", "within 6 h of discovery"},
		{"Replacement grab worker tick | 5 s", "internal/app/acquisition/replacement_grab_worker.go", "replacementGrabTick     = 5 * time.Second"},
		{"15 minutes", "internal/app/acquisition/reducer.go", "indexerOutageRetry = 15 * time.Minute"},
		{"`scan:trigger`", "internal/adapters/notify/plurx.go", "scan:trigger"},
		{"`failure_to_next_grab`, `runner_done_to_import`, `import_to_cinema_visible`", "internal/api/openapi.yaml", "enum: [failure_to_next_grab, runner_done_to_import, import_to_cinema_visible]"},
		{"nzbd 6789 · Monarr 7676 · plurx 32400", "docs/settings.md", "6789"},
	} {
		if !strings.Contains(page, c.literal) {
			t.Errorf("docs/integration.md does not quote %q", c.literal)
		}
		if !strings.Contains(read(t, c.file), c.needle) {
			t.Errorf("%s no longer contains %q; update docs/integration.md", c.file, c.needle)
		}
	}
}
