package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// The default quality profile a newly added item gets, per media kind.
//
// This used to be a constant in library.Add: profile 1 for everything, except
// books, which got the Ebook profile. That is a defensible starting point and
// an indefensible permanent answer — somebody who wants 4K movies had to edit
// every title after adding it, one at a time, with no setting anywhere that
// said what was going to happen.
//
// One default per kind rather than one global default, because the two axes
// are genuinely different: a book cannot share a video profile at all (format
// families never compete, ADR 0014 §7), and wanting 4K films alongside 1080p
// television is the ordinary case rather than an exotic one.
const defaultProfilePrefix = "default_profile."

// DefaultProfileSetting is the app_meta key holding the default for a kind.
// Exported so tests and the settings handler name the same string monarr does.
func DefaultProfileSetting(kind domain.MediaKind) string {
	return defaultProfilePrefix + string(kind)
}

// ErrProfileFamilyMismatch is returned when a default would pair a kind with a
// profile on the wrong format axis — a book defaulting to "1080p", say. The
// grab would then be impossible rather than merely unlikely: nothing an ebook
// indexer returns can satisfy a video target, so the item would sit wanted
// forever with no visible cause.
var ErrProfileFamilyMismatch = errors.New("quality profile is for a different kind of media")

// ErrProfileIsDefault is returned when deleting a profile some kind still
// defaults to. Same reasoning as ErrProfileInUse: the reference is real even
// though no row points at it, and letting the delete through would silently
// re-route every future add to a profile id that no longer exists.
var ErrProfileIsDefault = errors.New("quality profile is a default for new items")

// fallbackDefault is the built-in used when nothing is configured, or when what
// was configured has since been deleted. It reproduces the pre-setting
// behaviour exactly, so an upgrade changes nothing until somebody chooses.
func fallbackDefault(kind domain.MediaKind) int64 {
	if kind == domain.KindBook {
		return quality.EbookProfileID
	}
	return 1
}

// DefaultProfileID returns the profile a new item of this kind should get.
//
// Never fails the caller over a preference: an unreadable, unset, malformed,
// or dangling setting all resolve to the built-in. Adding a movie must not
// break because a profile was deleted out from under a setting.
func (d *DB) DefaultProfileID(ctx context.Context, kind domain.MediaKind) int64 {
	raw, err := d.GetMeta(ctx, DefaultProfileSetting(kind))
	if err != nil {
		return fallbackDefault(kind)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return fallbackDefault(kind)
	}
	if _, err := d.GetProfile(ctx, id); err != nil {
		return fallbackDefault(kind)
	}
	return id
}

// DefaultProfiles returns the resolved default for every kind, which is what
// the settings screen and the add form both need: the effective answer,
// including the built-in fallbacks, not just the rows somebody has saved.
func (d *DB) DefaultProfiles(ctx context.Context) map[domain.MediaKind]int64 {
	out := make(map[domain.MediaKind]int64, 3)
	for _, k := range []domain.MediaKind{domain.KindMovie, domain.KindSeries, domain.KindBook} {
		out[k] = d.DefaultProfileID(ctx, k)
	}
	return out
}

// SetDefaultProfile points a kind at a profile, refusing a profile that does
// not exist or lives on the wrong format axis.
func (d *DB) SetDefaultProfile(ctx context.Context, kind domain.MediaKind, id int64) error {
	if !domain.ValidKind(kind) {
		return fmt.Errorf("unknown media kind %q", kind)
	}
	p, err := d.GetProfile(ctx, id)
	if err != nil {
		return fmt.Errorf("quality profile: %w", err)
	}
	if err := checkFamily(kind, p); err != nil {
		return err
	}
	return d.SetMeta(ctx, DefaultProfileSetting(kind), strconv.FormatInt(id, 10))
}

// checkFamily refuses a book default on a video profile and vice versa.
func checkFamily(kind domain.MediaKind, p quality.Profile) error {
	wantBook := kind == domain.KindBook
	if quality.IsBookFormat(p.Target.Source) == wantBook {
		return nil
	}
	noun := "video"
	if !wantBook {
		noun = "book"
	}
	return fmt.Errorf("%w: %q targets %s, and %s items cannot use it",
		ErrProfileFamilyMismatch, p.Name, noun, kind)
}

// defaultingKinds lists the kinds whose stored setting names this profile.
// Only explicit settings count: a profile that merely happens to be the
// built-in fallback is not a reference, and refusing to delete profile 1 on
// that basis would be a rule nobody asked for.
func (d *DB) defaultingKinds(ctx context.Context, id int64) []domain.MediaKind {
	var out []domain.MediaKind
	want := strconv.FormatInt(id, 10)
	for _, k := range []domain.MediaKind{domain.KindMovie, domain.KindSeries, domain.KindBook} {
		if raw, err := d.GetMeta(ctx, DefaultProfileSetting(k)); err == nil && raw == want {
			out = append(out, k)
		}
	}
	return out
}
