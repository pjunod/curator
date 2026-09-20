package library

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// Preview is transient display metadata, never an AddRequest or a persisted item.
type Preview struct {
	Item           domain.MediaItem
	Ownership      string
	LibraryItemID  int64
	BookTypes      []quality.BookType
	Addability     string
	AddBlockReason string
}

var previewWorkID = regexp.MustCompile(`^OL[0-9]+W$`)

// PreviewMetadata performs bounded, read-only lookup. Local ownership is never
// cached with provider data and cannot make otherwise readable facts disappear.
func (s *Service) PreviewMetadata(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) (Preview, error) {
	if !validPreviewRef(kind, ref) {
		return Preview{}, ErrInvalidExternalID
	}
	item, err := s.previewRecord(ctx, kind, ref)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Item: item, Ownership: "absent", Addability: "supported"}
	if kind != domain.KindBook && !addable(resultFromItem(item, item.Source)) {
		out.Addability, out.AddBlockReason = "unsupported", "This provider cannot supply a supported record for adding."
	}
	s.previewOwnership(ctx, ref, &out)
	return out, nil
}

func validPreviewRef(kind domain.MediaKind, ref domain.ExternalRef) bool {
	if kind == domain.KindBook {
		return ref.Provider == "olid" && previewWorkID.MatchString(ref.Value)
	}
	if kind != domain.KindMovie && kind != domain.KindSeries {
		return false
	}
	if ref.Provider != "tmdb" && (kind != domain.KindSeries || ref.Provider != "tvdb") {
		return false
	}
	id, err := strconv.ParseInt(ref.Value, 10, 64)
	return err == nil && id > 0
}

func (s *Service) previewRecord(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) (domain.MediaItem, error) {
	if kind == domain.KindBook {
		if s.books == nil {
			return domain.MediaItem{}, ports.ErrProviderNotConfigured
		}
		item, err := s.books.GetBook(ctx, ref.Value)
		if err != nil {
			return domain.MediaItem{}, previewError(err)
		}
		if item.IDs.OLID != ref.Value {
			return domain.MediaItem{}, &IdentityConflictError{Cause: errors.New("provider returned a different work")}
		}
		item.Source = "openlibrary"
		return item, nil
	}
	var providers []ports.PreviewProvider
	if kind == domain.KindSeries && ref.Provider == "tvdb" {
		for _, candidate := range s.series {
			if p, ok := candidate.(ports.PreviewProvider); ok {
				providers = append(providers, p)
			}
		}
	}
	if p, ok := s.meta.(ports.PreviewProvider); ok {
		providers = append(providers, p)
	}
	if len(providers) == 0 {
		return domain.MediaItem{}, ports.ErrProviderNotConfigured
	}
	last := ErrNotFound
	for _, p := range providers {
		item, err := p.PreviewExternal(ctx, kind, ref)
		if err == nil {
			return item, nil
		}
		last = previewError(err)
		var remote *ports.RemoteError
		if errors.Is(err, ports.ErrProviderNotConfigured) {
			continue
		}
		if errors.As(err, &remote) && (remote.Category == ports.RemoteNotFound || remote.Category == ports.RemoteUnsupportedQuery || remote.Category == ports.RemoteUnsupportedHydration) {
			continue
		}
		return domain.MediaItem{}, last
	}
	return domain.MediaItem{}, last
}

func previewError(err error) error {
	if errors.Is(err, ports.ErrProviderNotConfigured) || errors.Is(err, ErrNotFound) {
		return err
	}
	var remote *ports.RemoteError
	if errors.As(err, &remote) {
		switch remote.Category {
		case ports.RemoteNotFound:
			return ErrNotFound
		case ports.RemoteIdentityConflict:
			return &IdentityConflictError{Cause: err}
		case ports.RemoteUnsupportedQuery, ports.RemoteUnsupportedHydration:
			return ErrUnsupportedHydration
		}
	}
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}

func (s *Service) previewOwnership(ctx context.Context, ref domain.ExternalRef, out *Preview) {
	matches := map[int64]domain.MediaItem{}
	if out.Item.Kind == domain.KindBook {
		id, err := s.db.GetMediaItemByKindOlid(ctx, domain.KindBook, ref.Value)
		if errors.Is(err, ErrNotFound) {
			return
		}
		if err != nil {
			out.Ownership = "unknown"
			return
		}
		item, err := s.Get(ctx, id)
		if err != nil {
			out.Ownership = "unknown"
			return
		}
		matches[id] = item
	} else {
		refs := []domain.ExternalRef{ref}
		ids := out.Item.IDs
		if ids.TMDB > 0 {
			refs = append(refs, domain.ExternalRef{Provider: "tmdb", Value: strconv.FormatInt(ids.TMDB, 10)})
		}
		if ids.TVDB > 0 {
			refs = append(refs, domain.ExternalRef{Provider: "tvdb", Value: strconv.FormatInt(ids.TVDB, 10)})
		}
		if ids.IMDB != "" {
			refs = append(refs, domain.ExternalRef{Provider: "imdb", Value: ids.IMDB})
		}
		for _, r := range refs {
			items, err := s.db.FindMediaItemsByExternalID(ctx, out.Item.Kind, r)
			if err != nil {
				out.Ownership = "unknown"
				return
			}
			for _, item := range items {
				matches[item.ID] = item
			}
		}
	}
	if len(matches) == 0 {
		return
	}
	for _, item := range matches {
		if len(matches) > 1 || previewIDsConflict(out.Item.IDs, item.IDs) {
			out.Ownership, out.Addability, out.AddBlockReason = "ambiguous", "conflict", "Multiple or conflicting library identities match this title. Resolve the conflict before adding."
			return
		}
		out.Ownership, out.LibraryItemID = "present", item.ID
		if item.Kind == domain.KindBook {
			seen := map[quality.BookType]bool{}
			for _, copy := range item.Copies {
				if quality.ValidBookType(copy.BookType) {
					seen[copy.BookType] = true
				}
			}
			if quality.ValidBookType(item.BookType) {
				seen[item.BookType] = true
			}
			for _, edition := range []quality.BookType{quality.BookTypeEbook, quality.BookTypeAudiobook} {
				if seen[edition] {
					out.BookTypes = append(out.BookTypes, edition)
				}
			}
		}
	}
}

func previewIDsConflict(a, b domain.ExternalIDs) bool {
	return a.TMDB > 0 && b.TMDB > 0 && a.TMDB != b.TMDB || a.TVDB > 0 && b.TVDB > 0 && a.TVDB != b.TVDB || a.IMDB != "" && b.IMDB != "" && !strings.EqualFold(a.IMDB, b.IMDB)
}
