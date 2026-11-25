package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/ports"
)

// GET /hdr-tiers is the vocabulary a profile names, best first.
func TestHDRTiersListTheVocabulary(t *testing.T) {
	e := newAPIEnv(t)
	var tiers []apigen.HDRTierOption
	e.get(t, "/api/v1/hdr-tiers").expect(t, http.StatusOK).into(t, &tiers)
	if len(tiers) != len(hdr.Tiers) || tiers[0].Tier != apigen.HDRTier(hdr.TierDVP7FEL) || tiers[0].Label != "DV P7 FEL" {
		t.Fatalf("tiers = %+v", tiers)
	}
}

// A profile's HDR order, refusals, ties and pre-flight policy round-trip, and
// every malformed one is refused with a reason (ADR 0031 §4-6).
func TestProfileHDRPolicyRoundTripsAndIsValidated(t *testing.T) {
	e := newAPIEnv(t)
	body := `{"name":"UHD","target":{"source":"remux","resolution":2160},
	  "hdrOrder":["dv-p7-fel","dv-p7-mel","dv-p8","hdr10"],"hdrReject":["dv-p5"],
	  "hdrTies":[["dv-p7-fel","dv-p7-mel"]],
	  "preflight":{"top":3,"bytes":1048576,"skipUnreadable":true}}`
	e.put(t, "/api/v1/profiles/1", body).expect(t, http.StatusOK)
	var all []apigen.QualityProfile
	e.get(t, "/api/v1/profiles").expect(t, http.StatusOK).into(t, &all)
	var p apigen.QualityProfile
	for _, x := range all {
		if x.Id == 1 {
			p = x
		}
	}
	if p.HdrOrder == nil || len(*p.HdrOrder) != 4 || p.HdrReject == nil || (*p.HdrReject)[0] != "dv-p5" {
		t.Fatalf("order/reject = %v / %v", p.HdrOrder, p.HdrReject)
	}
	if p.HdrTies == nil || len(*p.HdrTies) != 1 || len((*p.HdrTies)[0]) != 2 {
		t.Fatalf("ties = %v", p.HdrTies)
	}
	if p.Preflight == nil || *p.Preflight.Top != 3 || *p.Preflight.Bytes != 1<<20 || !*p.Preflight.SkipUnreadable {
		t.Fatalf("preflight = %+v", p.Preflight)
	}
	if !strings.Contains(p.Sentence, "prefers DV P7 FEL = DV P7 MEL › DV P8 › HDR10; never DV P5") {
		t.Errorf("sentence = %q", p.Sentence)
	}

	for name, bad := range map[string]string{
		"unknown tier":        `"hdrOrder":["dv-p9"]`,
		"tier twice":          `"hdrOrder":["hdr10","hdr10"]`,
		"tie of one":          `"hdrOrder":["hdr10"],"hdrTies":[["hdr10"]]`,
		"tie outside order":   `"hdrOrder":["hdr10"],"hdrTies":[["hdr10","dv-p8"]]`,
		"tier in two ties":    `"hdrOrder":["dv-p8","hdr10","hlg"],"hdrTies":[["dv-p8","hdr10"],["hdr10","hlg"]]`,
		"too many probes":     `"hdrOrder":["hdr10"],"preflight":{"top":11}`,
		"head too large":      `"hdrOrder":["hdr10"],"preflight":{"bytes":33554432}`,
		"unknown reject tier": `"hdrReject":["dolby"]`,
	} {
		r := e.put(t, "/api/v1/profiles/1", `{"name":"UHD","target":{"source":"remux","resolution":2160},`+bad+`}`)
		if r.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, body %s", name, r.Code, r.Body.String())
		}
	}
}

type deviceSource struct {
	fakeNotifier
	devices []ports.PlaybackDevice
	err     error
}

func (d deviceSource) PlaybackDevices(context.Context) ([]ports.PlaybackDevice, error) {
	return d.devices, d.err
}

// GET /hdr/devices derives each plurx device's order, ties and refusals, and
// reports a connection that could not answer instead of failing the list.
func TestHDRDevicesDeriveEachDevicesOrder(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	for _, n := range []ports.NotifierConfig{
		{Type: "plurx", Name: "cinema", Enabled: true, Settings: map[string]string{"url": "http://cinema"}},
		{Type: "plurx", Name: "offline", Enabled: true, Settings: map[string]string{"url": "http://offline"}},
		{Type: "plurx", Name: "disabled", Enabled: false, Settings: map[string]string{"url": "http://x"}},
		{Type: "webhook", Name: "hook", Enabled: true, Settings: map[string]string{"url": "http://hook"}},
	} {
		if _, err := e.db.AddNotifier(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.deps.NotifierFactory = func(cfg ports.NotifierConfig) ports.Notifier {
		switch cfg.Name {
		case "cinema":
			return deviceSource{devices: []ports.PlaybackDevice{{
				ID: "a1b2", User: "paul", Device: "Living room TV", Client: "plurx tvOS", LastSeen: 1,
				Caps: hdr.DeviceCaps{DolbyVision: true, HDR10: true},
			}}}
		case "offline":
			return deviceSource{err: errors.New("connection refused")}
		}
		return fakeNotifier{}
	}
	var out apigen.HDRDevices
	e.get(t, "/api/v1/hdr/devices").expect(t, http.StatusOK).into(t, &out)
	if len(out.Devices) != 1 || len(out.Errors) != 1 || out.Errors[0].Source != "offline" {
		t.Fatalf("devices = %+v errors = %+v", out.Devices, out.Errors)
	}
	d := out.Devices[0]
	if d.Device != "Living room TV" || !d.DolbyVision || d.Fel || len(d.HdrOrder) == 0 || len(d.HdrTies) == 0 {
		t.Errorf("device = %+v; a DV device without FEL ties FEL with MEL", d)
	}
	if !containsTier(d.HdrReject, hdr.TierDVP5) {
		t.Errorf("reject = %v; a device without profile 5 refuses it", d.HdrReject)
	}
}

func containsTier(ts []apigen.HDRTier, want hdr.Tier) bool {
	for _, t := range ts {
		if t == apigen.HDRTier(want) {
			return true
		}
	}
	return false
}

// The release and probe DTOs carry every field the evidence panel shows, and
// omit what is unknown rather than sending zeros.
func TestHDREvidenceDTOs(t *testing.T) {
	rel := releaseHDRDTO(acquisition.CandidateHDR{Tier: hdr.TierDVP7, Label: "DV P7", DVProfile: 7, ELType: "fel", Confidence: hdr.ConfidenceMedium})
	if rel.DvProfile == nil || *rel.DvProfile != 7 || rel.ElType == nil || rel.Confidence == nil {
		t.Errorf("release HDR = %+v", rel)
	}
	if bare := releaseHDRDTO(acquisition.CandidateHDR{Tier: hdr.TierHDR10, Label: "HDR10"}); bare.DvProfile != nil || bare.ElType != nil || bare.Confidence != nil {
		t.Errorf("bare release HDR = %+v", bare)
	}
	dovi := &mediainfo.DoViInfo{Profile: 7, Level: 6, RPU: true, EL: true, BL: true, BLCompatID: 6, ELType: "fel", CMVersion: "4.0", MaxCLL: 1000, MaxFALL: 400, MasteringMaxNits: 4000, Source: "rpu"}
	ev := probeEvidenceDTO(&acquisition.ProbeEvidence{
		Status: acquisition.ProbeMeasured, File: "Title.mkv", Path: []string{"yenc", "rar-store"},
		Articles: 6, Bytes: 4 << 20, Millis: 900, Container: "mkv", Facts: "2160p", Tier: hdr.TierDVP7FEL, DoVi: dovi,
	})
	if ev.DolbyVision == nil || *ev.DolbyVision.Level != 6 || *ev.DolbyVision.CmVersion != "4.0" || ev.Tier == nil || len(*ev.Path) != 2 || *ev.Millis != 900 {
		t.Errorf("probe evidence = %+v", ev)
	}
	if ev.Reason != nil || ev.Missing != nil {
		t.Errorf("unknown fields sent: %+v", ev)
	}
	if probeEvidenceDTO(nil) != nil || dolbyVisionDTO(mediainfo.Info{}) != nil {
		t.Error("absent evidence rendered")
	}
}
