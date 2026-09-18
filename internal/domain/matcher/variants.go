package matcher

import "strings"

// TitleVariant is one possible qualifier interpretation. It proposes
// evidence; callers must attempt the full literal title before using it.
type TitleVariant struct {
	Original string
	Base     string
	Country  string
	Rule     string
	Explicit bool
	Conflict bool
}

var countryNames = map[string]string{
	"US": "US", "USA": "US", "U.S.": "US", "U. S.": "US",
	"UNITED STATES": "US", "UNITED STATES OF AMERICA": "US",
	"UK": "GB", "GB": "GB", "UNITED KINGDOM": "GB", "GREAT BRITAIN": "GB",
	"AU": "AU", "AUSTRALIA": "AU", "CA": "CA", "CANADA": "CA",
	"NZ": "NZ", "NEW ZEALAND": "NZ",
}

// TitleVariants recognizes the deliberately small country vocabulary from
// the release-identity contract. Bracketed qualifiers may occur inside a
// title; ambiguous unbracketed qualifiers are considered only at an edge.
func TitleVariants(raw string) []TitleVariant {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []TitleVariant{{Original: raw, Base: raw, Rule: "literal"}}

	type span struct {
		start int
		end   int
		code  string
	}
	var spans []span
	for i := 0; i < len(raw); i++ {
		if raw[i] != '(' && raw[i] != '[' {
			continue
		}
		close := byte(')')
		if raw[i] == '[' {
			close = ']'
		}
		j := strings.IndexByte(raw[i+1:], close)
		if j < 0 {
			continue
		}
		j += i + 1
		if code := countryCode(raw[i+1 : j]); code != "" {
			spans = append(spans, span{start: i, end: j + 1, code: code})
		}
		i = j
	}
	if len(spans) > 0 {
		codes := map[string]bool{}
		var b strings.Builder
		last := 0
		for _, s := range spans {
			codes[s.code] = true
			b.WriteString(raw[last:s.start])
			last = s.end
		}
		b.WriteString(raw[last:])
		v := TitleVariant{
			Original: raw,
			Base:     strings.Join(strings.Fields(b.String()), " "),
			Rule:     "bracketed_country",
			Explicit: true,
		}
		if len(codes) == 1 {
			for code := range codes {
				v.Country = code
			}
		} else {
			v.Conflict = true
		}
		out = append(out, v)
	}

	// Release separators are title punctuation, not word boundaries to the
	// parser. Treat them as spaces only for edge-qualifier recognition so
	// "Show.US" and "Show U.S." produce the same evidence.
	edge := strings.NewReplacer(".", " ", "_", " ").Replace(raw)
	fields := strings.Fields(edge)
	for _, width := range []int{2, 1} {
		if len(fields) <= width {
			continue
		}
		if code := countryCode(strings.Join(fields[len(fields)-width:], " ")); code != "" {
			out = append(out, TitleVariant{
				Original: raw, Base: strings.Join(fields[:len(fields)-width], " "),
				Country: code, Rule: "trailing_country",
			})
			break
		}
	}
	for _, width := range []int{2, 1} {
		if len(fields) <= width {
			continue
		}
		if code := countryCode(strings.Join(fields[:width], " ")); code != "" {
			out = append(out, TitleVariant{
				Original: raw, Base: strings.Join(fields[width:], " "),
				Country: code, Rule: "leading_country",
			})
			break
		}
	}
	return dedupeVariants(out)
}

func countryCode(s string) string {
	s = strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
	if code := countryNames[s]; code != "" {
		return code
	}
	if strings.NewReplacer(".", "", " ", "").Replace(s) == "US" {
		return "US"
	}
	return ""
}

func dedupeVariants(in []TitleVariant) []TitleVariant {
	seen := map[string]bool{}
	out := in[:0]
	for _, variant := range in {
		key := variant.Base + "\x00" + variant.Country + "\x00" + variant.Rule
		if strings.TrimSpace(variant.Base) == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, variant)
	}
	return out
}
