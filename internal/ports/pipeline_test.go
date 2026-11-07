package ports

import "testing"

// The deployed runner-manifest-v1 digest (Runner delivery.rs golden vector):
// the publications on disk and the v1 adapter depend on it never changing.
func TestManifestDigestGoldenVector(t *testing.T) {
	const fileHash = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	got, err := ManifestDigest([]ManifestFile{
		{Path: "z.mkv", Bytes: 1, SHA256: fileHash},
		{Path: "é<&.mkv", Bytes: 1, SHA256: fileHash},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "a0acee1d6757529c7deca3dc2ac2fa2f0b6e96db781d83b8d85d732cedd6459f"; got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
	if _, err := ManifestDigest(nil); err == nil {
		t.Fatal("an empty manifest was accepted")
	}
}

// An old Runner's history strings map to typed terminals (contract §6).
func TestLegacyTerminal(t *testing.T) {
	for status, want := range map[string]RunnerTerminal{
		"SUCCESS":        {Outcome: OutcomeCompleted},
		"DELETED":        {Outcome: OutcomeCancelled},
		"FAILURE/HEALTH": {Outcome: OutcomeFailed, Reason: ReasonHealth, BlamesRelease: true},
		"FAILURE/FETCH":  {Outcome: OutcomeFailed, Reason: ReasonFetch, BlamesRelease: true},
		"PAR_FAILURE":    {Outcome: OutcomeFailed, Reason: ReasonPar, BlamesRelease: true},
		"UNPACK_FAILURE": {Outcome: OutcomeFailed, Reason: ReasonUnpack, BlamesRelease: true},
		"SCRIPT_FAILURE": {Outcome: OutcomeFailed, Reason: ReasonScript, BlamesRelease: true},
	} {
		got := LegacyTerminal(status, false)
		if got.Outcome != want.Outcome || got.Reason != want.Reason || got.BlamesRelease != want.BlamesRelease || got.Salvage != "none" {
			t.Errorf("%s = %+v, want %+v", status, got, want)
		}
	}
	if LegacyTerminal("FAILURE/HEALTH", true).BlamesRelease {
		t.Error("a blameless legacy failure blamed the release")
	}
}
