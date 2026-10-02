package acquisitionplan

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

func cand(k string, pack bool, class int, bytes int64, eps ...EpisodeKey) Candidate {
	return Candidate{Key: CandidateKey(k), Pack: pack, Pref: Preference{Class: class, QualityRank: class}, Eligible: eps, Payload: eps, SizeBytes: bytes, SizeKnown: true, Protocol: "usenet"}
}
func TestPolicyExamples(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want []CandidateKey
	}{
		{"one hole", Input{[]EpisodeKey{5}, []Candidate{func() Candidate {
			c := cand("pack", true, 2, 346, 5)
			c.Payload = []EpisodeKey{1, 2, 3, 4, 5, 6}
			return c
		}(), cand("single", false, 2, 10, 5)}, true}, []CandidateKey{"single"}},
		{"target saturation", Input{[]EpisodeKey{1, 2}, []Candidate{cand("pack", true, TargetMet, 15, 1, 2), cand("s1", false, TargetMet, 25, 1), cand("s2", false, TargetMet, 25, 2)}, true}, []CandidateKey{"pack"}},
		{"mixed", Input{[]EpisodeKey{1, 2}, []Candidate{cand("pack", true, 1, 15, 1, 2), cand("s1", false, 2, 25, 1)}, true}, []CandidateKey{"pack", "s1"}},
		{"coverage", Input{[]EpisodeKey{1, 2, 3}, []Candidate{cand("pack", true, 2, 100, 1, 2, 3), cand("s1", false, 2, 1, 1), cand("s2", false, 2, 1, 2)}, true}, []CandidateKey{"pack"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, e := Build(tt.in)
			if e != nil {
				t.Fatal(e)
			}
			if !slices.Equal(p.Releases, tt.want) {
				t.Fatalf("got %v want %v", p.Releases, tt.want)
			}
		})
	}
}
func TestHealthAndPoolInvariance(t *testing.T) {
	a, b := cand("a", false, 1, 210, 1), cand("b", false, 1, 211, 1)
	a.Protocol, b.Protocol = "torrent", "torrent"
	a.SeedersKnown, b.SeedersKnown = true, true
	b.Seeders = 150
	p, e := Build(Input{[]EpisodeKey{1}, []Candidate{a, b}, false})
	if e != nil || !slices.Equal(p.Releases, []CandidateKey{"b"}) {
		t.Fatalf("%+v %v", p, e)
	}
	pack := cand("pack", true, 1, 346, 1, 2)
	pack.Eligible = []EpisodeKey{1}
	pack.Protocol = "torrent"
	pack.SeedersKnown = true
	pack.Seeders = 10
	a.SeedersKnown = false
	p, e = Build(Input{[]EpisodeKey{1}, []Candidate{pack, a}, true})
	if e != nil || !slices.Equal(p.Releases, []CandidateKey{"a"}) {
		t.Fatalf("unknown seeds: %+v %v", p, e)
	}
	a.SeedersKnown = true
	p, e = Build(Input{[]EpisodeKey{1}, []Candidate{pack, a}, true})
	if e != nil || !slices.Equal(p.Releases, []CandidateKey{"pack"}) {
		t.Fatalf("zero seeds: %+v %v", p, e)
	}
	x, y := cand("x", false, 1, 10, 1), cand("y", false, 1, 10, 2)
	huge := cand("huge", true, 1, 100, 1, 2)
	before, _ := Build(Input{[]EpisodeKey{1, 2}, []Candidate{x, y, huge}, true})
	unknown := cand("unknown", false, 1, 0, 1)
	unknown.SizeKnown = false
	after, _ := Build(Input{[]EpisodeKey{1, 2}, []Candidate{x, y, huge, unknown}, true})
	if !slices.Equal(before.Releases, after.Releases) {
		t.Fatalf("unused uncertainty changes choice")
	}
}

// Exhaustively enumerate subsets independently of the pack-base construction.
// Compare outcome vector and numeric cost; ties are checked by permutations.
func oracle(in Input) (Cost, bool) {
	ideal := map[EpisodeKey]int{}
	for _, c := range in.Candidates {
		for _, e := range c.Eligible {
			if v, ok := ideal[e]; !ok || c.Pref.Class > v {
				ideal[e] = c.Pref.Class
			}
		}
	}
	var best Cost
	found := false
	for mask := 0; mask < 1<<len(in.Candidates); mask++ {
		outcomes := map[EpisodeKey]int{}
		providers := map[EpisodeKey]int{}
		for i, c := range in.Candidates {
			if mask&(1<<i) == 0 {
				continue
			}
			for _, e := range c.Eligible {
				v, ok := outcomes[e]
				if !ok || c.Pref.Class > v {
					outcomes[e] = c.Pref.Class
					providers[e] = i
				}
			}
		}
		if !reflect.DeepEqual(outcomes, ideal) {
			continue
		}
		used := map[int]int{}
		for _, i := range providers {
			used[i]++
		}
		cost := Cost{}
		valid := true
		for i, c := range in.Candidates {
			if mask&(1<<i) == 0 {
				continue
			}
			if used[i] == 0 {
				valid = false
				break
			}
			r, u := torrentRisk(c)
			cost.ZeroSeedTorrents += r
			cost.UnknownSeedTorrents += u
			if c.SizeKnown {
				cost.KnownBytes += c.SizeBytes
			} else {
				cost.UnknownSizes++
			}
			cost.RedundantEpisodes += len(c.Payload) - used[i]
			cost.Transfers++
		}
		if valid && (!found || compareCost(cost, best) < 0) {
			best = cost
			found = true
		}
	}
	return best, found
}
func TestSmallPoolsAgainstSubsetOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 500; trial++ {
		in := Input{Episodes: []EpisodeKey{1, 2, 3}, AllowPacks: true}
		for p := 0; p < 2; p++ {
			c := cand(string(rune('a'+p)), true, p+1, int64(rng.Intn(50)+1), 1, 2, 3)
			if p == 0 && rng.Intn(2) == 0 {
				c.Eligible = []EpisodeKey{1}
			}
			in.Candidates = append(in.Candidates, c)
		}
		for e := EpisodeKey(1); e <= 3; e++ {
			for j := 0; j < 2; j++ {
				c := cand(string(rune('c'+int(e)*2+j)), false, rng.Intn(3)+1, int64(rng.Intn(30)+1), e)
				in.Candidates = append(in.Candidates, c)
			}
		}
		for i := range in.Candidates {
			c := &in.Candidates[i]
			if rng.Intn(2) == 0 {
				c.Protocol = "torrent"
				c.SeedersKnown = rng.Intn(2) == 0
				c.Seeders = rng.Intn(3)
			}
			c.SizeKnown = rng.Intn(4) != 0
			if c.Pref.Class == 3 {
				c.Pref.Class = TargetMet
			}
		}
		original := slices.Clone(in.Candidates)
		p, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := oracle(in)
		if !ok || p.Cost != want {
			t.Fatalf("trial %d got %+v want %+v", trial, p.Cost, want)
		}
		rng.Shuffle(len(in.Candidates), func(i, j int) { in.Candidates[i], in.Candidates[j] = in.Candidates[j], in.Candidates[i] })
		shuffled, err := Build(in)
		if err != nil || !slices.Equal(p.Releases, shuffled.Releases) {
			t.Fatalf("permutation changes winner")
		}
		in.Candidates = append(original, original[0])
		duplicate, err := Build(in)
		if err != nil || !slices.Equal(p.Releases, duplicate.Releases) {
			t.Fatalf("duplicate changes winner")
		}
	}
}
func TestUnsupportedCoverage(t *testing.T) {
	for _, cs := range [][]Candidate{{cand("bundle", false, 1, 1, 1, 2)}, {cand("a", true, 1, 1, 1, 2), cand("b", true, 2, 1, 2, 3)}} {
		if _, e := Build(Input{[]EpisodeKey{1, 2, 3}, cs, true}); e == nil {
			t.Fatal("accepted unsupported coverage")
		}
	}
}
