package library

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

func TestMissingBookEditionCompletesRootlessOrUnmonitoredSetup(t *testing.T) {
	for _, rootless := range []bool{true, false} {
		t.Run(map[bool]string{true: "rootless", false: "destination exists"}[rootless], func(t *testing.T) {
			svc, db, _ := newService(t)
			svc.WithBooks(fakeBooks{})
			ctx := context.Background()
			root, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindBook))
			if err != nil {
				t.Fatal(err)
			}
			first := AddRequest{Kind: domain.KindBook, OLID: "OL17091839W", BookType: quality.BookTypeEbook}
			if !rootless {
				first.RootFolderID = root.ID
			}
			original, err := svc.Add(ctx, first)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Add(ctx, AddRequest{Kind: domain.KindBook, OLID: first.OLID, BookType: quality.BookTypeAudiobook, RootFolderID: root.ID, Monitored: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != original.ID || !got.Monitored || got.Path == "" || got.RootFolderID != root.ID || got.QualityProfileID != original.QualityProfileID {
				t.Fatalf("existing work setup was lost: %+v", got)
			}
			if len(got.Copies) != 1 || got.Copies[0].BookType != quality.BookTypeAudiobook || got.Copies[0].Path != "" || !got.Copies[0].Monitored {
				t.Fatalf("editions must share the restored destination: %+v", got.Copies)
			}
			for _, copy := range []CopyRequest{{BookType: quality.BookTypeEbook, QualityProfileID: quality.EbookProfileID}, {BookType: quality.BookTypeAudiobook, QualityProfileID: quality.AudiobookProfileID}} {
				if _, err := svc.AddCopy(ctx, got.ID, copy); !errors.Is(err, ErrAlreadyExists) {
					t.Fatalf("duplicate edition accepted: %v", err)
				}
			}
			if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindBook, OLID: first.OLID, BookType: "unknown"}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid edition accepted: %v", err)
			}
			items, err := db.ListMediaItems(ctx, domain.KindBook)
			if err != nil || len(items) != 1 {
				t.Fatalf("edition created another work: %+v %v", items, err)
			}
		})
	}
}

func TestCopyDestinationsRejectPrimaryAndExistingCopyFolders(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	firstRoot, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	secondRoot, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: firstRoot.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddCopy(ctx, item.ID, CopyRequest{RootFolderID: firstRoot.ID, QualityProfileID: item.QualityProfileID}); err == nil {
		t.Fatal("copy was allowed to take the primary folder")
	}
	got, err := svc.AddCopy(ctx, item.ID, CopyRequest{RootFolderID: secondRoot.ID, QualityProfileID: item.QualityProfileID})
	if err != nil || len(got.Copies) != 1 {
		t.Fatalf("separate destination: %+v %v", got, err)
	}
	if _, err := svc.AddCopy(ctx, item.ID, CopyRequest{RootFolderID: secondRoot.ID, QualityProfileID: item.QualityProfileID}); err == nil {
		t.Fatal("copy was allowed to take an existing copy folder")
	}
	got, err = svc.Get(ctx, item.ID)
	if err != nil || len(got.Copies) != 1 || got.Path != item.Path {
		t.Fatalf("rejected copies changed destinations: %+v %v", got, err)
	}
}

type previewBookProvider struct {
	fakeBooks
	item domain.MediaItem
	err  error
}

func (p previewBookProvider) GetBook(context.Context, string) (domain.MediaItem, error) {
	return p.item, p.err
}

type fallbackPreviewProvider struct {
	fakeProvider
	item  domain.MediaItem
	err   error
	calls *int
}

func (p fallbackPreviewProvider) PreviewExternal(context.Context, domain.MediaKind, domain.ExternalRef) (domain.MediaItem, error) {
	*p.calls++
	return p.item, p.err
}

func TestTVDBPreviewFallsThroughOnlyUnsupportedProviders(t *testing.T) {
	for _, firstErr := range []error{ports.ErrProviderNotConfigured, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}, &ports.RemoteError{Category: ports.RemoteUnsupportedHydration}, &ports.RemoteError{Category: ports.RemoteNotFound}} {
		svc, _, _ := newService(t)
		firstCalls, finalCalls := 0, 0
		svc.series = []ports.SeriesProvider{fallbackPreviewProvider{err: firstErr, calls: &firstCalls}}
		svc.meta = fallbackPreviewProvider{item: domain.MediaItem{Kind: domain.KindSeries, Title: "Readable fallback", IDs: domain.ExternalIDs{TMDB: 100, TVDB: 200}}, calls: &finalCalls}
		got, err := svc.PreviewMetadata(context.Background(), domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "200"})
		if err != nil || got.Item.Title != "Readable fallback" || got.Ownership != "absent" || firstCalls != 1 || finalCalls != 1 {
			t.Fatalf("fallback: %+v %v (%d,%d)", got, err, firstCalls, finalCalls)
		}
	}
	svc, _, _ := newService(t)
	if _, err := svc.PreviewMetadata(context.Background(), domain.KindMovie, domain.ExternalRef{Provider: "tmdb", Value: "550"}); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Fatalf("missing preview provider: %v", err)
	}
}

func TestBookPreviewErrorsAndUnavailableOwnershipKeepTheirMeaning(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	ref := domain.ExternalRef{Provider: "olid", Value: "OL17091839W"}
	if _, err := svc.PreviewMetadata(ctx, domain.KindBook, ref); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Fatalf("missing book provider: %v", err)
	}
	svc.books = previewBookProvider{err: errors.New("book service offline")}
	if _, err := svc.PreviewMetadata(ctx, domain.KindBook, ref); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("book lookup failure: %v", err)
	}
	svc.books = previewBookProvider{item: domain.MediaItem{Kind: domain.KindBook, IDs: domain.ExternalIDs{OLID: "OL1W"}}}
	if _, err := svc.PreviewMetadata(ctx, domain.KindBook, ref); !errors.As(err, new(*IdentityConflictError)) {
		t.Fatalf("wrong work accepted: %v", err)
	}
	svc.books = fakeBooks{}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := svc.PreviewMetadata(ctx, domain.KindBook, ref)
	if err != nil || got.Ownership != "unknown" || got.Item.Title != "Project Hail Mary" {
		t.Fatalf("storage outage erased readable facts: %+v %v", got, err)
	}
	calls := 0
	svc.meta = fallbackPreviewProvider{item: domain.MediaItem{Kind: domain.KindMovie, Title: "Readable movie", IDs: domain.ExternalIDs{TMDB: 550}}, calls: &calls}
	got, err = svc.PreviewMetadata(ctx, domain.KindMovie, domain.ExternalRef{Provider: "tmdb", Value: "550"})
	if err != nil || got.Ownership != "unknown" || got.Item.Title != "Readable movie" {
		t.Fatalf("movie ownership invented absence: %+v %v", got, err)
	}
}

func (p fallbackPreviewProvider) Name() string { return "fallback-fixture" }
func (p fallbackPreviewProvider) GetSeriesByTVDB(ctx context.Context, id int64) (domain.MediaItem, error) {
	return p.GetSeries(ctx, id)
}
