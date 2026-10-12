package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func TestDrainStopsNewTrafficAndLetsInflightFinish(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 2), be("b", config.TierStandard, 2))
	first := acquire(t, p, Request{Preferred: []string{"a"}})
	if first.Backend.Name != "a" {
		t.Fatalf("setup: got %s", first.Backend.Name)
	}
	st, ok := p.SetDrained("a", true)
	if !ok || !st.Drained || st.Inflight != 1 {
		t.Fatalf("SetDrained = %+v, %v", st, ok)
	}
	for range 2 {
		if l := acquire(t, p, Request{Preferred: []string{"a"}}); l.Backend.Name != "b" {
			t.Errorf("drained backend got new work")
		}
	}
	first.Release() // the in-flight request finishes normally
	if s := p.Snapshot()[0]; s.Inflight != 0 || !s.Drained {
		t.Errorf("after release: %+v", s)
	}
	if _, ok := p.SetDrained("nope", true); ok {
		t.Error("unknown backend reported as drained")
	}
}

func TestDrainingTheOnlyBackendFailsWaitersAndUndrainRestores(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 1))
	busy := acquire(t, p, Request{})
	errc := make(chan error, 1)
	go func() {
		_, err := p.Acquire(context.Background(), Request{})
		errc <- err
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	p.SetDrained("a", true)
	if err := <-errc; !errors.Is(err, ErrNoBackend) {
		t.Errorf("queued request: err = %v, want ErrNoBackend", err)
	}
	busy.Release()
	if _, err := p.Acquire(context.Background(), Request{}); !errors.Is(err, ErrNoBackend) {
		t.Errorf("new request while drained: err = %v, want ErrNoBackend", err)
	}
	p.SetDrained("a", false)
	acquire(t, p, Request{}).Release()
}

func TestDrainSurvivesReload(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 1), be("b", config.TierStandard, 1))
	p.SetDrained("a", true)
	moved := be("a", config.TierStandard, 4)
	moved.URL = "http://a-new"
	if _, err := p.Update([]config.Backend{moved, be("b", config.TierStandard, 1)}, p.opts); err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Snapshot() {
		if s.Name == "a" && !s.Drained {
			t.Error("drain lost when the backend was replaced by a reload")
		}
	}
}

func TestHighPriorityShedsLowPriorityFromFullQueue(t *testing.T) {
	p := newPool(t, Options{MaxWaiting: 2}, be("a", config.TierStandard, 1))
	busy := acquire(t, p, Request{})
	type out struct {
		name string
		err  error
	}
	results := make(chan out, 4)
	start := func(name string, prio int) {
		go func() {
			l, err := p.Acquire(context.Background(), Request{Priority: prio})
			if l != nil {
				l.Release()
			}
			results <- out{name, err}
		}()
	}
	start("low-old", 0)
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	start("low-new", 0)
	waitFor(t, func() bool { return p.QueueDepth() == 2 })

	// Equal priority is still turned away: shedding never reorders peers.
	if _, err := p.Acquire(context.Background(), Request{Priority: 0}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("equal priority: err = %v, want ErrQueueFull", err)
	}

	start("high", 5)
	r := <-results
	if r.name != "low-new" || !errors.Is(r.err, ErrShed) {
		t.Fatalf("first result = %+v, want low-new shed", r)
	}
	waitFor(t, func() bool { return p.QueueDepth() == 2 })
	busy.Release()
	got := map[string]error{}
	for range 2 {
		select {
		case r := <-results:
			got[r.name] = r.err
		case <-time.After(time.Second):
			t.Fatal("waiters not served")
		}
	}
	if got["high"] != nil || got["low-old"] != nil || len(got) != 2 {
		t.Errorf("served = %v, want high and low-old to succeed", got)
	}
}
