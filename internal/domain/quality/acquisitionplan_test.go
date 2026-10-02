package quality

import "testing"

// The restricted pack optimizer relies on nested authorization and target
// interchangeability. Exercise the real policy over all supported video ranks,
// languages, floors, verification and baseline qualities.
func TestAcquisitionPlanMonotonicity(t *testing.T) {
	qs := Vocabulary(Quality{SourceWEBDL, 1080})
	for _, target := range qs {
		for _, floor := range append([]Quality{{}}, qs...) {
			for _, required := range [][]string{nil, {"en"}} {
				p := Profile{Target: target, Languages: required, UpgradesAllowed: true}
				if floor.Source != "" {
					p.Floor = &floor
				}
				for _, base := range qs {
					for _, verified := range []bool{false, true} {
						for _, audio := range []Audio{{}, {Languages: []string{"de"}, Known: true}, {Languages: []string{"en"}, Known: true}} {
							for _, offered := range [][]string{{"en"}, {"de"}, {"mul"}} {
								for _, a := range qs {
									for _, b := range qs {
										if !p.Acceptable(a) || !p.Acceptable(b) || !p.LanguageAcceptable(offered) {
											continue
										}
										aa := p.Upgrade(a, offered, base, verified, audio)
										bb := p.Upgrade(b, offered, base, verified, audio)
										if Rank(a) >= Rank(b) && bb && !aa {
											t.Fatalf("nonmonotone: %+v base %v a %v b %v", p, base, a, b)
										}
										if p.Met(a, true) && p.Met(b, true) && aa != bb {
											t.Fatalf("target candidates not interchangeable: %+v base %v a %v b %v", p, base, a, b)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}
