// Package docs holds the tests that keep the documentation honest: the index
// lists every file, every relative link resolves, and the literals
// docs/integration.md quotes are the ones the code uses.
package docs

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
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
	files := []string{"README.md", "STATUS.md"}
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

// constDuration reads a named constant out of a Go source file and evaluates
// it, so the test compares against the value the code uses and not against how
// the line happens to be formatted. It understands what the constants here are
// built from: integer literals, other named constants of the same file, the
// time.Nanosecond..time.Hour units, and * + - between them.
func constDuration(t *testing.T, file, name string) time.Duration {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, file), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	exprs := map[string]ast.Expr{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if i < len(vs.Values) {
					exprs[id.Name] = vs.Values[i]
				}
			}
		}
	}
	units := map[string]time.Duration{"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
	var eval func(e ast.Expr) (constant.Value, error)
	eval = func(e ast.Expr) (constant.Value, error) {
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind != token.INT {
				return nil, fmt.Errorf("unsupported literal %s", e.Value)
			}
			return constant.MakeFromLiteral(e.Value, e.Kind, 0), nil
		case *ast.ParenExpr:
			return eval(e.X)
		case *ast.Ident:
			x, ok := exprs[e.Name]
			if !ok {
				return nil, fmt.Errorf("%s is not a constant of %s", e.Name, file)
			}
			return eval(x)
		case *ast.SelectorExpr:
			if pkg, ok := e.X.(*ast.Ident); ok && pkg.Name == "time" {
				if u, ok := units[e.Sel.Name]; ok {
					return constant.MakeInt64(int64(u)), nil
				}
			}
			return nil, fmt.Errorf("unsupported selector %s", e.Sel.Name)
		case *ast.BinaryExpr:
			x, err := eval(e.X)
			if err != nil {
				return nil, err
			}
			y, err := eval(e.Y)
			if err != nil {
				return nil, err
			}
			if e.Op != token.MUL && e.Op != token.ADD && e.Op != token.SUB {
				return nil, fmt.Errorf("unsupported operator %s", e.Op)
			}
			return constant.BinaryOp(x, e.Op, y), nil
		}
		return nil, fmt.Errorf("unsupported expression %T", e)
	}
	x, ok := exprs[name]
	if !ok {
		t.Fatalf("%s: no constant %s; update internal/docs and docs/integration.md", file, name)
	}
	v, err := eval(x)
	if err != nil {
		t.Fatalf("%s: constant %s: %v", file, name, err)
	}
	n, ok := constant.Int64Val(constant.ToInt(v))
	if !ok {
		t.Fatalf("%s: constant %s does not fit a duration", file, name)
	}
	return time.Duration(n)
}

// spoken prints a duration the way the page's prose does: "20 s", "30
// minutes", "6 hours".
func spoken(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	default:
		return strconv.Itoa(int(d/time.Second)) + " s"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// sections splits a page by its level-2 headings, keyed by the heading text.
// Fenced blocks are kept in the body but cannot start a section, so a shell
// comment starting with "## " is not mistaken for a heading.
func sections(page string) map[string]string {
	out := map[string]string{}
	var name string
	var body strings.Builder
	flush := func() {
		if name != "" {
			out[name] = body.String()
		}
		body.Reset()
	}
	inFence := false
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			flush()
			name = strings.TrimPrefix(line, "## ")
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return out
}

// section returns the one section whose heading starts with prefix.
func section(t *testing.T, secs map[string]string, prefix string) string {
	t.Helper()
	var found []string
	for name := range secs {
		if strings.HasPrefix(name, prefix) {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("docs/integration.md: want exactly one section starting %q, found %v", prefix, found)
	}
	return secs[found[0]]
}

// tableRows reads the `| label | value | source |` rows of a section.
func tableRows(body string) map[string][2]string {
	out := map[string][2]string{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), "|")
		if len(cells) != 3 {
			continue
		}
		out[strings.TrimSpace(cells[0])] = [2]string{strings.TrimSpace(cells[1]), strings.Trim(strings.TrimSpace(cells[2]), "`")}
	}
	return out
}

// ladderTable prints a ladder the way the literals table does:
// `5s, 30s, 2m`, then every 5m (or "hourly" when the last rung is an hour).
func ladderTable(l backoff.Ladder) string {
	var rungs []string
	for _, d := range l {
		rungs = append(rungs, dur(d))
	}
	every := "every " + dur(l.Max())
	if l.Max() == time.Hour {
		every = "hourly"
	}
	return "`" + strings.Join(rungs, ", ") + "`, then " + every
}

// TestIntegrationLiteralsMatchTheCode checks the literals the integration page
// quotes against the code that owns them: constants are read from the source
// and evaluated, rendered the way the page prints them, and looked for in the
// section of the page that discusses them and in the literals table.
func TestIntegrationLiteralsMatchTheCode(t *testing.T) {
	page := read(t, "docs/integration.md")
	secs := sections(page)
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

	table := tableRows(section(t, secs, "Literals this page quotes"))
	expectRow := func(label, value, source string) {
		t.Helper()
		row, ok := table[label]
		if !ok {
			t.Errorf("literals table has no row %q", label)
			return
		}
		if row[0] != value {
			t.Errorf("literals table row %q says %q; the code says %q", label, row[0], value)
		}
		if row[1] != source {
			t.Errorf("literals table row %q names source %q, want %q", label, row[1], source)
		}
	}
	// inSection asserts the literal appears in the named section of the page.
	inSection := func(prefix, literal string) {
		t.Helper()
		if !strings.Contains(section(t, secs, prefix), literal) {
			t.Errorf("docs/integration.md section %q does not quote %q", prefix, literal)
		}
	}

	// Ladders: prose prints each as `a → b → c` in section 6; the table prints
	// the comma form.
	for _, c := range []struct {
		name   string
		l      backoff.Ladder
		label  string
		prefix string
	}{
		{"outbox", backoff.Outbox, "Outbox ladder", "6. "},
		{"retry", backoff.Retry, "Submission, manual-admission and import-retry ladder", "6. "},
	} {
		var rungs []string
		for _, d := range c.l {
			rungs = append(rungs, dur(d))
		}
		if want := "`" + strings.Join(rungs, " → ") + "`"; !strings.Contains(section(t, secs, c.prefix), want) {
			t.Errorf("docs/integration.md section %q does not print the %s ladder as %s", c.prefix, c.name, want)
		}
		expectRow(c.label, ladderTable(c.l), "internal/domain/backoff")
	}

	const (
		deliver   = "internal/app/notify/deliver.go"
		reducer   = "internal/app/acquisition/reducer.go"
		worker    = "internal/app/acquisition/replacement_grab_worker.go"
		pipelineV = "internal/infra/sqlite/pipeline_v2.go"
	)
	sendTimeout := constDuration(t, deliver, "sendTimeout")
	warnAfter := constDuration(t, deliver, "WarningAfter")
	goneGrace := constDuration(t, reducer, "goneGrace")
	outage := constDuration(t, reducer, "indexerOutageRetry")
	tick := constDuration(t, worker, "replacementGrabTick")
	stalled := constDuration(t, pipelineV, "DeadlinePublication")
	lanes := constDuration(t, deliver, "laneWidth") // a count, evaluated the same way

	expectRow("Per-attempt deadline of a sub-delivery", spoken(sendTimeout), deliver)
	expectRow("Deliveries in flight per notifier", strconv.Itoa(int(lanes)), deliver)
	expectRow("Sub-delivery shown as a warning after", spoken(warnAfter), deliver)
	expectRow("`failed{lost}` after", spoken(goneGrace)+" of successful polls", reducer)
	expectRow("Publication `import_failed{stalled}` after", spoken(stalled), reducer)
	expectRow("Replacement grab worker tick", spoken(tick), worker)
	expectRow("Indexer-outage re-queue", spoken(outage), reducer)

	inSection("6. ", "its own "+spoken(sendTimeout)+" deadline")
	inSection("6. ", spoken(warnAfter)+" after the import")
	inSection("4. ", "(every "+spoken(tick)+")")
	inSection("4. ", "after "+spoken(outage))

	// The reason Monarr writes on a stalled publication quotes the same window.
	short := strings.Replace(spoken(stalled), " hours", " h", 1)
	if !strings.Contains(read(t, reducer), "within "+short+" of its import starting") {
		t.Errorf("%s: the stalled-publication reason no longer says %q; it must match sqlite.DeadlinePublication", reducer, "within "+short+" of its import starting")
	}

	for _, c := range []struct{ literal, file, needle string }{
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
