package language

import (
	"reflect"
	"testing"
)

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"eng": "en", "en": "en", "EN-us": "en", "English": "en",
		"ger": "de", "deu": "de", "de-DE": "de", "GERMAN": "de",
		"fre": "fr", "fra": "fr", "pt-BR": "pt", "pt_BR": "pt",
		"und": "", "": "", "  ": "", "zxx": "",
		// An unknown but declared language stays declared, lowercased.
		"xyz": "xyz", "Klingon": "klingon", "tlh-Latn": "tlh",
		"mul": Multi, "MULTI": Multi,
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplay(t *testing.T) {
	if got := Display("de"); got != "German" {
		t.Fatalf("Display(de) = %q", got)
	}
	if got := Display("xyz"); got != "XYZ" {
		t.Fatalf("Display(xyz) = %q", got)
	}
	if got := Display(""); got != "undeclared" {
		t.Fatalf("Display('') = %q", got)
	}
	cases := map[string][]string{
		"":                          nil,
		"English":                   {"en"},
		"English or French":         {"en", "fr"},
		"English, French or German": {"en", "fr", "de"},
	}
	for want, in := range cases {
		if got := DisplayList(in); got != want {
			t.Errorf("DisplayList(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	got := Normalize([]string{"GER", "eng", "und", "", "en-US", "deu"})
	want := []string{"de", "en"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize = %v, want %v", got, want)
	}
	if Normalize(nil) != nil {
		t.Fatal("Normalize(nil) should be nil")
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		offered, required []string
		want              bool
	}{
		{nil, nil, true},
		{[]string{"de"}, nil, true},
		{nil, []string{"en"}, false},
		{[]string{"de"}, []string{"en"}, false},
		{[]string{"ger", "eng"}, []string{"en"}, true},
		{[]string{"de"}, []string{"en", "de"}, true},
		{[]string{Multi}, []string{"en"}, true},
		{[]string{"und"}, []string{"en"}, false},
	}
	for _, c := range cases {
		if got := Satisfies(c.offered, c.required); got != c.want {
			t.Errorf("Satisfies(%v, %v) = %v, want %v", c.offered, c.required, got, c.want)
		}
	}
}

func TestReleaseToken(t *testing.T) {
	yes := map[string]string{
		"GERMAN": "de", "German": "de", "ITA": "it", "TRUEFRENCH": "fr", "VFF": "fr",
		"MULTi": Multi, "DL": Multi, "DUAL-AUDIO": Multi, "LATINO": "es", "NL": "nl",
	}
	for tok, want := range yes {
		got, ok := ReleaseToken(tok)
		if !ok || got != want {
			t.Errorf("ReleaseToken(%q) = %q,%v want %q", tok, got, ok, want)
		}
	}
	// Ordinary words that happen to be ISO codes are not release tags.
	for _, tok := range []string{"per", "may", "ice", "fin", "est", "ind", "cat", "lat", "Mistake", "1080p", "WEB-DL", "VOSTFR", "SUBBED"} {
		if _, ok := ReleaseToken(tok); ok {
			t.Errorf("ReleaseToken(%q) should not be a language", tok)
		}
	}
}

func TestOptionsAndValid(t *testing.T) {
	opts := Options()
	if len(opts) == 0 || opts[0].Code != "en" {
		t.Fatalf("Options() = %v", opts)
	}
	for _, o := range opts {
		if o.Code == Multi {
			t.Fatal("Multi must not be a requirable option")
		}
		if !Valid(o.Code) {
			t.Errorf("Valid(%q) = false", o.Code)
		}
	}
	if Valid(Multi) || Valid("") || Valid("xx") {
		t.Fatal("Valid accepted a code a profile must not require")
	}
}
