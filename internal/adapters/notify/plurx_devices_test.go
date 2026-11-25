package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The playback-devices contract (plurx docs/API.md, ADR 0031 §5): the key is
// sent as a bearer, the HDR summary maps onto hdr.DeviceCaps, and the two
// refusals an operator can fix say what to fix.
func TestPlurxPlaybackDevices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/playback/devices" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer plx_reader":
			_, _ = w.Write([]byte(`{"devices":[{"id":"abcd1234","user":"paul","device":"Living room","client":"linux-player",
				"last_seen_at":1795560000,"recorded_at":1795560000,
				"hdr":{"dolby_vision":true,"dv_profiles":[5,7,8],"fel":true,"profile5":true,"hdr10":true,"hlg":true,"hdr10plus":null}},
				{"id":"ef567890","user":"paul","device":null,"client":"web","last_seen_at":1,"recorded_at":1,
				"hdr":{"dolby_vision":false,"dv_profiles":[],"fel":false,"profile5":false,"hdr10":true,"hlg":false,"hdr10plus":null}}]}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()

	devices, err := (&Plurx{URL: srv.URL, APIKey: "plx_reader"}).PlaybackDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	lr := devices[0]
	if lr.Device != "Living room" || lr.Client != "linux-player" || !lr.Caps.DolbyVision || !lr.Caps.FEL ||
		!lr.Caps.Profile5 || !lr.Caps.HDR10 || !lr.Caps.HLG || lr.Caps.HDR10Plus {
		t.Errorf("living room = %+v", lr)
	}
	if web := devices[1]; web.Device != "" || web.Caps.DolbyVision || !web.Caps.HDR10 {
		t.Errorf("web = %+v", web)
	}

	_, err = (&Plurx{URL: srv.URL, APIKey: "plx_scanonly"}).PlaybackDevices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "playback:read") {
		t.Errorf("a key without the scope says which scope to add: %v", err)
	}
	_, err = (&Plurx{URL: srv.URL + "/old", APIKey: "plx_reader"}).PlaybackDevices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an older plurx says it does not report devices: %v", err)
	}
}
