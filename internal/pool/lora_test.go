package pool

import (
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/engine"
)

func loraEngine(maxLoRA int, adapters ...string) engine.Stats {
	return engine.Stats{Engine: "vllm", Running: 0, KVUsage: 0, Generated: -1, Adapters: adapters, MaxLoRA: maxLoRA}
}

func adapterReq(name string) Request {
	return Request{Model: name, Base: "llama", Exclude: map[string]bool{}}
}

func TestLoRAPrefersBackendWithAdapterLoaded(t *testing.T) {
	p := newPool(t, Options{EngineMaxAge: time.Hour, AffinityMaxImbalance: 0.5},
		be("a", config.TierStandard, 4), be("b", config.TierStandard, 4), be("c", config.TierStandard, 4))
	p.SetEngine(p.byName["a"], loraEngine(2, "sql-bot", "legal"), nil) // full, without ours
	p.SetEngine(p.byName["b"], loraEngine(2, "support"), nil)          // has ours
	p.SetEngine(p.byName["c"], loraEngine(2), nil)                     // empty slots
	l := acquire(t, p, adapterReq("support"))
	if l.Backend.Name != "b" || !l.AdapterLoaded || l.NeedsLoad {
		t.Fatalf("went to %s (loaded=%v needsLoad=%v), want b with the adapter", l.Backend.Name, l.AdapterLoaded, l.NeedsLoad)
	}
	// An adapter nobody has goes where a slot is free, not where loading
	// would evict another adapter.
	if got := acquire(t, p, adapterReq("new-one")).Backend.Name; got != "c" {
		t.Errorf("unloaded adapter went to %s, want c (free slot)", got)
	}
	// Requests for the base model are not affected.
	if got := acquire(t, p, Request{Model: "llama"}).Backend.Name; got != "a" {
		t.Errorf("base model request went to %s, want a (least loaded)", got)
	}
}

func TestLoRASpillsWhenHolderIsBusy(t *testing.T) {
	p := newPool(t, Options{EngineMaxAge: time.Hour, AffinityMaxImbalance: 0.5},
		be("a", config.TierStandard, 4), be("b", config.TierStandard, 4))
	p.SetEngine(p.byName["a"], loraEngine(4, "support"), nil)
	var got []string
	for range 4 {
		got = append(got, acquire(t, p, adapterReq("support")).Backend.Name)
	}
	// a holds it until it is more than half fuller than b.
	if want := []string{"a", "a", "a", "b"}; !equal(got, want) {
		t.Errorf("placements %v, want %v", got, want)
	}
}

func TestDynamicLoRAEligibility(t *testing.T) {
	static := be("static", config.TierStandard, 4)
	dyn := be("dyn", config.TierStandard, 4)
	dyn.DynamicLoRA = true
	p := newPool(t, Options{}, static, dyn)
	// Both list only the base model (as /v1/models would).
	p.SetDiscoveredModels(p.byName["static"], []string{"llama"})
	p.SetDiscoveredModels(p.byName["dyn"], []string{"llama"})

	l := acquire(t, p, adapterReq("support"))
	if l.Backend.Name != "dyn" || !l.NeedsLoad {
		t.Fatalf("went to %s needsLoad=%v; only dyn can load it", l.Backend.Name, l.NeedsLoad)
	}
	l.Release()
	p.NoteAdapter(p.byName["dyn"], "support")
	if l := acquire(t, p, adapterReq("support")); l.NeedsLoad || !l.AdapterLoaded {
		t.Errorf("after loading: needsLoad=%v loaded=%v", l.NeedsLoad, l.AdapterLoaded)
	}
	// A newer model list without the adapter (the engine restarted) wins
	// over the router's record.
	time.Sleep(time.Millisecond)
	p.SetDiscoveredModels(p.byName["dyn"], []string{"llama"})
	if l := acquire(t, p, adapterReq("support")); !l.NeedsLoad {
		t.Error("a model list newer than the load should make the router load it again")
	}
	// Without dynamic loading, an adapter must be listed.
	p.SetDiscoveredModels(p.byName["static"], []string{"llama", "support"})
	ctx := Request{Model: "support", Base: "llama", Exclude: map[string]bool{"dyn": true}}
	if l := acquire(t, p, ctx); l.Backend.Name != "static" || l.NeedsLoad || !l.AdapterLoaded {
		t.Errorf("listed adapter: %s needsLoad=%v loaded=%v", l.Backend.Name, l.NeedsLoad, l.AdapterLoaded)
	}
	if st := p.Snapshot(); len(st[1].LoRAAdapters) != 0 || !st[1].DynamicLoRA {
		t.Errorf("dyn status: %+v", st[1])
	}
}
