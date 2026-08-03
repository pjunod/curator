package library

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestSuggestPlacementUsesTheSameDefaultRootAndNameAsAdd(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.SuggestPlacement(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RootFolderID != rf.ID || got.Path != filepath.Join(root, "Fight Club (1999)") {
		t.Fatalf("suggestion = %+v", got)
	}
}
