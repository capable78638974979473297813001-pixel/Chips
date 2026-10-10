package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func newPool(t *testing.T, opts Options, backends ...config.Backend) *Pool {
	t.Helper()
	if opts.MaxWaiting == 0 {
		opts.MaxWaiting = 100
	}
	if opts.LargeRequestTokens == 0 {
		opts.LargeRequestTokens = 1000
	}
	if opts.UnhealthyThreshold == 0 {
		opts.UnhealthyThreshold = 2
	}
	if opts.HealthyThreshold == 0 {
		opts.HealthyThreshold = 2
	}
	p, err := New(backends, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func be(name, tier string, slots int) config.Backend {
	return config.Backend{Name: name, URL: "http://" + name, Tier: tier, MaxConcurrency: slots}
}

func acquire(t *testing.T, p *Pool, r Request) *Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, r)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return l
}

func TestTierPreferenceBySize(t *testing.T) {
	p := newPool(t, Options{},
		be("heavy", config.TierHeavy, 4), be("std", config.TierStandard, 4),
		be("light", config.TierLight, 4), be("reserve", config.TierReserve, 4))
	if got := acquire(t, p, Request{PromptTokens: 10}).Backend.Name; got != "light" {
		t.Errorf("small request went to %s, want light", got)
	}
	if got := acquire(t, p, Request{PromptTokens: 5000}).Backend.Name; got != "heavy" {
		t.Errorf("large request went to %s, want heavy", got)
	}
}

func TestSpillsOverAndUsesReserveLast(t *testing.T) {
	p := newPool(t, Options{}, be("reserve", config.TierReserve, 1), be("light", config.TierLight, 1), be("heavy", config.TierHeavy, 1))
	var got []string
	for range 3 {
		got = append(got, acquire(t, p, Request{PromptTokens: 10}).Backend.Name)
	}
	if want := []string{"light", "heavy", "reserve"}; !equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestLeastUtilizedWithinTier(t *testing.T) {
	p := newPool(t, Options{}, be("small", config.TierStandard, 2), be("big", config.TierStandard, 8))
	var got []string
	for range 4 {
		got = append(got, acquire(t, p, Request{}).Backend.Name)
	}
	// big wins ties on free slots; small only once big is busier proportionally.
	if want := []string{"big", "small", "big", "big"}; !equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestModelAndContextFilters(t *testing.T) {
	llama := be("llama", config.TierStandard, 4)
	llama.Models = []string{"llama"}
	tiny := be("tiny", config.TierLight, 4)
	tiny.MaxContextTokens = 100
	tiny.Models = []string{"llama", "phi"}
	p := newPool(t, Options{}, llama, tiny)

	if got := acquire(t, p, Request{Model: "llama", PromptTokens: 10}).Backend.Name; got != "tiny" {
		t.Errorf("small request should prefer the light tier, got %s", got)
	}
	if got := acquire(t, p, Request{Model: "phi", PromptTokens: 10}).Backend.Name; got != "tiny" {
		t.Errorf("model filter: got %s", got)
	}
	if got := acquire(t, p, Request{PromptTokens: 90, MaxTokens: 50}).Backend.Name; got != "llama" {
		t.Errorf("context filter: got %s", got)
	}
	if _, err := p.Acquire(context.Background(), Request{Model: "mistral", PromptTokens: 90, MaxTokens: 50}); !errors.Is(err, ErrNoBackend) {
		t.Errorf("err = %v, want ErrNoBackend", err)
	}
}

func TestDiscoveredModels(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 1))
	p.SetDiscoveredModels(p.Backends()[0], []string{"qwen"})
	if _, err := p.Acquire(context.Background(), Request{Model: "llama"}); !errors.Is(err, ErrNoBackend) {
		t.Errorf("err = %v", err)
	}
	acquire(t, p, Request{Model: "qwen"})
}

func TestAffinityRespectsImbalance(t *testing.T) {
	p := newPool(t, Options{AffinityMaxImbalance: 0.5}, be("a", config.TierStandard, 4), be("b", config.TierStandard, 4))
	l := acquire(t, p, Request{Preferred: "b"})
	if l.Backend.Name != "b" || !l.Affinity {
		t.Fatalf("preferred backend not chosen: %s", l.Backend.Name)
	}
	acquire(t, p, Request{Preferred: "b"}) // b at 1/4 vs a at 0/4
	acquire(t, p, Request{Preferred: "b"}) // b at 2/4 vs a at 0/4: still within 0.5
	l = acquire(t, p, Request{Preferred: "b"})
	if l.Backend.Name != "a" || l.Affinity {
		t.Errorf("b at 3/4 vs a at 0/4 should rebalance to a, got %s", l.Backend.Name)
	}
}

func TestQueueFIFOAndRelease(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 1))
	first := acquire(t, p, Request{})

	results := make(chan int, 2)
	for i := range 2 {
		go func() {
			l, err := p.Acquire(context.Background(), Request{})
			if err != nil {
				t.Error(err)
				return
			}
			results <- i
			time.Sleep(10 * time.Millisecond)
			l.Release()
		}()
		waitFor(t, func() bool { return p.QueueDepth() == i+1 })
	}
	first.Release()
	if a, b := <-results, <-results; a != 0 || b != 1 {
		t.Errorf("served out of order: %d then %d", a, b)
	}
}

func TestSmallRequestsSkipBlockedLargeOne(t *testing.T) {
	big := be("big", config.TierHeavy, 1)
	small := be("small", config.TierLight, 1)
	small.MaxContextTokens = 100
	p := newPool(t, Options{}, big, small)
	bigLease := acquire(t, p, Request{PromptTokens: 5000})
	smallLease := acquire(t, p, Request{PromptTokens: 10})

	done := make(chan string, 2)
	go func() {
		l, _ := p.Acquire(context.Background(), Request{PromptTokens: 5000})
		done <- "large:" + l.Backend.Name
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	go func() {
		l, _ := p.Acquire(context.Background(), Request{PromptTokens: 10})
		done <- "small:" + l.Backend.Name
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 2 })
	smallLease.Release() // only the small request can use this slot
	if got := <-done; got != "small:small" {
		t.Errorf("got %s", got)
	}
	bigLease.Release()
	if got := <-done; got != "large:big" {
		t.Errorf("got %s", got)
	}
}

func TestQueueFullAndTimeout(t *testing.T) {
	p := newPool(t, Options{MaxWaiting: 1}, be("a", config.TierStandard, 1))
	acquire(t, p, Request{})
	go p.Acquire(context.Background(), Request{})
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	if _, err := p.Acquire(context.Background(), Request{}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}

	p2 := newPool(t, Options{}, be("a", config.TierStandard, 1))
	acquire(t, p2, Request{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p2.Acquire(ctx, Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline", err)
	}
	if p2.QueueDepth() != 0 {
		t.Error("timed-out waiter left in queue")
	}
}

func TestHealthTransitions(t *testing.T) {
	p := newPool(t, Options{PassiveHealth: true}, be("a", config.TierStandard, 1), be("b", config.TierStandard, 1))
	a, b := p.Backends()[0], p.Backends()[1]

	// First active check decides immediately.
	p.Observe(a, false, false, "connection refused")
	if p.Snapshot()[0].Healthy {
		t.Fatal("first failed check should mark backend down")
	}
	p.Observe(a, true, false, "")
	if p.Snapshot()[0].Healthy {
		t.Fatal("one success should not be enough to recover")
	}
	p.Observe(a, true, false, "")
	if !p.Snapshot()[0].Healthy {
		t.Fatal("two successes should recover")
	}

	// A waiter whose only backend goes down is failed, not left hanging.
	a.models = map[string]bool{"only-a": true}
	b.models = map[string]bool{"only-b": true}
	hold := acquire(t, p, Request{Model: "only-b"})
	errc := make(chan error, 1)
	go func() {
		_, err := p.Acquire(context.Background(), Request{Model: "only-b"})
		errc <- err
	}()
	waitFor(t, func() bool { return p.QueueDepth() == 1 })
	p.Observe(b, true, false, "") // first check: healthy
	p.Observe(b, false, true, "boom")
	p.Observe(b, false, true, "boom")
	if err := <-errc; !errors.Is(err, ErrNoBackend) {
		t.Errorf("err = %v, want ErrNoBackend", err)
	}
	hold.Release()
}

func TestPassiveHealthOff(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 1))
	for range 5 {
		p.Observe(p.Backends()[0], false, true, "x")
	}
	if !p.Snapshot()[0].Healthy {
		t.Error("passive failures must be ignored when passive health is off")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p := newPool(t, Options{}, be("a", config.TierStandard, 2))
	l := acquire(t, p, Request{})
	l.Release()
	l.Release()
	if got := p.Snapshot()[0].Inflight; got != 0 {
		t.Errorf("inflight = %d", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPriorityJumpsTheQueue(t *testing.T) {
	p := newPool(t, Options{}, be("only", config.TierStandard, 1))
	busy := acquire(t, p, Request{})
	order := make(chan string, 4)
	for i, w := range []struct {
		name string
		prio int
	}{{"low", 0}, {"normal-1", 1}, {"high", 2}, {"normal-2", 1}} {
		go func() {
			l, err := p.Acquire(context.Background(), Request{Priority: w.prio})
			if err != nil {
				t.Error(err)
				return
			}
			order <- w.name
			l.Release()
		}()
		for p.QueueDepth() != i+1 { // enqueue strictly in this order
			time.Sleep(time.Millisecond)
		}
	}
	busy.Release()
	var seen []string
	for range 4 {
		seen = append(seen, <-order)
	}
	if want := []string{"high", "normal-1", "normal-2", "low"}; !equal(seen, want) {
		t.Errorf("served %v, want %v", seen, want)
	}
}

func TestFasterBackendWinsTies(t *testing.T) {
	p := newPool(t, Options{}, be("slow", config.TierStandard, 4), be("fast", config.TierStandard, 4))
	p.ObserveTTFT(p.byName["slow"], 900*time.Millisecond)
	p.ObserveTTFT(p.byName["fast"], 200*time.Millisecond)
	if got := acquire(t, p, Request{}).Backend.Name; got != "fast" {
		t.Errorf("went to %s, want fast", got)
	}
	// Load still comes first: fast now has one in flight, slow none.
	if got := acquire(t, p, Request{}).Backend.Name; got != "slow" {
		t.Errorf("went to %s, want slow once fast is busier", got)
	}
	// Within the margin it is a tie, decided by config order.
	q := newPool(t, Options{}, be("a", config.TierStandard, 4), be("b", config.TierStandard, 4))
	q.ObserveTTFT(q.byName["a"], 300*time.Millisecond)
	q.ObserveTTFT(q.byName["b"], 250*time.Millisecond)
	if got := acquire(t, q, Request{}).Backend.Name; got != "a" {
		t.Errorf("went to %s, want a (within margin)", got)
	}
	if ms := q.Snapshot()[1].TTFTMillis; ms != 250 {
		t.Errorf("ttft_ms = %v", ms)
	}
	// The average moves toward new samples.
	q.ObserveTTFT(q.byName["b"], 1250*time.Millisecond)
	if ms := q.Snapshot()[1].TTFTMillis; ms != 450 {
		t.Errorf("ttft_ms after a slow sample = %v, want 450", ms)
	}
}
