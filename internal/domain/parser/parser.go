// Package parser is the release-name parser — the single highest-risk
// component in the system (blueprint §4.3). Pure, table-driven, golden-
// tested against the corpus in testdata/releases/, and fuzzed: Parse must
// never panic on arbitrary bytes.
package parser

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/monarr-media/monarr/internal/domain/quality"
)

// Parsed is the structured reading of a release title.
type Parsed struct {
	Title    string // cleaned series/movie title
	Year     int    // movie year or series year hint; 0 unknown
	Season   int    // -1 = none
	Episodes []int  // empty for movies and season packs
	// SeasonPack: a season marker with no episode ("Show S02", "Season 2
	// Complete") — one download satisfying many episode wantables.
	SeasonPack bool
	Daily      string // "2024-01-15" for date-based releases
	// Absolute holds anime-style absolute episode numbers
	// ("[Group] Show - 15" → [15]); empty otherwise.
	Absolute []int
	// Author is set for book-format releases (ADR 0006) when the name
	// carries one ("Author - Title ... EPUB", "Title by Author ... M4B").
	Author  string
	Quality quality.Quality
	Proper  bool
	Repack  bool
	Codec   string
	Group   string
}

var (
	reBracketPrefix = regexp.MustCompile(`^\[([^\[\]]{2,40})\]\s*`)
	reDaily         = regexp.MustCompile(`\b((?:19|20)\d{2})[ ._-](\d{2})[ ._-](\d{2})\b`)
	reSxxEyy        = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]?E(\d{1,3})((?:[ ._-]?E\d{1,3}|-E?\d{1,3})*)\b`)
	reEpCont        = regexp.MustCompile(`(?i)[E-](\d{1,3})`)
	reNxx           = regexp.MustCompile(`\b(\d{1,2})x(\d{2,3})(?:-(\d{2,3}))?\b`)
	reSeasonOnly    = regexp.MustCompile(`(?i)\b(?:S(\d{1,2})|Season[ ._-]?(\d{1,2}))(?:[ ._-]?(?:Complete|COMPLETE))?\b`)
	// Anime absolute numbering: " - 15", " - 015v2", " - 15-16" after the
	// title (only trusted when a [Group] prefix marked the release as anime).
	reAbsolute    = regexp.MustCompile(`\s-\s(\d{1,4})(?:-(\d{1,4}))?(?:v\d)?\s*(?:\[|$|\()`)
	reYear        = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	reResolution  = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4k|uhd)\b`)
	reProper      = regexp.MustCompile(`(?i)\bPROPER\b`)
	reRepack      = regexp.MustCompile(`(?i)\b(?:REPACK|RERIP)\b`)
	reCodec       = regexp.MustCompile(`(?i)\b(x264|x265|h\.?264|h\.?265|HEVC|XviD|AV1)\b`)
	reContainer   = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|m4v|ts|wmv)$`)
	reGroupSuffix = regexp.MustCompile(`-([A-Za-z0-9][A-Za-z0-9_]{1,24})$`)
	reCleanJunk   = regexp.MustCompile(`[._]+`)
	reMultiSpace  = regexp.MustCompile(`\s{2,}`)
)

// sourcePatterns are matched in order; first hit wins. Book formats come
// first: their tokens are unambiguous, while a book release like
// "Author - Title (Retail) EPUB" must not fall through to video sources.
var sourcePatterns = []struct {
	re  *regexp.Regexp
	src quality.Source
}{
	{regexp.MustCompile(`(?i)\bEPUB\b`), quality.SourceEPUB},
	{regexp.MustCompile(`(?i)\bAZW3?\b`), quality.SourceAZW3},
	{regexp.MustCompile(`(?i)\bMOBI\b`), quality.SourceMOBI},
	{regexp.MustCompile(`(?i)\bPDF\b`), quality.SourcePDF},
	{regexp.MustCompile(`(?i)\bM4B\b`), quality.SourceM4B},
	{regexp.MustCompile(`(?i)\bMP3\b`), quality.SourceMP3},
	{regexp.MustCompile(`(?i)\bREMUX\b`), quality.SourceRemux},
	{regexp.MustCompile(`(?i)\b(?:Blu[- .]?Ray|BDRip|BRRip|BD)\b`), quality.SourceBluray},
	{regexp.MustCompile(`(?i)\bWEB[- .]?DL\b`), quality.SourceWEBDL},
	{regexp.MustCompile(`(?i)\bWEB[- .]?Rip\b`), quality.SourceWEBRip},
	{regexp.MustCompile(`(?i)\bWEB\b`), quality.SourceWEBDL},
	{regexp.MustCompile(`(?i)\b(?:HDTV|PDTV|SDTV|DSR)\b`), quality.SourceHDTV},
	{regexp.MustCompile(`(?i)\b(?:DVDRip|DVD[- .]?R|DVD)\b`), quality.SourceDVD},
	{regexp.MustCompile(`(?i)\b(?:HDCAM|CAMRip|CAM)\b`), quality.SourceCAM},
	{regexp.MustCompile(`(?i)\b(?:TELESYNC|HDTS)\b`), quality.SourceTelesync},
}

// Parse reads a release title. It never panics; unparseable input yields a
// Parsed with best-effort fields (Season -1, empty title possible).
func Parse(release string) Parsed {
	p := Parsed{Season: -1}
	if len(release) > 512 {
		release = release[:512]
	}
	s := strings.TrimSpace(release)
	if s == "" {
		return p
	}
	// Underscores are separators in release names, but word characters to
	// the regex engine — normalize them away up front.
	s = strings.ReplaceAll(s, "_", ".")

	// Container extension off the end first.
	s = reContainer.ReplaceAllString(s, "")

	// Anime-style "[Group] Title ..." prefix.
	if m := reBracketPrefix.FindStringSubmatch(s); m != nil {
		p.Group = m[1]
		s = s[len(m[0]):]
	}

	// Structural markers; the earliest one ends the title.
	titleEnd := len(s)
	claim := func(loc []int) {
		if loc != nil && loc[0] < titleEnd {
			titleEnd = loc[0]
		}
	}

	daily := reDaily.FindStringSubmatchIndex(s)
	sxe := reSxxEyy.FindStringSubmatchIndex(s)
	nxx := reNxx.FindStringSubmatchIndex(s)

	switch {
	case sxe != nil:
		claim(sxe[0:2])
		p.Season = atoi(s[sxe[2]:sxe[3]])
		p.Episodes = []int{atoi(s[sxe[4]:sxe[5]])}
		if sxe[6] >= 0 {
			for _, c := range reEpCont.FindAllStringSubmatch(s[sxe[6]:sxe[7]], -1) {
				p.Episodes = append(p.Episodes, atoi(c[1]))
			}
			p.Episodes = expandRuns(p.Episodes)
		}
	case daily != nil:
		claim(daily[0:2])
		p.Daily = s[daily[2]:daily[3]] + "-" + s[daily[4]:daily[5]] + "-" + s[daily[6]:daily[7]]
	case nxx != nil:
		claim(nxx[0:2])
		p.Season = atoi(s[nxx[2]:nxx[3]])
		p.Episodes = []int{atoi(s[nxx[4]:nxx[5]])}
		if nxx[6] >= 0 {
			p.Episodes = expandRuns(append(p.Episodes, atoi(s[nxx[6]:nxx[7]])))
		}
	default:
		// Anime absolute numbering ("[Group] Show - 15 [1080p]"): only when
		// a bracket-group prefix marked this as an anime-style release.
		if p.Group != "" {
			if m := reAbsolute.FindStringSubmatchIndex(s); m != nil {
				first := atoi(s[m[2]:m[3]])
				// A 4-digit "episode" that looks like a year is a year.
				if first < 1900 {
					claim(m[0:2])
					p.Absolute = []int{first}
					if m[4] >= 0 {
						p.Absolute = expandRuns([]int{first, atoi(s[m[4]:m[5]])})
					}
					break
				}
			}
		}
		// Season pack? Only meaningful with a real season marker.
		if m := reSeasonOnly.FindStringSubmatchIndex(s); m != nil {
			num := ""
			if m[2] >= 0 {
				num = s[m[2]:m[3]]
			} else if m[4] >= 0 {
				num = s[m[4]:m[5]]
			}
			if num != "" {
				claim(m[0:2])
				p.Season = atoi(num)
				p.SeasonPack = true
			}
		}
	}

	// Quality tokens also bound the title.
	if m := reResolution.FindStringIndex(s); m != nil {
		claim(m)
		p.Quality.Resolution = resolutionOf(s[m[0]:m[1]])
	}
	p.Quality.Source = quality.SourceUnknown
	for _, sp := range sourcePatterns {
		// Skip matches at position 0: a title starting with "Cam", "Web",
		// or "DVD" is a title, not a source tag.
		var hit []int
		for _, m := range sp.re.FindAllStringIndex(s, -1) {
			if m[0] > 0 {
				hit = m
				break
			}
		}
		if hit != nil {
			claim(hit)
			p.Quality.Source = sp.src
			break
		}
	}
	if p.Quality.Source == quality.SourceRemux && p.Quality.Resolution == 0 {
		p.Quality.Resolution = 1080
	}

	// Movie year: last standalone year token (avoids years inside titles
	// like "2001 A Space Odyssey 1968"), unless it's part of a daily date.
	if p.Daily == "" {
		years := reYear.FindAllStringIndex(s, -1)
		for i := len(years) - 1; i >= 0; i-- {
			y := years[i]
			if y[0] == 0 {
				continue // a title that IS a year ("1917") stays a title
			}
			p.Year = atoi(s[y[0]:y[1]])
			claim(y)
			break
		}
	}

	p.Proper = reProper.MatchString(s)
	p.Repack = reRepack.MatchString(s)
	if m := reCodec.FindStringSubmatch(s); m != nil {
		p.Codec = strings.ToLower(strings.ReplaceAll(m[1], ".", ""))
	}
	if p.Group == "" {
		if m := reGroupSuffix.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
			if !reResolution.MatchString(m[1]) && !reCodec.MatchString(m[1]) {
				p.Group = m[1]
			}
		}
	}

	p.Title = cleanTitle(s[:titleEnd])

	// Book-format releases (ADR 0006): the blob usually carries an author —
	// "Author - Title" or "Title by Author". Books have no season structure;
	// anything the video patterns claimed above is noise.
	if quality.IsBookFormat(p.Quality.Source) {
		p.Season, p.Episodes, p.SeasonPack, p.Daily, p.Absolute = -1, nil, false, "", nil
		p.Author, p.Title = splitBookTitle(p.Title)
	}
	return p
}

// splitBookTitle separates author from title in a book release blob.
// Dash order is assumed "Author - Title" (the dominant convention); the
// matcher tolerates the swapped order.
func splitBookTitle(blob string) (author, title string) {
	blob = strings.ReplaceAll(blob, " – ", " - ") // en-dash variant
	if idx := strings.Index(blob, " - "); idx > 0 {
		return strings.TrimSpace(blob[:idx]), strings.TrimSpace(blob[idx+3:])
	}
	lower := strings.ToLower(blob)
	if idx := strings.LastIndex(lower, " by "); idx > 0 {
		return strings.TrimSpace(blob[idx+4:]), strings.TrimSpace(blob[:idx])
	}
	return "", blob
}

func cleanTitle(raw string) string {
	t := reCleanJunk.ReplaceAllString(raw, " ")
	t = strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ").Replace(t)
	t = reMultiSpace.ReplaceAllString(strings.TrimSpace(t), " ")
	t = strings.Trim(t, "-– ")
	return strings.TrimSpace(t)
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func resolutionOf(tok string) int {
	switch strings.ToLower(tok) {
	case "2160p", "4k", "uhd":
		return 2160
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "480p":
		return 480
	}
	return 0
}

func expandRuns(eps []int) []int {
	if len(eps) == 2 && eps[1] > eps[0]+1 && eps[1]-eps[0] <= 12 {
		out := make([]int, 0, eps[1]-eps[0]+1)
		for e := eps[0]; e <= eps[1]; e++ {
			out = append(out, e)
		}
		return out
	}
	seen := map[int]bool{}
	out := eps[:0]
	for _, e := range eps {
		if e >= 0 && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}
