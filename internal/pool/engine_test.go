package pool

import (
	"errors"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/engine"
)

func withMetrics(b config.Backend) config.Backend {
	b.MetricsPath = "/metrics"
	return b
}

func TestEngineQueueingBackendGoesLast(t *testing.T) {
	p := newPool(t, Options{}, withMetrics(be("a", config.TierStandard, 4)), withMetrics(be("b", config.TierStandard, 4)))
	a, b := p.Backends()[0], p.Backends()[1]
	if l := acquire(t, p, Request{}); l.Backend != a { // control: equal load, index order
		t.Fatalf("control picked %s", l.Backend.Name)
	} else {
		l.Release()
	}

	p.SetEngine(a, engine.Stats{Engine: "vllm", Running: 1, Waiting: 2, KVUsage: 0.3}, nil)
	var held []*Lease
	for range 4 {
		l := acquire(t, p, Request{})
		if l.Backend != b {
			t.Errorf("request went to %s, whose engine is queueing", l.Backend.Name)
		}
		held = append(held, l)
	}
	// With b full, the queueing engine is still better than waiting.
	if l := acquire(t, p, Request{}); l.Backend != a {
		t.Errorf("overflow went to %s", l.Backend.Name)
	}
	for _, l := range held {
		l.Release()
	}
}

func TestEngineLoadAndKVBreakTies(t *testing.T) {
	p := newPool(t, Options{}, withMetrics(be("a", config.TierStandard, 16)), withMetrics(be("b", config.TierStandard, 16)))
	a, b := p.Backends()[0], p.Backends()[1]
	// Traffic the router cannot see: another client keeps 10 requests on a.
	p.SetEngine(a, engine.Stats{Engine: "vllm", Running: 10, KVUsage: 0.2}, nil)
	p.SetEngine(b, engine.Stats{Engine: "vllm", Running: 0, KVUsage: 0.1}, nil)
	if l := acquire(t, p, Request{}); l.Backend != b {
		t.Errorf("picked %s, busier per its engine", l.Backend.Name)
	}
	// A fuller KV cache counts as load too.
	p.SetEngine(a, engine.Stats{Engine: "sglang", Running: 0, KVUsage: 0.7}, nil)
	p.SetEngine(b, engine.Stats{Engine: "sglang", Running: 0, KVUsage: 0.1}, nil)
	if l := acquire(t, p, Request{}); l.Backend != b {
		t.Errorf("picked %s, with the fuller KV cache", l.Backend.Name)
	}
}

func TestSaturatedPrefixHolderIsSkipped(t *testing.T) {
	p := newPool(t, Options{KVSaturated: 0.9, AffinityMaxImbalance: 1}, withMetrics(be("a", config.TierStandard, 8)), withMetrics(be("b", config.TierStandard, 8)))
	a, b := p.Backends()[0], p.Backends()[1]
	p.SetEngine(b, engine.Stats{Engine: "vllm", Running: 0, KVUsage: 0.5}, nil)
	if l := acquire(t, p, Request{Preferred: []string{"b"}}); l.Backend != b || !l.Affinity {
		t.Fatalf("control: prefix holder not used")
	} else {
		l.Release()
	}
	p.SetEngine(b, engine.Stats{Engine: "vllm", Running: 0, KVUsage: 0.93}, nil)
	if l := acquire(t, p, Request{Preferred: []string{"b"}}); l.Backend != a {
		t.Errorf("saturated prefix holder %s still chosen", l.Backend.Name)
	}
}

func TestStaleEngineStatsAreIgnored(t *testing.T) {
	p := newPool(t, Options{EngineMaxAge: 20 * time.Millisecond}, withMetrics(be("a", config.TierStandard, 4)), withMetrics(be("b", config.TierStandard, 4)))
	a := p.Backends()[0]
	p.SetEngine(a, engine.Stats{Engine: "vllm", Running: 0, Waiting: 5, KVUsage: -1}, nil)
	if l := acquire(t, p, Request{}); l.Backend == a {
		t.Fatal("fresh stats ignored")
	} else {
		l.Release()
	}
	time.Sleep(30 * time.Millisecond)
	p.SetEngine(a, engine.Stats{}, errors.New("connection refused")) // a failed scrape keeps nothing fresh
	if l := acquire(t, p, Request{}); l.Backend != a {
		t.Errorf("stale stats still steer routing: picked %s", l.Backend.Name)
	}
	st := p.Snapshot()[0].Engine
	if st == nil || !st.Stale || st.Error != "connection refused" || st.Waiting != 5 {
		t.Errorf("status = %+v", st)
	}
}

func TestEngineStatus(t *testing.T) {
	p := newPool(t, Options{}, withMetrics(be("a", config.TierStandard, 4)), be("plain", config.TierStandard, 4))
	if p.Snapshot()[1].Engine != nil {
		t.Error("backend without metrics_path has engine status")
	}
	if st := p.Snapshot()[0].Engine; st == nil || st.Error != "not scraped yet" {
		t.Errorf("before scrape: %+v", st)
	}
	p.SetEngine(p.Backends()[0], engine.Stats{Engine: "tgi", Running: 3, Waiting: 1, KVUsage: -1}, nil)
	st := p.Snapshot()[0].Engine
	if st.Engine != "tgi" || *st.Running != 3 || st.KVUsage != nil || !st.Saturated || st.Stale {
		t.Errorf("status = %+v", st)
	}
	if u := p.Backends()[0].MetricsURL.String(); u != "http://a/metrics" {
		t.Errorf("metrics url = %s", u)
	}
}
