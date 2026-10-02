// Package acquisitionplan selects a uniform full-season pack base and useful
// singles. Inputs have already passed identity, language and replacement gates.
package acquisitionplan

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

const TargetMet = math.MaxInt

type EpisodeKey int64
type CandidateKey string
type Preference struct{ Class, QualityRank, FormatScore int }
type Candidate struct {
	Key               CandidateKey
	Pack              bool
	Pref              Preference
	Eligible, Payload []EpisodeKey
	SizeBytes         int64
	SizeKnown         bool
	Protocol          string
	Seeders           int
	SeedersKnown      bool
}
type Input struct {
	Episodes   []EpisodeKey
	Candidates []Candidate
	AllowPacks bool
}

// Cost is a fixed lexicographic tuple. Unknown sizes are never priced as zero.
type Cost struct {
	ZeroSeedTorrents    int
	UnknownSizes        int
	KnownBytes          int64
	UnknownSeedTorrents int
	RedundantEpisodes   int
	Transfers           int
}
type Plan struct {
	Releases     []CandidateKey
	Providers    map[EpisodeKey]CandidateKey
	Unserved     []EpisodeKey
	Outcomes     map[EpisodeKey]int
	Cost         Cost
	Alternatives []Alternative
}
type Alternative struct {
	Releases []CandidateKey
	Cost     Cost
}

// Build is deterministic and never mutates input. Two packs are unnecessary
// under the asserted nested eligibility and common full-season payload contract.
func Build(in Input) (Plan, error) {
	episodes := slices.Clone(in.Episodes)
	slices.Sort(episodes)
	targets := map[EpisodeKey]bool{}
	for _, e := range episodes {
		if targets[e] {
			return Plan{}, fmt.Errorf("duplicate target %d", e)
		}
		targets[e] = true
	}
	candidates := slices.Clone(in.Candidates)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Key < candidates[j].Key })
	unique := candidates[:0]
	var payload []EpisodeKey
	for _, c := range candidates {
		if c.Key == "" || c.SizeBytes < 0 || c.Seeders < 0 || len(c.Eligible) == 0 {
			return Plan{}, fmt.Errorf("invalid candidate %q", c.Key)
		}
		c.Eligible = slices.Clone(c.Eligible)
		c.Payload = slices.Clone(c.Payload)
		slices.Sort(c.Eligible)
		slices.Sort(c.Payload)
		if len(slices.Compact(slices.Clone(c.Eligible))) != len(c.Eligible) || len(slices.Compact(slices.Clone(c.Payload))) != len(c.Payload) {
			return Plan{}, fmt.Errorf("duplicate coverage %q", c.Key)
		}
		for _, e := range c.Eligible {
			if !targets[e] || !slices.Contains(c.Payload, e) {
				return Plan{}, fmt.Errorf("invalid eligible episode %d for %q", e, c.Key)
			}
		}
		if !c.Pack && (len(c.Payload) != 1 || len(c.Eligible) != 1) {
			return Plan{}, fmt.Errorf("unsupported single %q", c.Key)
		}
		if c.Pack {
			if len(c.Payload) < 2 {
				return Plan{}, fmt.Errorf("unsupported pack %q", c.Key)
			}
			if payload == nil {
				payload = c.Payload
			} else if !slices.Equal(payload, c.Payload) {
				return Plan{}, fmt.Errorf("packs have different payloads")
			}
		}
		if len(unique) > 0 && unique[len(unique)-1].Key == c.Key {
			prev := unique[len(unique)-1]
			if !sameCandidate(prev, c) {
				return Plan{}, fmt.Errorf("conflicting duplicate %q", c.Key)
			}
			continue
		}
		unique = append(unique, c)
	}
	candidates = unique
	var packs []Candidate
	singles := map[EpisodeKey]Candidate{}
	ideal := map[EpisodeKey]int{}
	for _, c := range candidates {
		if c.Pack && !in.AllowPacks {
			continue
		}
		for _, e := range c.Eligible {
			if v, ok := ideal[e]; !ok || c.Pref.Class > v {
				ideal[e] = c.Pref.Class
			}
		}
		if c.Pack {
			packs = append(packs, c)
			continue
		}
		e := c.Eligible[0]
		old, ok := singles[e]
		if !ok || c.Pref.Class > old.Pref.Class || (c.Pref.Class == old.Pref.Class && compareSingle(c, old) < 0) {
			singles[e] = c
		}
	}
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].Pref.Class != packs[j].Pref.Class {
			return packs[i].Pref.Class < packs[j].Pref.Class
		}
		return packs[i].Key < packs[j].Key
	})
	for i := 1; i < len(packs); i++ {
		a, b := packs[i], packs[i-1]
		if a.Pref.Class == b.Pref.Class && !slices.Equal(a.Eligible, b.Eligible) {
			return Plan{}, fmt.Errorf("noninterchangeable equal-class packs %q / %q", a.Key, b.Key)
		}
		for _, e := range b.Eligible {
			if _, ok := slices.BinarySearch(a.Eligible, e); !ok {
				return Plan{}, fmt.Errorf("non-nested pack eligibility %q / %q", a.Key, b.Key)
			}
		}
	}
	bases := append([]Candidate{{}}, packs...)
	var best *Plan
	var alternatives []Alternative
	for _, base := range bases {
		chosen := map[CandidateKey]Candidate{}
		providers := map[EpisodeKey]CandidateKey{}
		outcomes := map[EpisodeKey]int{}
		if base.Key != "" {
			chosen[base.Key] = base
			for _, e := range base.Eligible {
				providers[e] = base.Key
				outcomes[e] = base.Pref.Class
			}
		}
		feasible := true
		for _, e := range episodes {
			single, ok := singles[e]
			current, has := outcomes[e]
			if ok && (!has || single.Pref.Class > current) {
				chosen[single.Key] = single
				providers[e] = single.Key
				outcomes[e] = single.Pref.Class
			}
			if v, ok := ideal[e]; ok {
				if o, has := outcomes[e]; !has || o != v {
					feasible = false
					break
				}
			}
		}
		if !feasible {
			continue
		}
		used := map[CandidateKey]int{}
		for _, k := range providers {
			used[k]++
		}
		var cs []Candidate
		for k, c := range chosen {
			if used[k] > 0 {
				cs = append(cs, c)
			}
		}
		sort.Slice(cs, func(i, j int) bool { return slotLess(cs[i], cs[j]) })
		p := Plan{Providers: providers, Outcomes: outcomes}
		for _, e := range episodes {
			if _, ok := ideal[e]; !ok {
				p.Unserved = append(p.Unserved, e)
			}
		}
		for _, c := range cs {
			p.Releases = append(p.Releases, c.Key)
			risk, unknown := torrentRisk(c)
			p.Cost.ZeroSeedTorrents += risk
			p.Cost.UnknownSeedTorrents += unknown
			if !c.SizeKnown {
				p.Cost.UnknownSizes++
			} else {
				if c.SizeBytes > math.MaxInt64-p.Cost.KnownBytes {
					return Plan{}, fmt.Errorf("size overflow")
				}
				p.Cost.KnownBytes += c.SizeBytes
			}
			p.Cost.RedundantEpisodes += len(c.Payload) - used[c.Key]
			p.Cost.Transfers++
		}
		alternatives = append(alternatives, Alternative{Releases: slices.Clone(p.Releases), Cost: p.Cost})
		if best == nil || compareCost(p.Cost, best.Cost) < 0 || (compareCost(p.Cost, best.Cost) == 0 && compareSignature(cs, lookup(best.Releases, candidates)) < 0) {
			best = &p
		}
	}
	if best == nil {
		return Plan{}, fmt.Errorf("candidate pool cannot attain its vector")
	}
	best.Alternatives = alternatives
	return *best, nil
}
func sameCandidate(a, b Candidate) bool {
	return a.Key == b.Key && a.Pack == b.Pack && a.Pref == b.Pref && slices.Equal(a.Eligible, b.Eligible) && slices.Equal(a.Payload, b.Payload) && a.SizeBytes == b.SizeBytes && a.SizeKnown == b.SizeKnown && a.Protocol == b.Protocol && a.Seeders == b.Seeders && a.SeedersKnown == b.SeedersKnown
}
func torrentRisk(c Candidate) (int, int) {
	if c.Protocol != "torrent" {
		return 0, 0
	}
	if !c.SeedersKnown {
		return 0, 1
	}
	if c.Seeders == 0 {
		return 1, 0
	}
	return 0, 0
}
func compareCost(a, b Cost) int {
	for _, p := range [][2]int64{{int64(a.ZeroSeedTorrents), int64(b.ZeroSeedTorrents)}, {int64(a.UnknownSizes), int64(b.UnknownSizes)}, {a.KnownBytes, b.KnownBytes}, {int64(a.UnknownSeedTorrents), int64(b.UnknownSeedTorrents)}, {int64(a.RedundantEpisodes), int64(b.RedundantEpisodes)}, {int64(a.Transfers), int64(b.Transfers)}} {
		if p[0] < p[1] {
			return -1
		}
		if p[0] > p[1] {
			return 1
		}
	}
	return 0
}
func slotLess(a, b Candidate) bool {
	if a.Pack != b.Pack {
		return a.Pack
	}
	if !a.Pack && a.Eligible[0] != b.Eligible[0] {
		return a.Eligible[0] < b.Eligible[0]
	}
	return a.Key < b.Key
}
func tie(a, b Candidate) int {
	if a.Pref.QualityRank != b.Pref.QualityRank {
		if a.Pref.QualityRank > b.Pref.QualityRank {
			return -1
		}
		return 1
	}
	if a.Pref.FormatScore != b.Pref.FormatScore {
		if a.Pref.FormatScore > b.Pref.FormatScore {
			return -1
		}
		return 1
	}
	if a.Protocol != b.Protocol {
		if a.Protocol < b.Protocol {
			return -1
		}
		return 1
	}
	as, bs := 0, 0
	if a.Protocol == "torrent" && a.SeedersKnown {
		as = a.Seeders
	}
	if b.Protocol == "torrent" && b.SeedersKnown {
		bs = b.Seeders
	}
	if as != bs {
		if as > bs {
			return -1
		}
		return 1
	}
	if a.Key < b.Key {
		return -1
	}
	if a.Key > b.Key {
		return 1
	}
	return 0
}
func compareSingle(a, b Candidate) int {
	ar, au := torrentRisk(a)
	br, bu := torrentRisk(b)
	ac := Cost{ZeroSeedTorrents: ar, UnknownSeedTorrents: au, KnownBytes: a.SizeBytes}
	bc := Cost{ZeroSeedTorrents: br, UnknownSeedTorrents: bu, KnownBytes: b.SizeBytes}
	if !a.SizeKnown {
		ac.UnknownSizes = 1
		ac.KnownBytes = 0
	}
	if !b.SizeKnown {
		bc.UnknownSizes = 1
		bc.KnownBytes = 0
	}
	if v := compareCost(ac, bc); v != 0 {
		return v
	}
	return tie(a, b)
}
func compareSignature(a, b []Candidate) int {
	for i := range a {
		if v := tie(a[i], b[i]); v != 0 {
			return v
		}
	}
	return 0
}
func lookup(keys []CandidateKey, cs []Candidate) []Candidate {
	out := make([]Candidate, 0, len(keys))
	for _, k := range keys {
		for _, c := range cs {
			if c.Key == k {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
