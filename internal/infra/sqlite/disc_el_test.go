package sqlite

import (
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

// The disc master record (ADR 0026 §6): MEL holds only until an FEL is seen,
// and an FEL is never overwritten — a MEL remux of an FEL disc is a different
// master, not news about the disc.
func TestDiscEnhancementLayerIsStickyForFEL(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Title", Year: 2021, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		m, err := db.GetMediaItemFull(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return m.DiscEL
	}
	if got := read(); got != "" {
		t.Fatalf("a new title has disc master %q", got)
	}
	steps := []struct{ el, want string }{
		{"none", ""}, // not a profile 7 classification: ignored
		{"mel", "mel"},
		{"fel", "fel"},
		{"mel", "fel"},
	}
	for _, s := range steps {
		if err := db.RecordDiscEnhancementLayer(ctx, id, s.el); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != s.want {
			t.Fatalf("after recording %q: disc master %q, want %q", s.el, got, s.want)
		}
	}
}
