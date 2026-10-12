package pool

import (
	"math"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/engine"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A backend's running cost is split between the requests running at each
// moment, and time with nothing running is idle.
func TestCostSplitsBetweenConcurrentRequests(t *testing.T) {
	cb := be("h100", config.TierHeavy, 4)
	cb.CostPerHour = 3600 // $1 a second
	cb.HardwareName = "H100"
	p := newPool(t, Options{}, cb)
	t0 := time.Now()
	clock := t0
	p.now = func() time.Time { return clock }
	p.byName["h100"].costAt = t0

	clock = t0.Add(2 * time.Second) // 2s idle
	a := acquire(t, p, Request{})
	clock = t0.Add(3 * time.Second) // a alone for 1s
	b := acquire(t, p, Request{})
	clock = t0.Add(5 * time.Second) // both for 2s
	a.Release()
	clock = t0.Add(6 * time.Second) // b alone for 1s
	b.Release()
	clock = t0.Add(10 * time.Second) // 4s idle

	if got := a.Cost(0, 0); !near(got, 2) { // 1 + 2/2
		t.Errorf("a cost $%v, want $2", got)
	}
	if got := b.Cost(0, 0); !near(got, 2) { // 2/2 + 1
		t.Errorf("b cost $%v, want $2", got)
	}
	if a.Hardware() != "H100" {
		t.Errorf("hardware = %q", a.Hardware())
	}
	idle := p.IdleCosts()
	if len(idle) != 1 || !near(idle[0].Dollars, 6) || idle[0].Hardware != "H100" {
		t.Errorf("idle = %+v, want $6 on H100", idle)
	}
}

// Requests the engine reports running but the router did not send (other
// routers, direct clients) take their share of the cost.
func TestCostCountsEngineRequests(t *testing.T) {
	cb := be("gpu", config.TierStandard, 4)
	cb.CostPerHour = 3600
	cb.MetricsPath = "/metrics"
	p := newPool(t, Options{EngineMaxAge: time.Hour}, cb)
	b := p.byName["gpu"]
	t0 := time.Now()
	clock := t0
	p.now = func() time.Time { return clock }
	b.costAt = t0
	l := acquire(t, p, Request{})
	// The engine says 4 requests run: ours and 3 others.
	p.SetEngine(b, engine.Stats{Engine: "vllm", Running: 4, KVUsage: -1, Generated: -1}, nil)
	clock = t0.Add(4 * time.Second)
	l.Release()
	if got := l.Cost(0, 0); !near(got, 1) {
		t.Errorf("cost $%v, want $1 (a quarter of 4s at $1/s)", got)
	}
}

func TestTokenPricesAndHardwareFromConfig(t *testing.T) {
	cb := be("api", config.TierStandard, 4)
	cb.CostPerMillionPromptTokens = 3
	cb.CostPerMillionCompletionTokens = 15
	p := newPool(t, Options{}, cb)
	l := acquire(t, p, Request{})
	l.Release()
	if got := l.Cost(1_000_000, 2_000_000); !near(got, 33) {
		t.Errorf("cost $%v, want $33", got)
	}
	if l.Hardware() != "api" {
		t.Errorf("hardware defaults to the backend name, got %q", l.Hardware())
	}
	if len(p.IdleCosts()) != 0 {
		t.Error("a backend without cost_per_hour has no idle cost")
	}
}
