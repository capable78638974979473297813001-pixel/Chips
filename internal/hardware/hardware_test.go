package hardware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// golden is written by chipagent's own tests, so decoding it here proves the
// C++ agent and the Go router agree on the schema.
const golden = "../../agent/tests/testdata/telemetry.json"

func serve(t *testing.T, body string, status int) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TelemetryPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

func TestFetchGolden(t *testing.T) {
	raw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Fetch(context.Background(), http.DefaultClient, serve(t, string(raw), 200))
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "gpu-node-1" || len(r.Devices) != 2 {
		t.Fatalf("got %+v", r)
	}
	if got, want := r.Vendors(), []string{"amd", "nvidia"}; !slices.Equal(got, want) {
		t.Errorf("vendors = %v, want %v", got, want)
	}
	if got, want := r.MemoryTotal(), uint64(80+192)*gib; got != want {
		t.Errorf("memory total = %d, want %d", got, want)
	}
	if got := r.MemoryUsed(); got != gib {
		t.Errorf("memory used = %d", got)
	}
	// Only the H100 reports utilization; the MI300X's null must not count as 0.
	if u, ok := r.Utilization(); !ok || u != 35 {
		t.Errorf("utilization = %v, %v; want 35", u, ok)
	}
	if d := r.Devices[1]; d.TemperatureC == nil || *d.TemperatureC != 48.5 || d.PowerWatts == nil || *d.PowerWatts != 301.25 {
		t.Errorf("amd sensors = %+v", d)
	}
}

func TestFetchErrors(t *testing.T) {
	for name, u := range map[string]*url.URL{
		"status": serve(t, `{"schema":1}`, 500),
		"json":   serve(t, `not json`, 200),
		"schema": serve(t, `{"schema":2,"devices":[]}`, 200),
	} {
		if _, err := Fetch(context.Background(), http.DefaultClient, u); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func dev(vendor string, memGiB uint64, unified bool) Device {
	return Device{Vendor: vendor, MemoryTotalBytes: memGiB * gib, UnifiedMemory: unified}
}

func TestTier(t *testing.T) {
	cases := []struct {
		name    string
		devices []Device
		want    string
	}{
		{"cpu only", nil, config.TierLight},
		{"h100", []Device{dev("nvidia", 80, false)}, config.TierHeavy},
		{"mi300x", []Device{dev("amd", 192, false)}, config.TierHeavy},
		{"3x l4 adds up", []Device{dev("nvidia", 24, false), dev("nvidia", 24, false), dev("nvidia", 24, false)}, config.TierHeavy},
		{"l4", []Device{dev("nvidia", 24, false)}, config.TierStandard},
		{"radeon 7900", []Device{dev("amd", 24, false)}, config.TierStandard},
		{"t4", []Device{dev("nvidia", 16, false)}, config.TierLight},
		{"memory unknown", []Device{dev("intel", 0, false)}, config.TierStandard},
		{"m2 ultra capped", []Device{dev("apple", 192, true)}, config.TierStandard},
		{"small mac", []Device{dev("apple", 16, true)}, config.TierLight},
	}
	for _, c := range cases {
		if got := DefaultThresholds.Tier(&Report{Devices: c.devices}); got != c.want {
			t.Errorf("%s: tier = %s, want %s", c.name, got, c.want)
		}
	}
}
