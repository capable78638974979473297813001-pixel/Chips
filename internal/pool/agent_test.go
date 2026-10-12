package pool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/hardware"
)

const gib = 1 << 30

func report(vendor string, memGiB uint64, busy *float64) *hardware.Report {
	return &hardware.Report{Schema: 1, Devices: []hardware.Device{{
		Vendor: vendor, MemoryTotalBytes: memGiB * gib, UtilizationPercent: busy,
	}}}
}

func pct(v float64) *float64 { return &v }

func withAgent(b config.Backend) config.Backend {
	b.AgentURL = "http://" + b.Name + ":9400"
	return b
}

func auto(name string, slots int) config.Backend { return withAgent(be(name, config.TierAuto, slots)) }

func TestAutoTierFromHardware(t *testing.T) {
	p := newPool(t, Options{}, auto("mi300x", 4), auto("l4", 4), auto("t4", 4))
	b := p.byName
	if st := p.Snapshot()[0]; st.Tier != config.TierStandard || st.TierSource != "auto" || st.Hardware.AgentError == "" {
		t.Errorf("before polling: %+v", st)
	}
	p.SetHardware(b["mi300x"], report("amd", 192, nil), nil)
	p.SetHardware(b["l4"], report("nvidia", 24, nil), nil)
	p.SetHardware(b["t4"], report("nvidia", 16, nil), nil)

	// Vendor does not matter: the AMD node is heavy because of its memory.
	if got := acquire(t, p, Request{PromptTokens: 5000}).Backend.Name; got != "mi300x" {
		t.Errorf("large request went to %s, want mi300x", got)
	}
	if got := acquire(t, p, Request{PromptTokens: 10}).Backend.Name; got != "t4" {
		t.Errorf("small request went to %s, want t4", got)
	}
	st := p.Snapshot()
	if st[0].Tier != config.TierHeavy || st[1].Tier != config.TierStandard || st[2].Tier != config.TierLight {
		t.Errorf("tiers = %s %s %s", st[0].Tier, st[1].Tier, st[2].Tier)
	}
	if hw := st[0].Hardware; hw.AgentError != "" || hw.Vendors[0] != "amd" || hw.MemoryTotalBytes != 192*gib {
		t.Errorf("hardware status = %+v", hw)
	}
}

func TestConfiguredTierIsNotOverridden(t *testing.T) {
	p := newPool(t, Options{}, withAgent(be("pinned", config.TierReserve, 4)))
	p.SetHardware(p.byName["pinned"], report("nvidia", 80, nil), nil)
	if st := p.Snapshot()[0]; st.Tier != config.TierReserve || st.TierSource != "config" || st.Hardware.Accelerators != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestGPUBusyBreaksTies(t *testing.T) {
	p := newPool(t, Options{}, be("shared", config.TierStandard, 4), be("idle", config.TierStandard, 4))
	p.SetHardware(p.byName["shared"], report("nvidia", 24, pct(95)), nil)
	p.SetHardware(p.byName["idle"], report("amd", 24, pct(5)), nil)
	if got := acquire(t, p, Request{}).Backend.Name; got != "idle" {
		t.Errorf("went to %s, want idle (the other node's GPU is busy with outside work)", got)
	}

	// Within the noise margin, the usual tie-breaks apply (config order here).
	q := newPool(t, Options{}, be("a", config.TierStandard, 4), be("b", config.TierStandard, 4))
	q.SetHardware(q.byName["a"], report("nvidia", 24, pct(10)), nil)
	q.SetHardware(q.byName["b"], report("amd", 24, pct(5)), nil)
	if got := acquire(t, q, Request{}).Backend.Name; got != "a" {
		t.Errorf("went to %s, want a", got)
	}

	// Router-side load still comes first.
	r := newPool(t, Options{}, be("busy-gpu", config.TierStandard, 2), be("busy-router", config.TierStandard, 2))
	r.SetHardware(r.byName["busy-gpu"], report("nvidia", 24, pct(99)), nil)
	r.SetHardware(r.byName["busy-router"], report("nvidia", 24, pct(1)), nil)
	if got := acquire(t, r, Request{}).Backend.Name; got != "busy-router" {
		t.Errorf("went to %s, want busy-router while both are idle in the router", got)
	}
	if got := acquire(t, r, Request{}).Backend.Name; got != "busy-gpu" {
		t.Errorf("went to %s, want busy-gpu once busy-router has more requests in flight", got)
	}
}

func TestStaleTelemetryIsIgnored(t *testing.T) {
	p := newPool(t, Options{}, withAgent(be("a", config.TierStandard, 4)), withAgent(be("b", config.TierStandard, 4)))
	p.SetHardware(p.byName["b"], report("amd", 24, pct(5)), nil)
	p.SetHardware(p.byName["a"], report("nvidia", 24, pct(95)), nil)
	p.SetHardware(p.byName["a"], nil, errors.New("connection refused"))
	if got := acquire(t, p, Request{}).Backend.Name; got != "a" {
		t.Errorf("went to %s; stale 95%% reading should not count", got)
	}
	if hw := p.Snapshot()[0].Hardware; hw.AgentError != "connection refused" || hw.Accelerators != 1 || hw.UtilizationPercent != nil {
		t.Errorf("hardware status = %+v", hw)
	}
}

func TestAgentPoller(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"schema":1,"hostname":"n1","devices":[{"vendor":"amd","name":"AMD Instinct MI300X","memory_total_bytes":206158430208,"utilization_percent":12}]}`)
	}))
	defer agent.Close()
	cb := be("n1", config.TierAuto, 4)
	cb.AgentURL = agent.URL
	plain := be("n2", config.TierLight, 4)
	p := newPool(t, Options{}, cb, plain)

	ap := &AgentPoller{Pool: p, Client: agent.Client(), Interval: time.Hour, Timeout: time.Second}
	if !ap.HasAgents() {
		t.Fatal("HasAgents = false")
	}
	ap.PollAll(context.Background())
	st := p.Snapshot()
	if st[0].Tier != config.TierHeavy || st[0].Hardware.Hostname != "n1" || *st[0].Hardware.UtilizationPercent != 12 {
		t.Errorf("polled status = %+v / %+v", st[0], st[0].Hardware)
	}
	if st[1].Hardware != nil {
		t.Errorf("backend without agent_url has hardware status %+v", st[1].Hardware)
	}
}

func TestFailedAcceleratorTakesBackendOut(t *testing.T) {
	p := newPool(t, Options{}, withAgent(be("a", config.TierStandard, 2)), withAgent(be("b", config.TierStandard, 2)))
	a := p.byName["a"]
	lost := report("nvidia", 24, nil)
	lost.Devices = append(lost.Devices, hardware.Device{Vendor: "nvidia", Kind: "gpu", Name: "NVIDIA GPU 1", Error: "GPU is lost"})
	p.SetHardware(a, lost, nil)
	for range 3 {
		l := acquire(t, p, Request{})
		if l.Backend.Name != "b" {
			t.Error("backend with a lost GPU got new work")
		}
		l.Release()
	}
	onlyA := Request{Exclude: map[string]bool{"b": true}}
	if _, err := p.Acquire(context.Background(), onlyA); !errors.Is(err, ErrNoBackend) {
		t.Errorf("request only a could serve: err = %v, want ErrNoBackend", err)
	}
	if hs := p.Snapshot()[0].Hardware; len(hs.Faults) != 1 || hs.Faults[0] != "NVIDIA GPU 1: GPU is lost" {
		t.Errorf("faults = %v", hs.Faults)
	}
	// An unreachable agent keeps the fault; a healthy report clears it.
	p.SetHardware(a, nil, errors.New("connection refused"))
	if _, err := p.Acquire(context.Background(), onlyA); !errors.Is(err, ErrNoBackend) {
		t.Error("fault forgotten when the agent became unreachable")
	}
	p.SetHardware(a, report("nvidia", 24, nil), nil)
	if acquire(t, p, onlyA).Backend.Name != "a" {
		t.Error("backend not back after its GPUs recovered")
	}
}

func TestStaleAgentReadings(t *testing.T) {
	p := newPool(t, Options{}, auto("a", 2))
	a := p.byName["a"]
	p.SetHardware(a, report("amd", 192, nil), nil)
	if p.Snapshot()[0].Tier != config.TierHeavy {
		t.Fatal("setup: not heavy")
	}
	// A hung collection with nothing to fall back on: keep what we had.
	p.SetHardware(a, &hardware.Report{Schema: 1, Stale: true, CollectError: "collection did not finish"}, nil)
	st := p.Snapshot()[0]
	if st.Tier != config.TierHeavy || st.Hardware.Accelerators != 1 || !strings.Contains(st.Hardware.AgentError, "no readings yet") {
		t.Errorf("after an empty stale report: %+v %+v", st.Tier, st.Hardware)
	}
	// Stale but with readings: used, and flagged.
	stale := report("amd", 192, nil)
	stale.Stale, stale.CollectError = true, "readings are 12 s old"
	p.SetHardware(a, stale, nil)
	if hs := p.Snapshot()[0].Hardware; !strings.Contains(hs.AgentError, "stale readings") || hs.Accelerators != 1 {
		t.Errorf("stale report: %+v", hs)
	}
}
