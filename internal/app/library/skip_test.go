package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
)

func TestSkipMatcherDefaults(t *testing.T) {
	m := newSkipMatcher("")
	// The NAS entries are the ones that matter in practice: they appear in
	// every share on the hardware many homelab libraries run on.
	for _, name := range []string{
		".hidden", "@eaDir", "#recycle", "lost+found", "Extras", "SAMPLE",
		"System Volume Information", "subs",
	} {
		if !m.skip(name) {
			t.Errorf("%q should be skipped by default", name)
		}
	}
	for _, name := range []string{"Arrival (2016)", "Severance", "The Extras Man"} {
		if m.skip(name) {
			t.Errorf("%q is media and must not be skipped", name)
		}
	}
}

func TestSkipMatcherUserPatternsAndGlobs(t *testing.T) {
	m := newSkipMatcher("_incoming\n# a comment\n\n*.tmp\nseason ??")
	for _, name := range []string{"_incoming", "half.tmp", "Season 01"} {
		if !m.skip(name) {
			t.Errorf("%q should match a user pattern", name)
		}
	}
	if m.skip("# a comment") {
		t.Error("comments must not become patterns")
	}
	if m.skip("Dune") {
		t.Error("unrelated names must survive")
	}
}

// A malformed glob must never break a scan — it simply matches nothing.
func TestSkipMatcherToleratesBadPattern(t *testing.T) {
	m := newSkipMatcher("[unclosed")
	if m.skip("Dune") {
		t.Error("a malformed pattern must not swallow everything")
	}
}

// The end-to-end promise: skipped and dismissed directories stop being
// offered, and both are counted so the exclusion is visible.
func TestScanSkipsPatternsAndHonoursDismissals(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	for _, name := range []string{"Arrival (2016)", "@eaDir", ".config", "Home Videos"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}

	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(report.UnmatchedDirs); got != 2 {
		t.Fatalf("unmatched = %d, want 2 (@eaDir and .config skipped): %+v", got, report.UnmatchedDirs)
	}
	if report.SkippedDirs != 2 {
		t.Errorf("skippedDirs = %d, want 2", report.SkippedDirs)
	}

	// Dismiss the one that is not media; it must not come back.
	if err := svc.IgnoreDir(ctx, filepath.Join(root, "Home Videos"), "not a film"); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.UnmatchedDirs) != 1 || after.UnmatchedDirs[0].Name != "Arrival (2016)" {
		t.Fatalf("a dismissal must survive a rescan, got %+v", after.UnmatchedDirs)
	}
	if after.IgnoredDirs != 1 {
		t.Errorf("ignoredDirs = %d, want 1", after.IgnoredDirs)
	}

	// And undoing it brings the candidate back.
	if err := svc.UnignoreDir(ctx, filepath.Join(root, "Home Videos")); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.UnmatchedDirs) != 2 {
		t.Fatalf("undoing a dismissal must restore the candidate, got %+v", restored.UnmatchedDirs)
	}
}

// Dismissing prunes the persisted report immediately, so the list the user
// is looking at reflects the click without waiting for a rescan.
func TestIgnoreDirPrunesTheCurrentReport(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	for _, name := range []string{"Dune (2021)", "Junk"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.IgnoreDir(ctx, filepath.Join(root, "Junk"), ""); err != nil {
		t.Fatal(err)
	}
	report, ok, err := svc.LastScanReport(ctx)
	if err != nil || !ok {
		t.Fatalf("report: ok=%v err=%v", ok, err)
	}
	for _, d := range report.UnmatchedDirs {
		if d.Name == "Junk" {
			t.Fatal("dismissed dir still in the persisted report")
		}
	}
}
