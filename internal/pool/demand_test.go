package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func TestColdStartHoldsUntilBackendIsUp(t *testing.T) {
	p := newPool(t, Options{ColdStart: true}, be("gpu", config.TierStandard, 2))
	b := p.byName["gpu"]
	p.Observe(b, false, false, "booting") // first check: down

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, Request{Model: "m"}); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("without Hold: err %v, want ErrNoBackend", err)
	}
	if !p.Cold(Request{Model: "m"}) {
		t.Error("Cold should report a model whose only backend is down")
	}

	got := make(chan *Lease, 1)
	go func() {
		l, err := p.Acquire(context.Background(), Request{Model: "m", Hold: true})
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	d := p.Demand(0.8, 16)
	if len(d) != 1 || d[0].Model != "m" || d[0].Held != 1 || d[0].Queued != 1 || d[0].DesiredBackends != 1 {
		t.Errorf("demand while held = %+v", d)
	}
	p.Observe(b, true, false, "")
	p.Observe(b, true, false, "") // healthy_threshold 2: back up
	select {
	case l := <-got:
		if l.Backend != b {
			t.Errorf("granted %v", l.Backend.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("held request not granted once the backend came up")
	}
	if p.Cold(Request{Model: "m"}) {
		t.Error("not cold once up")
	}
}

func TestScaleToZeroModelWaitsForNewBackend(t *testing.T) {
	other := be("other", config.TierStandard, 2)
	other.Models = []string{"small"}
	opts := Options{ColdStart: true, ColdModels: map[string]bool{"big": true}}
	p := newPool(t, opts, other)
	if d := p.Demand(0.8, 16); len(d) != 2 || d[0].Model != "big" || d[0].ReadyBackends != 0 {
		t.Errorf("scale-to-zero model should be listed with no backends: %+v", d)
	}
	got := make(chan error, 1)
	go func() {
		_, err := p.Acquire(context.Background(), Request{Model: "big", Hold: true})
		got <- err
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	// The autoscaler starts a backend; discovery adds it.
	big := be("big-0", config.TierHeavy, 4)
	big.Models = []string{"big"}
	if _, err := p.Update([]config.Backend{other, big}, opts); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("not granted when a backend for the model joined")
	}
	// A model nothing could ever serve still fails fast.
	if _, err := p.Acquire(context.Background(), Request{Model: "nope", Hold: true}); !errors.Is(err, ErrNoBackend) {
		t.Errorf("unknown model: %v", err)
	}
}

func TestDemandCountsInflightAndDesired(t *testing.T) {
	a, b := be("a", config.TierStandard, 4), be("b", config.TierStandard, 4)
	a.Models, b.Models = []string{"m"}, []string{"m"}
	a.DynamicLoRA, b.DynamicLoRA = true, true
	p := newPool(t, Options{}, a, b)
	for range 7 {
		acquire(t, p, Request{Model: "m"})
	}
	l := acquire(t, p, Request{Model: "sup", Base: "m"}) // adapters count toward their base
	d := p.Demand(0.5, 16)
	if len(d) != 1 || d[0].Inflight != 8 || d[0].ReadyBackends != 2 || d[0].Capacity != 8 || d[0].DesiredBackends != 4 {
		t.Errorf("demand = %+v, want 8 in flight on 2 backends, 4 desired at 50%%", d)
	}
	l.Release()
	if d := p.Demand(0.5, 16); d[0].Inflight != 7 {
		t.Errorf("after release: %+v", d)
	}
}
