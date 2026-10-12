package pool

import (
	"slices"
	"time"
)

// LoRA-aware routing. Many fine-tuned adapters often share one base model
// and its GPUs. An engine serves an adapter fastest when the adapter is
// already loaded: loading means reading weights and, when its adapter slots
// (vLLM's --max-loras) are full, evicting another adapter that requests
// then wait for. So a request for an adapter prefers:
//
//  1. a backend that has the adapter loaded, or is loading it, unless it is
//     much busier than the least busy alternative (routing.affinity_max_imbalance);
//  2. a backend with a free adapter slot;
//  3. any other backend that can serve it.
//
// A backend "has" an adapter if its model list says so (configured, or
// /v1/models, where vLLM lists loaded adapters), its engine metrics show
// requests for it, or this router loaded or used it there since the model
// list was last refreshed.

// servesRequest reports whether b can take r: it serves r's model, or r
// names an adapter of a base model b serves and b can load adapters.
func (b *Backend) servesRequest(r Request) bool {
	if b.serves(r.Model) {
		return true
	}
	return r.Base != "" && b.dynamicLoRA && b.serves(r.Base)
}

// hasAdapterLocked reports whether b is known to have adapter loaded.
func (b *Backend) hasAdapterLocked(adapter string, now time.Time) bool {
	if b.models[adapter] || b.discovered[adapter] {
		return true
	}
	if st, ok := b.engineStats(now); ok && slices.Contains(st.Adapters, adapter) {
		return true
	}
	// A load or use newer than the last model list: the list may not show it yet.
	t, ok := b.adapters[adapter]
	return ok && (b.discoveredAt.IsZero() || t.After(b.discoveredAt))
}

// adapterRoomLocked reports whether b's engine has a free adapter slot, as
// far as its metrics say; true when it doesn't report.
func (b *Backend) adapterRoomLocked(now time.Time) bool {
	st, ok := b.engineStats(now)
	return !ok || st.MaxLoRA < 0 || len(st.Adapters) < st.MaxLoRA
}

// loraCandidatesLocked narrows free (with their loads) to the backends a
// request for adapter should choose between, by the order above.
func (p *Pool) loraCandidatesLocked(free []*Backend, adapter string, loads map[*Backend]float64, minLoad float64, now time.Time) []*Backend {
	var loaded, room []*Backend
	minLoaded := 2.0
	for _, b := range free {
		switch {
		case b.hasAdapterLocked(adapter, now) || b.loading[adapter] > 0:
			loaded = append(loaded, b)
			minLoaded = min(minLoaded, loads[b])
		case b.adapterRoomLocked(now):
			room = append(room, b)
		}
	}
	if len(loaded) > 0 && minLoaded <= minLoad+p.opts.AffinityMaxImbalance {
		return loaded
	}
	if len(loaded)+len(room) > 0 {
		return append(loaded, room...)
	}
	return free
}

// NoteAdapter records that b has adapter loaded: the router loaded it, or
// b just answered a request for it.
func (p *Pool) NoteAdapter(b *Backend, adapter string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.adapters == nil {
		b.adapters = map[string]time.Time{}
	}
	b.adapters[adapter] = time.Now()
}

// AdapterLoadDone ends a load of adapter on b that a lease with NeedsLoad
// started. Requests for the adapter that arrive during the load go to b as
// if it had the adapter, and wait for the same load, instead of each
// loading it on another backend. A successful load is noted separately
// (NoteAdapter).
func (p *Pool) AdapterLoadDone(b *Backend, adapter string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.loading[adapter]--; b.loading[adapter] <= 0 {
		delete(b.loading, adapter)
	}
}

// knownAdaptersLocked lists the adapters b is known to have loaded, of
// those configured (the router only knows which names are adapters).
func (b *Backend) knownAdaptersLocked(now time.Time) []string {
	var out []string
	for name := range b.adapters {
		if b.hasAdapterLocked(name, now) {
			out = append(out, name)
		}
	}
	if st, ok := b.engineStats(now); ok {
		for _, a := range st.Adapters {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	slices.Sort(out)
	return out
}
