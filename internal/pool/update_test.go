package pool

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/hardware"
)

func TestUpdateAddsRemovesAndKeepsState(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 2), be("b", config.TierStandard, 2))
	a := p.byName["a"]
	p.ObserveTTFT(a, 300*time.Millisecond)
	inflight := acquire(t, p, Request{})

	changes, err := p.Update([]config.Backend{be("a", config.TierHeavy, 8), be("c", config.TierLight, 2)}, Options{MaxWaiting: 100, LargeRequestTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"updated backend a", "added backend c", "removed backend b"}
	if !slices.Equal(changes, want) {
		t.Errorf("changes = %q, want %q", changes, want)
	}
	if p.byName["a"] != a || a.MaxConcurrency != 8 || a.Tier != config.TierHeavy || a.ttft == 0 {
		t.Errorf("a lost its identity or state: %+v", a)
	}
	if names := backendNames(p); !slices.Equal(names, []string{"a", "c"}) {
		t.Errorf("backends = %v", names)
	}
	// A lease taken before the update still releases cleanly, even if its
	// backend was removed.
	inflight.Release()
	if got := acquire(t, p, Request{PromptTokens: 5000}).Backend.Name; got != "a" {
		t.Errorf("large request went to %s, want a (now heavy)", got)
	}
}

func TestUpdateReplacesBackendWhenEndpointChanges(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 2))
	old := p.byName["a"]
	moved := be("a", config.TierStandard, 2)
	moved.URL = "http://new-host:8000"
	changes, _ := p.Update([]config.Backend{moved}, Options{MaxWaiting: 100})
	if len(changes) != 1 || p.byName["a"] == old || p.byName["a"].URL.Host != "new-host:8000" {
		t.Errorf("changes %q, backend %+v", changes, p.byName["a"])
	}
	if changes, _ := p.Update([]config.Backend{moved}, Options{MaxWaiting: 100}); len(changes) != 0 {
		t.Errorf("no-op update reported %q", changes)
	}
}

func TestUpdateUnblocksAndFailsWaiters(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 1))
	busy := acquire(t, p, Request{})
	got := make(chan error, 1)
	go func() {
		l, err := p.Acquire(context.Background(), Request{})
		if l != nil {
			l.Release()
		}
		got <- err
	}()
	for p.QueueDepth() != 1 {
		time.Sleep(time.Millisecond)
	}
	// More capacity: the waiter gets a slot at once.
	p.Update([]config.Backend{be("a", config.TierStandard, 2)}, Options{MaxWaiting: 100})
	if err := <-got; err != nil {
		t.Fatalf("waiter after capacity increase: %v", err)
	}
	// A waiter whose model nobody serves any more fails instead of waiting out its timeout.
	only := be("a", config.TierStandard, 2)
	only.Models = []string{"m1"}
	p.Update([]config.Backend{only}, Options{MaxWaiting: 100})
	l2 := acquire(t, p, Request{Model: "m1"})
	go func() {
		_, err := p.Acquire(context.Background(), Request{Model: "m1"})
		got <- err
	}()
	for p.QueueDepth() != 1 {
		time.Sleep(time.Millisecond)
	}
	other := be("a", config.TierStandard, 2)
	other.Models = []string{"m2"}
	p.Update([]config.Backend{other}, Options{MaxWaiting: 100})
	if err := <-got; !errors.Is(err, ErrNoBackend) {
		t.Errorf("waiter for a removed model: %v, want ErrNoBackend", err)
	}
	busy.Release()
	l2.Release()
}

func TestUpdateRetiersAutoBackends(t *testing.T) {
	p := newPool(t, Options{}, auto("gpu", 4))
	p.SetHardware(p.byName["gpu"], report("nvidia", 48, nil), nil) // standard under defaults
	if p.Snapshot()[0].Tier != config.TierStandard {
		t.Fatal("precondition")
	}
	p.Update([]config.Backend{auto("gpu", 4)}, Options{MaxWaiting: 100, Tiers: hwThresholds(40, 10)})
	if tier := p.Snapshot()[0].Tier; tier != config.TierHeavy {
		t.Errorf("after lowering the heavy threshold, tier = %s", tier)
	}
	// Switching a fixed-tier backend to auto uses its last hardware report.
	fixed := withAgent(be("gpu", config.TierReserve, 4))
	p.Update([]config.Backend{fixed}, Options{MaxWaiting: 100, Tiers: hwThresholds(40, 10)})
	if tier := p.Snapshot()[0].Tier; tier != config.TierReserve {
		t.Errorf("fixed tier not applied: %s", tier)
	}
	p.Update([]config.Backend{auto("gpu", 4)}, Options{MaxWaiting: 100, Tiers: hwThresholds(40, 10)})
	if tier := p.Snapshot()[0].Tier; tier != config.TierHeavy {
		t.Errorf("back to auto: tier = %s, want heavy from the last report", tier)
	}
}

// Concurrent requests, reloads and health signals must not race.
func TestUpdateUnderLoad(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 4), be("b", config.TierStandard, 4))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	for range 8 {
		go func() {
			for ctx.Err() == nil {
				if l, err := p.Acquire(ctx, Request{}); err == nil {
					l.Release()
				}
			}
			done <- struct{}{}
		}()
	}
	sets := [][]config.Backend{
		{be("a", config.TierStandard, 4), be("b", config.TierStandard, 4)},
		{be("a", config.TierHeavy, 1)},
		{be("b", config.TierStandard, 2), be("c", config.TierLight, 8)},
	}
	for i := 0; ctx.Err() == nil; i++ {
		p.Update(sets[i%len(sets)], Options{MaxWaiting: 100})
		for _, b := range p.Backends() {
			p.Observe(b, true, false, "")
		}
		p.Snapshot()
	}
	for range 8 {
		<-done
	}
}

func backendNames(p *Pool) []string {
	var out []string
	for _, b := range p.Backends() {
		out = append(out, b.Name)
	}
	return out
}

func hwThresholds(heavyGiB, standardGiB uint64) hardware.Thresholds {
	return hardware.Thresholds{HeavyMinBytes: heavyGiB * gib, StandardMinBytes: standardGiB * gib}
}
