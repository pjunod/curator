package notify

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/adapters/notify/cinematest"
	"github.com/pjunod/monarr/internal/ports"
)

// The connection test learns Cinema's features (contract amendment 5.3);
// an older Cinema supports none, and the feature probe agrees.
func TestPlurxTestLearnsRemovedPathsFeature(t *testing.T) {
	root := t.TempDir()
	c := cinematest.New(root)
	defer c.Close()
	c.Set(func(c *cinematest.Cinema) { c.RemovedPaths = true })
	p := &Plurx{URL: c.URL(), APIKey: "plx_secret"}
	if _, err := p.TestPaths(context.Background(), []string{root}); err != nil {
		t.Fatal(err)
	}
	if got := p.LearnedFeatures(); len(got) != 1 || got[0] != "removed_paths" {
		t.Fatalf("features = %v, want removed_paths", got)
	}
	c.Set(func(c *cinematest.Cinema) { c.RemovedPaths = false; c.Old = true })
	got, err := p.ProbeFeatures(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("old cinema features = %v %v, want none", got, err)
	}
}

// A Cinema that supports neither offload nor removed paths is not told of an
// Offload (any request would end in a full scan that drops the title): the
// adapter answers with a lasting "not told" result, nothing is posted.
func TestPlurxOffloadToACinemaWithNeitherFeatureIsNotToldAndSaysSo(t *testing.T) {
	root := t.TempDir()
	c := cinematest.New(root)
	defer c.Close()
	p := &Plurx{URL: c.URL(), APIKey: "plx_secret"}
	info := &ports.ImportInfo{OffloadedPaths: []string{root + "/a.mkv"}}
	res, err := p.scan(context.Background(), root, info, "op/x", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.NotTold != NotToldNoOffload || res.State != "done" {
		t.Fatalf("result: %+v", res)
	}
}
