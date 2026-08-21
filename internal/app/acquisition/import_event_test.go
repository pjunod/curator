package acquisition

import (
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestImportedBookEventCarriesStableEditionMetadata(t *testing.T) {
	item := domain.MediaItem{
		ID: 84, Kind: domain.KindBook, Title: "The Dispossessed",
		Author: "Ursula K. Le Guin", BookType: quality.BookTypeEbook,
		IDs:        domain.ExternalIDs{OLID: "OL87320W"},
		PosterPath: "https://covers.openlibrary.org/b/id/123-L.jpg",
		Copies: []domain.MediaCopy{{
			ID: 91, MediaItemID: 84, BookType: quality.BookTypeAudiobook,
		}},
	}
	result := ImportResult{Imported: 1, Files: []FileOutcome{{
		Imported: true, Path: "/media/books/Ursula K. Le Guin/The Dispossessed/book.epub",
	}}}

	ebook := importedEvent(item, sqlite.Download{ID: 1}, result)
	if ebook.BookTitle != item.Title || ebook.BookAuthor != item.Author || ebook.BookMedium != "ebook" {
		t.Fatalf("ebook facts = title %q author %q medium %q",
			ebook.BookTitle, ebook.BookAuthor, ebook.BookMedium)
	}
	if ebook.BookWorkID != "curator:openlibrary:OL87320W" ||
		ebook.BookEditionID != "curator:item:84:ebook" {
		t.Fatalf("ebook identity = work %q edition %q", ebook.BookWorkID, ebook.BookEditionID)
	}
	if ebook.BookCoverURL != item.PosterPath {
		t.Fatalf("cover = %q", ebook.BookCoverURL)
	}

	audio := importedEvent(item, sqlite.Download{ID: 2, CopyID: 91}, result)
	if audio.BookMedium != "audiobook" || audio.BookWorkID != ebook.BookWorkID ||
		audio.BookEditionID != "curator:item:84:audiobook" {
		t.Fatalf("audiobook identity = medium %q work %q edition %q",
			audio.BookMedium, audio.BookWorkID, audio.BookEditionID)
	}
}

func TestImportedBookEventBoundsProviderFacts(t *testing.T) {
	item := domain.MediaItem{
		ID: 84, Kind: domain.KindBook, Title: "Manual\x00book",
		Author:     strings.Repeat("a", 513),
		BookType:   quality.BookTypeEbook,
		IDs:        domain.ExternalIDs{OLID: "not/a/work"},
		PosterPath: "https://user@covers.openlibrary.org/b/id/123-L.jpg",
	}
	event := importedEvent(item, sqlite.Download{}, ImportResult{})
	if event.BookWorkID != "curator:item:84" {
		t.Errorf("unsafe OLID became identity: %q", event.BookWorkID)
	}
	if event.BookCoverURL != "" {
		t.Errorf("credential-bearing cover escaped the sender boundary: %q", event.BookCoverURL)
	}
	if event.BookTitle != "" || event.BookAuthor != "" {
		t.Errorf("unbounded display facts escaped the sender boundary: title %q author %q",
			event.BookTitle, event.BookAuthor)
	}

	item.PosterPath = "https://covers.openlibrary.org:443/b/id/123-L.jpg"
	if got := importedEvent(item, sqlite.Download{}, ImportResult{}).BookCoverURL; got != "" {
		t.Errorf("explicit-port cover escaped the sender boundary: %q", got)
	}
}
