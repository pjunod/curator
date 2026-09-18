package matcher

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
)

// ReleaseEvidence contains only attributes reported by the release itself.
// IDs used to issue a query must never be copied into this structure.
type ReleaseEvidence struct {
	Parsed   parser.Parsed
	IDs      domain.ExternalIDs
	IDIssues []domain.IdentityIssue
}

// MatchDecision explains identity and episode coverage. Acquisition policy
// (quality, size, blocklist, and in-flight state) remains a later decision.
type MatchDecision struct {
	Matched       bool
	Method        string
	Code          string
	Reason        string
	MatchedTitle  string
	MatchedID     *domain.ExternalRef
	Warnings      []string
	Country       string
	ConventionIDs []int64
	SnapshotTimes []time.Time
}

// IndexedWork is one distinct video work. Episodes and quality copies of a
// media item intentionally share it.
type IndexedWork struct {
	ItemID   int64
	Kind     domain.MediaKind
	Source   string
	Identity domain.MediaIdentity
}

// IdentityIndex is an immutable all-library snapshot. Now is captured by the
// operation so age-based freshness changes even without a database revision.
type IdentityIndex struct {
	Works map[int64]IndexedWork
	Now   time.Time
}

// NewIdentityIndex builds the matching view from all video library items,
// including unmonitored and already-complete works.
func NewIdentityIndex(items []domain.MediaItem, now time.Time) IdentityIndex {
	idx := IdentityIndex{Works: make(map[int64]IndexedWork, len(items)), Now: now}
	for _, item := range items {
		if item.Kind == domain.KindBook {
			continue
		}
		idx.Works[item.ID] = IndexedWork{
			ItemID: item.ID, Kind: item.Kind, Source: item.Source,
			Identity: domain.MediaIdentity{
				IDs: item.IDs, Title: item.Title, Year: item.Year,
				Aliases:   append([]domain.TitleAlias(nil), item.Aliases...),
				Countries: append([]domain.CountryEvidence(nil), item.Countries...),
				Sources:   append([]domain.IdentitySourceStatus(nil), item.IdentitySources...),
			},
		}
	}
	return idx
}

type titleHit struct {
	itemID  int64
	tier    int
	title   string
	method  string
	country string
}

// Evaluate applies hard conflicts, ranked title evidence, exact IDs,
// ambiguity resolution, and finally requested coverage. It performs no I/O.
func Evaluate(e ReleaseEvidence, want domain.Wantable, index IdentityIndex) MatchDecision {
	target := identityOf(want)
	if target.Title == "" {
		return rejected("identity_unresolved", "Target has no usable identity metadata")
	}
	if index.Now.IsZero() {
		index.Now = time.Now()
	}
	if index.Works == nil {
		index.Works = map[int64]IndexedWork{}
	}
	if _, ok := index.Works[want.MediaItemID()]; !ok {
		index.Works[want.MediaItemID()] = IndexedWork{
			ItemID: want.MediaItemID(), Kind: kindOf(want), Identity: target,
		}
	}

	var warnings []string
	for _, issue := range e.IDIssues {
		if issue.Code == "conflicting_ids" {
			return rejected("id_conflict", fmt.Sprintf(
				"Release reports conflicting %s IDs", strings.ToUpper(issue.Provider)))
		}
		warnings = append(warnings, fmt.Sprintf("ignored malformed %s id", issue.Provider))
	}
	if blockedAlias(e.Parsed, target.Aliases) {
		return rejected("numbering_scope_conflict",
			"This alias names a provider grouping whose episode numbering cannot be mapped")
	}

	rawTitle := e.Parsed.RawTitle
	if rawTitle == "" {
		rawTitle = e.Parsed.Title
	}
	variants := TitleVariants(rawTitle)
	for _, variant := range variants {
		if variant.Conflict {
			return rejected("country_conflict", "Release contains conflicting country qualifiers")
		}
	}

	literalKey := NormalizeTitle(e.Parsed.Title)
	if literalKey == "" {
		literalKey = NormalizeTitle(rawTitle)
	}
	if !yearCompatible(kindOf(want), e.Parsed, target.Year) {
		actual := e.Parsed.Year
		if kindOf(want) == domain.KindSeries {
			actual = e.Parsed.SeriesTitleYear
		}
		return rejected("year_conflict", fmt.Sprintf("Release year %d differs from library year %d", actual, target.Year))
	}
	titleHits := collectTitleHits(index, kindOf(want), literalKey, variants, e.Parsed)
	idItems, matchedID, idErr := resolveReleaseIDs(index, kindOf(want), e.IDs)
	if idErr != "" {
		return rejected("id_conflict", idErr)
	}

	selection := selectIdentity(index, titleHits, idItems, variants)
	if selection.code != "" {
		decision := rejected(selection.code, selection.reason)
		decision.Warnings = warnings
		return decision
	}
	if selection.itemID == 0 {
		decision := rejected("title_mismatch", fmt.Sprintf(
			"Release title %q does not match %q or its known aliases",
			e.Parsed.Title, target.Title))
		decision.Warnings = warnings
		return decision
	}
	if selection.itemID != want.MediaItemID() {
		code := "ambiguous_identity"
		if len(idItems) == 1 {
			code = "id_conflict"
		}
		decision := rejected(code, "Release identity selects a different library item")
		decision.Warnings = warnings
		return decision
	}
	if conflict := targetIDConflict(e.IDs, target.IDs); conflict != "" {
		return rejected("id_conflict", conflict)
	}
	releaseCountry := countryForTarget(variants, target)
	if conflict := countryConflict(target, releaseCountry); conflict != "" {
		return rejected("country_conflict", conflict)
	}
	if code, reason := coverageDecision(e.Parsed, want); code != "" {
		decision := rejected(code, reason)
		decision.Warnings = warnings
		return decision
	}

	decision := MatchDecision{
		Matched: true, Method: selection.method,
		Reason:       successReason(selection.method, selection.matchedTitle, matchedID),
		MatchedTitle: selection.matchedTitle, MatchedID: matchedID,
		Warnings: warnings, Country: releaseCountry,
		ConventionIDs: selection.conventionIDs,
		SnapshotTimes: selection.snapshotTimes,
	}
	return decision
}

func countryForTarget(variants []TitleVariant, identity domain.MediaIdentity) string {
	canonical := NormalizeTitle(identity.Title)
	for _, variant := range variants {
		if variant.Country == "" || variant.Conflict {
			continue
		}
		if variant.Explicit {
			return variant.Country
		}
		base := NormalizeTitle(variant.Base)
		if base != canonical {
			continue
		}
		if qualifierSupported(identity, variant.Country, base) || hasAuthoritativeCountry(identity) {
			return variant.Country
		}
	}
	return ""
}

func hasAuthoritativeCountry(identity domain.MediaIdentity) bool {
	for _, evidence := range identity.Countries {
		if evidence.Basis == "origin" || evidence.Basis == "manual" || evidence.Basis == "title_qualifier" {
			return true
		}
	}
	return false
}

func collectTitleHits(index IdentityIndex, kind domain.MediaKind, literal string, variants []TitleVariant, parsed parser.Parsed) []titleHit {
	best := map[int64]titleHit{}
	add := func(hit titleHit) {
		old, exists := best[hit.itemID]
		if !exists || hit.tier < old.tier {
			best[hit.itemID] = hit
		}
	}
	for itemID, work := range index.Works {
		if work.Kind != kind || !yearCompatible(kind, parsed, work.Identity.Year) {
			continue
		}
		if key := NormalizeTitle(work.Identity.Title); key != "" && key == literal {
			add(titleHit{itemID: itemID, tier: 1, title: work.Identity.Title, method: "title"})
		}
		for _, alias := range work.Identity.Aliases {
			if alias.Scope != "work" {
				continue
			}
			key := NormalizeTitle(alias.Title)
			if key == "" || key != literal {
				continue
			}
			tier := 3
			if alias.Source == "manual" || alias.Role == "manual" {
				tier = 2
			}
			add(titleHit{itemID: itemID, tier: tier, title: alias.Title, method: "alias"})
		}
		for _, variant := range variants {
			if variant.Rule == "literal" || variant.Country == "" || variant.Conflict {
				continue
			}
			base := NormalizeTitle(variant.Base)
			if base == "" || NormalizeTitle(work.Identity.Title) != base {
				continue
			}
			if qualifierSupported(work.Identity, variant.Country, base) {
				add(titleHit{itemID: itemID, tier: 4, title: work.Identity.Title,
					method: "country", country: variant.Country})
			}
		}
	}
	hits := make([]titleHit, 0, len(best))
	for _, hit := range best {
		hits = append(hits, hit)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].tier == hits[j].tier {
			return hits[i].itemID < hits[j].itemID
		}
		return hits[i].tier < hits[j].tier
	})
	return hits
}

type identitySelection struct {
	itemID        int64
	method        string
	matchedTitle  string
	country       string
	conventionIDs []int64
	snapshotTimes []time.Time
	code          string
	reason        string
}

func selectIdentity(index IdentityIndex, hits []titleHit, idItems []int64, variants []TitleVariant) identitySelection {
	if len(idItems) == 1 {
		for _, hit := range hits {
			if hit.itemID == idItems[0] {
				return identitySelection{itemID: hit.itemID, method: hit.method,
					matchedTitle: hit.title, country: hit.country}
			}
		}
		return identitySelection{itemID: idItems[0], method: "id", country: variantCountry(variants)}
	}
	if len(hits) == 0 {
		return identitySelection{}
	}
	bestTier := hits[0].tier
	var tied []titleHit
	for _, hit := range hits {
		if hit.tier == bestTier {
			tied = append(tied, hit)
		}
	}
	if len(tied) == 1 {
		hit := tied[0]
		return identitySelection{itemID: hit.itemID, method: hit.method,
			matchedTitle: hit.title, country: hit.country}
	}
	if len(tied) == 2 && bestTier == 1 && variantCountry(variants) == "" {
		return originalConvention(index, tied)
	}
	return identitySelection{code: "ambiguous_identity",
		reason: "This name identifies multiple library items; a matching ID is needed"}
}

func originalConvention(index IdentityIndex, tied []titleHit) identitySelection {
	qualified := [2]bool{}
	qualifiedAlias := ""
	var snapshots []time.Time
	for i, hit := range tied {
		identity := index.Works[hit.itemID].Identity
		if !snapshotsComplete(identity, index.Now) {
			return identitySelection{code: "ambiguous_identity",
				reason: "Regional naming convention cannot be used with incomplete or stale identity metadata"}
		}
		for _, source := range identity.Sources {
			snapshots = append(snapshots, source.FetchedAt)
		}
		for _, alias := range identity.Aliases {
			if alias.Scope != "work" {
				continue
			}
			for _, variant := range TitleVariants(alias.Title) {
				if variant.Country != "" && NormalizeTitle(variant.Base) == NormalizeTitle(identity.Title) {
					qualified[i] = true
					qualifiedAlias = alias.Title
				}
			}
		}
	}
	if qualified[0] == qualified[1] {
		return identitySelection{code: "ambiguous_identity",
			reason: "Both regional works have the same qualifier evidence"}
	}
	pick := 0
	if qualified[0] {
		pick = 1
	}
	return identitySelection{
		itemID: tied[pick].itemID, method: "convention", matchedTitle: qualifiedAlias,
		conventionIDs: []int64{tied[0].itemID, tied[1].itemID}, snapshotTimes: snapshots,
	}
}

func resolveReleaseIDs(index IdentityIndex, kind domain.MediaKind, ids domain.ExternalIDs) ([]int64, *domain.ExternalRef, string) {
	selected := map[int64]bool{}
	var matched *domain.ExternalRef
	for _, candidate := range []domain.ExternalRef{
		{Provider: "tvdb", Value: intString(ids.TVDB)},
		{Provider: "imdb", Value: strings.ToLower(ids.IMDB)},
		{Provider: "tmdb", Value: intString(ids.TMDB)},
	} {
		if candidate.Value == "" {
			continue
		}
		var found []int64
		for itemID, work := range index.Works {
			if work.Kind == kind && externalValue(work.Identity.IDs, candidate.Provider) == candidate.Value {
				found = append(found, itemID)
			}
		}
		if len(found) > 1 {
			return nil, nil, fmt.Sprintf("%s ID identifies multiple library items", strings.ToUpper(candidate.Provider))
		}
		if len(found) == 1 {
			selected[found[0]] = true
			copy := candidate
			matched = &copy
		}
	}
	if len(selected) > 1 {
		return nil, nil, "Release IDs identify different library items"
	}
	items := make([]int64, 0, len(selected))
	for itemID := range selected {
		items = append(items, itemID)
	}
	return items, matched, ""
}

func identityOf(want domain.Wantable) domain.MediaIdentity {
	switch target := want.(type) {
	case domain.MovieWantable:
		if target.Identity.Title != "" {
			return target.Identity
		}
		return domain.MediaIdentity{Title: target.Title, Year: target.Year}
	case domain.EpisodeWantable:
		if target.Identity.Title != "" {
			return target.Identity
		}
		return domain.MediaIdentity{Title: target.Title, Year: target.Year}
	case domain.SeasonWantable:
		if target.Identity.Title != "" {
			return target.Identity
		}
		return domain.MediaIdentity{Title: target.Title, Year: target.Year}
	}
	return domain.MediaIdentity{}
}

func kindOf(want domain.Wantable) domain.MediaKind {
	switch want.(type) {
	case domain.MovieWantable:
		return domain.KindMovie
	case domain.EpisodeWantable, domain.SeasonWantable:
		return domain.KindSeries
	default:
		return domain.KindBook
	}
}

func qualifierSupported(identity domain.MediaIdentity, country, base string) bool {
	for _, evidence := range identity.Countries {
		if evidence.Code == country && (evidence.Basis == "origin" ||
			evidence.Basis == "manual" || evidence.Basis == "title_qualifier") {
			return true
		}
	}
	for _, alias := range identity.Aliases {
		if alias.Scope != "work" {
			continue
		}
		for _, variant := range TitleVariants(alias.Title) {
			if variant.Country == country && NormalizeTitle(variant.Base) == base {
				return true
			}
		}
	}
	return false
}

func countryConflict(identity domain.MediaIdentity, releaseCountry string) string {
	if releaseCountry == "" {
		return ""
	}
	known := map[string]bool{}
	for _, evidence := range identity.Countries {
		if evidence.Basis == "origin" || evidence.Basis == "manual" || evidence.Basis == "title_qualifier" {
			known[evidence.Code] = true
		}
	}
	if len(known) == 1 && !known[releaseCountry] {
		for country := range known {
			return fmt.Sprintf("Release specifies %s; library item specifies %s", releaseCountry, country)
		}
	}
	return ""
}

func blockedAlias(parsed parser.Parsed, aliases []domain.TitleAlias) bool {
	keys := []string{NormalizeTitle(parsed.Title), NormalizeTitle(parsed.RawTitle)}
	for _, alias := range aliases {
		if alias.Scope == "work" {
			continue
		}
		key := NormalizeTitle(alias.Title)
		if key == "" {
			continue
		}
		for _, candidate := range keys {
			if candidate == key {
				return true
			}
		}
	}
	return false
}

func snapshotsComplete(identity domain.MediaIdentity, now time.Time) bool {
	if len(identity.Sources) == 0 {
		return false
	}
	for _, source := range identity.Sources {
		if !source.CompleteAt(now) {
			return false
		}
	}
	return true
}

func yearCompatible(kind domain.MediaKind, parsed parser.Parsed, year int) bool {
	if year == 0 {
		return true
	}
	if kind == domain.KindSeries {
		return parsed.SeriesTitleYear == 0 || parsed.SeriesTitleYear == year
	}
	if parsed.Year == 0 {
		return true
	}
	delta := parsed.Year - year
	return delta >= -1 && delta <= 1
}

func targetIDConflict(release, target domain.ExternalIDs) string {
	for _, provider := range []string{"tvdb", "imdb", "tmdb"} {
		actual, expected := externalValue(release, provider), externalValue(target, provider)
		if actual != "" && expected != "" && actual != expected {
			return fmt.Sprintf("Release %s ID %s differs from library %s",
				strings.ToUpper(provider), actual, expected)
		}
	}
	return ""
}

func coverageDecision(parsed parser.Parsed, want domain.Wantable) (string, string) {
	if parsed.Daily != "" {
		return "coverage_unsupported", "Date-named episodes are not supported by this matching path"
	}
	switch target := want.(type) {
	case domain.MovieWantable:
		if len(parsed.Episodes) > 0 || parsed.SeasonPack {
			return "episode_mismatch", "Release contains episode coverage for a movie"
		}
	case domain.EpisodeWantable:
		if parsed.SeasonPack && parsed.Season == target.Season {
			return "", ""
		}
		if len(parsed.Absolute) > 0 && target.Absolute > 0 {
			for _, absolute := range parsed.Absolute {
				if absolute == target.Absolute {
					return "", ""
				}
			}
			return "episode_mismatch", fmt.Sprintf("Release does not contain absolute episode %d", target.Absolute)
		}
		if parsed.Season != target.Season {
			return "episode_mismatch", fmt.Sprintf("Release season %d does not match season %d", parsed.Season, target.Season)
		}
		for _, episode := range parsed.Episodes {
			if episode == target.Episode {
				return "", ""
			}
		}
		return "episode_mismatch", fmt.Sprintf("Release does not contain S%02dE%02d", target.Season, target.Episode)
	case domain.SeasonWantable:
		if !parsed.SeasonPack || parsed.Season != target.Season {
			return "episode_mismatch", fmt.Sprintf("Release is not season %d", target.Season)
		}
	}
	return "", ""
}

func variantCountry(variants []TitleVariant) string {
	for _, variant := range variants {
		if variant.Country != "" {
			return variant.Country
		}
	}
	return ""
}

func externalValue(ids domain.ExternalIDs, provider string) string {
	switch provider {
	case "tvdb":
		return intString(ids.TVDB)
	case "imdb":
		return strings.ToLower(ids.IMDB)
	case "tmdb":
		return intString(ids.TMDB)
	default:
		return ""
	}
}

func intString(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func rejected(code, reason string) MatchDecision {
	return MatchDecision{Code: code, Reason: reason}
}

func successReason(method, title string, ref *domain.ExternalRef) string {
	switch method {
	case "alias":
		return fmt.Sprintf("Matched alternate title: %s", title)
	case "country":
		return "Matched a verified country title variant"
	case "convention":
		return "Unqualified title assigned to the original under the known regional naming convention"
	case "id":
		if ref != nil {
			return fmt.Sprintf("Matched %s ID %s; release title differs", strings.ToUpper(ref.Provider), ref.Value)
		}
	}
	return "Matched canonical title"
}
