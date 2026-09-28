// Package language is the one vocabulary for audio languages (ADR 0022).
//
// Three sources describe the language of a soundtrack, and none of them
// agree on spelling: a Matroska track says "ger" or "de-DE", an MP4 media
// header says "deu", and a release name says "GERMAN". A profile that
// requires English has to be able to compare all three, so every one of them
// is reduced to the same canonical code before anything looks at it.
//
// Canonical codes are ISO 639-1 where one exists ("en", "de", "ja"), which is
// what most people recognise and what BCP 47 tags start with. Two special
// codes carry meaning of their own: Multi ("mul", the real ISO 639-2 code for
// "multiple languages") marks a release that advertises several soundtracks
// without naming them, and the empty string means "not declared".
package language

import (
	"sort"
	"strings"
)

// Multi is the canonical code for "several languages, unnamed": what a MULTi
// or DUAL-AUDIO release tag promises. It satisfies any requirement, because a
// MULTi release of an English-language film carries the English track along
// with whatever else was added — that is what the tag is for.
const Multi = "mul"

// English is the code most requirements and defaults reduce to.
const English = "en"

// entry is one known language: its canonical code, its display name, and
// every spelling a container or a release name might use for it.
type entry struct {
	code    string
	name    string
	aliases []string
}

// table lists the languages monarr understands. Order matters only for
// display (All returns it as written). The aliases carry ISO 639-2/B,
// ISO 639-2/T, the English name, the language's own name where release
// groups use it, and the scene abbreviations that turn up in release names.
var table = []entry{
	{"en", "English", []string{"eng", "english", "englisch"}},
	{"de", "German", []string{"ger", "deu", "german", "deutsch"}},
	{"fr", "French", []string{"fre", "fra", "french", "francais", "français", "truefrench", "vff", "vfq", "vf", "vfi", "vf2", "vof"}},
	{"es", "Spanish", []string{"spa", "spanish", "espanol", "español", "castellano", "latino", "esp"}},
	{"it", "Italian", []string{"ita", "italian", "italiano"}},
	{"pt", "Portuguese", []string{"por", "portuguese", "portugues", "português", "brazilian", "pt-br", "ptbr"}},
	{"nl", "Dutch", []string{"dut", "nld", "dutch", "nederlands", "flemish", "vlaams"}},
	{"ja", "Japanese", []string{"jpn", "japanese", "jap"}},
	{"ko", "Korean", []string{"kor", "korean"}},
	{"zh", "Chinese", []string{"chi", "zho", "chinese", "mandarin", "cantonese", "chs", "cht"}},
	{"ru", "Russian", []string{"rus", "russian"}},
	{"uk", "Ukrainian", []string{"ukr", "ukrainian"}},
	{"pl", "Polish", []string{"pol", "polish", "polski", "lektor"}},
	{"cs", "Czech", []string{"cze", "ces", "czech"}},
	{"sk", "Slovak", []string{"slo", "slk", "slovak"}},
	{"hu", "Hungarian", []string{"hun", "hungarian", "magyar"}},
	{"ro", "Romanian", []string{"rum", "ron", "romanian"}},
	{"bg", "Bulgarian", []string{"bul", "bulgarian"}},
	{"el", "Greek", []string{"gre", "ell", "greek"}},
	{"tr", "Turkish", []string{"tur", "turkish"}},
	{"sv", "Swedish", []string{"swe", "swedish", "svenska"}},
	{"no", "Norwegian", []string{"nor", "nob", "nno", "norwegian", "norsk"}},
	{"da", "Danish", []string{"dan", "danish", "dansk"}},
	{"fi", "Finnish", []string{"fin", "finnish", "suomi"}},
	{"is", "Icelandic", []string{"ice", "isl", "icelandic"}},
	{"he", "Hebrew", []string{"heb", "hebrew"}},
	{"ar", "Arabic", []string{"ara", "arabic"}},
	{"fa", "Persian", []string{"per", "fas", "persian", "farsi"}},
	{"hi", "Hindi", []string{"hin", "hindi"}},
	{"ta", "Tamil", []string{"tam", "tamil"}},
	{"te", "Telugu", []string{"tel", "telugu"}},
	{"bn", "Bengali", []string{"ben", "bengali"}},
	{"th", "Thai", []string{"tha", "thai"}},
	{"vi", "Vietnamese", []string{"vie", "vietnamese"}},
	{"id", "Indonesian", []string{"ind", "indonesian"}},
	{"ms", "Malay", []string{"may", "msa", "malay"}},
	{"tl", "Tagalog", []string{"tgl", "fil", "tagalog", "filipino"}},
	{"ca", "Catalan", []string{"cat", "catalan"}},
	{"eu", "Basque", []string{"baq", "eus", "basque"}},
	{"hr", "Croatian", []string{"hrv", "croatian"}},
	{"sr", "Serbian", []string{"srp", "serbian"}},
	{"sl", "Slovenian", []string{"slv", "slovenian"}},
	{"lt", "Lithuanian", []string{"lit", "lithuanian"}},
	{"lv", "Latvian", []string{"lav", "latvian"}},
	{"et", "Estonian", []string{"est", "estonian"}},
	{Multi, "Multiple languages", []string{"multi", "multiple", "dual", "dual-audio", "dualaudio", "dl"}},
}

var (
	byAlias = map[string]string{}
	byCode  = map[string]entry{}
)

func init() {
	for _, e := range table {
		byCode[e.code] = e
		byAlias[e.code] = e.code
		byAlias[e.name] = e.code
		byAlias[strings.ToLower(e.name)] = e.code
		for _, a := range e.aliases {
			byAlias[a] = e.code
		}
	}
}

// Canonical reduces any spelling of a language to its code. Unknown input
// comes back lowercased and trimmed rather than dropped: a track tagged with
// a language monarr has no table entry for is still a declared language, and
// it must not be mistaken for "undeclared". "und" (ISO for undetermined) and
// blank ARE undeclared and reduce to "".
func Canonical(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "und" || s == "zxx" {
		return ""
	}
	if c, ok := byAlias[s]; ok {
		return c
	}
	// BCP 47 subtags: "en-US", "pt_BR". Try the whole thing first (pt-br is
	// an alias), then the primary subtag.
	if i := strings.IndexAny(s, "-_"); i > 0 {
		if c, ok := byAlias[s[:i]]; ok {
			return c
		}
		return s[:i]
	}
	return s
}

// Display renders a code the way people write it ("de" → "German"). A code
// outside the table is shown as itself, uppercased, so nothing is hidden.
func Display(code string) string {
	if e, ok := byCode[code]; ok {
		return e.name
	}
	if code == "" {
		return "undeclared"
	}
	return strings.ToUpper(code)
}

// DisplayList renders several codes for a sentence: "English", "English or
// French", "English, French or German".
func DisplayList(codes []string) string {
	names := make([]string, 0, len(codes))
	for _, c := range codes {
		names = append(names, Display(c))
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// Normalize canonicalizes, dedupes and sorts a list, dropping undeclared
// entries. It is what every stored or compared list goes through.
func Normalize(codes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range codes {
		c = Canonical(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Satisfies reports whether a set of offered languages meets a requirement:
// no requirement is always met; Multi on offer meets any requirement; and
// otherwise at least one required language must be offered. Both sides are
// canonicalized here so callers can pass raw values.
func Satisfies(offered, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := map[string]bool{}
	for _, o := range offered {
		have[Canonical(o)] = true
	}
	if have[Multi] {
		return true
	}
	for _, r := range required {
		if have[Canonical(r)] {
			return true
		}
	}
	return false
}

// releaseTokens is the vocabulary a RELEASE NAME may use, kept deliberately
// smaller than the alias table. A container tag is data and every ISO code is
// safe there; a release name is prose with tags in it, and three-letter codes
// like "per", "may", "ice", "fin" or "est" are ordinary words that turn up in
// episode titles. Only the spellings release groups actually write as
// language tags are recognised here.
var releaseTokens = map[string]string{
	"english": "en", "eng": "en",
	"german": "de", "deutsch": "de", "ger": "de",
	"french": "fr", "francais": "fr", "truefrench": "fr", "vff": "fr", "vfq": "fr",
	"vfi": "fr", "vf2": "fr", "vof": "fr", "vf": "fr", "fre": "fr", "fra": "fr",
	"spanish": "es", "castellano": "es", "latino": "es", "spa": "es", "esp": "es",
	"italian": "it", "italiano": "it", "ita": "it",
	"portuguese": "pt", "brazilian": "pt", "ptbr": "pt", "pt-br": "pt", "por": "pt",
	"dutch": "nl", "nl": "nl", "flemish": "nl",
	"japanese": "ja", "jpn": "ja", "jap": "ja",
	"korean": "ko", "kor": "ko",
	"chinese": "zh", "mandarin": "zh", "cantonese": "zh", "chs": "zh", "cht": "zh",
	"russian": "ru", "rus": "ru",
	"ukrainian": "uk", "ukr": "uk",
	"polish": "pl", "lektor": "pl", "pldub": "pl", "pl": "pl",
	"czech": "cs", "cze": "cs", "cz": "cs",
	"slovak":    "sk",
	"hungarian": "hu",
	"romanian":  "ro",
	"bulgarian": "bg",
	"greek":     "el",
	"turkish":   "tr",
	"swedish":   "sv", "swe": "sv",
	"norwegian": "no",
	"danish":    "da",
	"finnish":   "fi",
	"icelandic": "is",
	"hebrew":    "he",
	"arabic":    "ar",
	"persian":   "fa", "farsi": "fa",
	"hindi":      "hi",
	"tamil":      "ta",
	"telugu":     "te",
	"bengali":    "bn",
	"thai":       "th",
	"vietnamese": "vi",
	"indonesian": "id",
	"tagalog":    "tl", "filipino": "tl",
	"catalan":   "ca",
	"croatian":  "hr",
	"serbian":   "sr",
	"slovenian": "sl",
	"multi":     Multi, "multiple": Multi, "dual": Multi, "dual-audio": Multi,
	"dualaudio": Multi, "dl": Multi,
}

// ReleaseToken reads one token of a release name as a language tag. ok is
// false for anything that is not one — most tokens.
func ReleaseToken(tok string) (code string, ok bool) {
	code, ok = releaseTokens[strings.ToLower(strings.TrimSpace(tok))]
	return code, ok
}

// Option is one selectable language for an editor.
type Option struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Options lists the languages a profile can require, in table order, without
// Multi — a requirement of "several, unnamed" means nothing.
func Options() []Option {
	out := make([]Option, 0, len(table))
	for _, e := range table {
		if e.code == Multi {
			continue
		}
		out = append(out, Option{Code: e.code, Name: e.name})
	}
	return out
}

// Valid reports whether a code is one a profile may require.
func Valid(code string) bool {
	_, ok := byCode[code]
	return ok && code != Multi
}
