package library

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

type previewProvider struct {
	fakeProvider
	item domain.MediaItem
	err  error
}

func (p previewProvider) PreviewExternal(context.Context, domain.MediaKind, domain.ExternalRef) (domain.MediaItem, error) {
	return p.item, p.err
}

func TestPreviewFreshOwnershipAndAmbiguity(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	item := domain.MediaItem{Kind: domain.KindSeries, Title: "Preview show", IDs: domain.ExternalIDs{TMDB: 100, TVDB: 200, IMDB: "tt1234567"}, Overview: "One.\n\nTwo.", Runtime: 42}
	svc.meta = previewProvider{item: item}
	ref := domain.ExternalRef{Provider: "tmdb", Value: "100"}
	preview, err := svc.PreviewMetadata(ctx, domain.KindSeries, ref)
	if err != nil || preview.Ownership != "absent" || preview.Item.Overview != item.Overview {
		t.Fatalf("initial: %+v, %v", preview, err)
	}
	rows, err := db.ListMediaItems(ctx, domain.KindSeries)
	if err != nil || len(rows) != 0 {
		t.Fatalf("preview persisted data: %v, %v", rows, err)
	}
	// Different rows each claim a different verified key; neither may win.
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Local", IDs: domain.ExternalIDs{TVDB: 200}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err = svc.PreviewMetadata(ctx, domain.KindSeries, ref)
	if err != nil || preview.Ownership != "present" || preview.LibraryItemID != id {
		t.Fatalf("fresh ownership: %+v, %v", preview, err)
	}
	_, err = db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Other", IDs: domain.ExternalIDs{TMDB: 100}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err = svc.PreviewMetadata(ctx, domain.KindSeries, ref)
	if err != nil || preview.Ownership != "ambiguous" || preview.LibraryItemID != 0 || preview.Addability != "conflict" || preview.Item.Runtime != 42 {
		t.Fatalf("ambiguous: %+v, %v", preview, err)
	}
}

func TestPreviewDeduplicatesVerifiedKeys(t *testing.T) {
	svc, db, _ := newService(t)
	item := domain.MediaItem{Kind: domain.KindMovie, Title: "Movie", IDs: domain.ExternalIDs{TMDB: 550, IMDB: "tt0137523"}}
	svc.meta = previewProvider{item: item}
	id, err := db.CreateMediaItem(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.PreviewMetadata(context.Background(), item.Kind, domain.ExternalRef{Provider: "tmdb", Value: "550"})
	if err != nil || got.Ownership != "present" || got.LibraryItemID != id {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPreviewBooksKeepEditionOwnership(t *testing.T) {
	svc, db, _ := newService(t)
	svc.WithBooks(fakeBooks{})
	ctx := context.Background()
	ref := domain.ExternalRef{Provider: "olid", Value: "OL17091839W"}
	before, err := svc.PreviewMetadata(ctx, domain.KindBook, ref)
	if err != nil || before.Ownership != "absent" {
		t.Fatalf("before %+v, %v", before, err)
	}
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindBook, Title: "Book", IDs: domain.ExternalIDs{OLID: ref.Value}, BookType: quality.BookTypeAudiobook, QualityTarget: quality.Quality{Source: quality.SourceM4B}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.PreviewMetadata(ctx, domain.KindBook, ref)
	if err != nil || got.LibraryItemID != id || len(got.BookTypes) != 1 || got.BookTypes[0] != quality.BookTypeAudiobook {
		t.Fatalf("book %+v, %v", got, err)
	}
}

func TestPreviewErrorAndUnsupportedOutcomes(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	ref := domain.ExternalRef{Provider: "tmdb", Value: "550"}
	for _, tc := range []struct {
		name              string
		providerErr, want error
	}{
		{"unavailable", errors.New("offline"), ErrProviderUnavailable},
		{"missing", &ports.RemoteError{Category: ports.RemoteNotFound}, ErrNotFound},
		{"unconfigured", ports.ErrProviderNotConfigured, ports.ErrProviderNotConfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc.meta = previewProvider{err: tc.providerErr}
			_, err := svc.PreviewMetadata(ctx, domain.KindMovie, ref)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	svc.meta = previewProvider{err: &ports.RemoteError{Category: ports.RemoteIdentityConflict}}
	if _, err := svc.PreviewMetadata(ctx, domain.KindMovie, ref); !errors.As(err, new(*IdentityConflictError)) {
		t.Fatalf("conflict: %v", err)
	}
	svc.meta = previewProvider{item: domain.MediaItem{Kind: domain.KindMovie, Title: "Readable", Overview: "Facts"}}
	got, err := svc.PreviewMetadata(ctx, domain.KindMovie, ref)
	if err != nil || got.Addability != "unsupported" || got.Item.Overview != "Facts" {
		t.Fatalf("unsupported %+v %v", got, err)
	}
	for _, invalid := range []domain.ExternalRef{{Provider: "imdb", Value: "tt1234567"}, {Provider: "tmdb", Value: "0"}, {Provider: "tvdb", Value: "12"}} {
		if _, err := svc.PreviewMetadata(ctx, domain.KindMovie, invalid); !errors.Is(err, ErrInvalidExternalID) {
			t.Fatalf("invalid %v: %v", invalid, err)
		}
	}
}
