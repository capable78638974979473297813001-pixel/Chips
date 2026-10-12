package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/engine"
)

// feed reports engine stats for b every step for the given duration,
// adding perStep generated tokens each time; it returns the final count.
func feed(p *Pool, b *Backend, running, generated, perStep float64, d time.Duration) float64 {
	const step = 10 * time.Millisecond
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(step) {
		generated += perStep
		p.SetEngine(b, engine.Stats{Engine: "vllm", Running: running, Waiting: 0, KVUsage: 0.1, Generated: generated}, nil)
	}
	return generated
}

func stallPool(t *testing.T) (*Pool, *Backend, *Backend) {
	p := newPool(t, Options{StallAfter: 80 * time.Millisecond, StallMinRate: 1},
		withMetrics(be("a", config.TierStandard, 8)), withMetrics(be("b", config.TierStandard, 8)))
	return p, p.Backends()[0], p.Backends()[1]
}

func TestStalledEngineLeavesRotationAndReturns(t *testing.T) {
	p, a, b := stallPool(t)
	// 8 requests running on a, almost no tokens: a hung GPU.
	gen := feed(p, a, 8, 1000, 0.01, 150*time.Millisecond)
	if st := p.Snapshot()[0].Engine; st == nil || !st.Stalled {
		t.Fatalf("not marked stalled: %+v", st)
	}
	for range 3 {
		if l := acquire(t, p, Request{}); l.Backend != b {
			t.Errorf("stalled backend got new work")
		}
	}
	// Tokens flow again (8 requests at hundreds of tokens per second each).
	feed(p, a, 8, gen, 50, 30*time.Millisecond)
	if p.Snapshot()[0].Engine.Stalled {
		t.Fatal("still stalled after tokens flowed")
	}
}

func TestBusyOrIdleEnginesAreNotStalled(t *testing.T) {
	p, a, b := stallPool(t)
	feed(p, a, 8, 0, 50, 150*time.Millisecond)  // busy and generating
	feed(p, b, 0, 500, 0, 150*time.Millisecond) // idle: no requests, no tokens
	for _, st := range p.Snapshot() {
		if st.Engine.Stalled {
			t.Errorf("%s marked stalled", st.Name)
		}
	}
}

func TestEngineRestartIsNotAStall(t *testing.T) {
	p, a, _ := stallPool(t)
	feed(p, a, 4, 90000, 40, 100*time.Millisecond)
	feed(p, a, 4, 0, 40, 100*time.Millisecond) // counter reset by a restart, then healthy
	if p.Snapshot()[0].Engine.Stalled {
		t.Error("counter reset taken for a stall")
	}
}

func TestOnlyBackendStalledFailsFast(t *testing.T) {
	p := newPool(t, Options{StallAfter: 80 * time.Millisecond, StallMinRate: 1}, withMetrics(be("a", config.TierStandard, 1)))
	a := p.Backends()[0]
	busy := acquire(t, p, Request{})
	errc := make(chan error, 1)
	go func() {
		_, err := p.Acquire(context.Background(), Request{})
		errc <- err
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	feed(p, a, 1, 0, 0, 150*time.Millisecond)
	select {
	case err := <-errc:
		if !errors.Is(err, ErrNoBackend) {
			t.Errorf("queued request: err = %v, want ErrNoBackend", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request still waiting on a stalled backend")
	}
	busy.Release()
}
