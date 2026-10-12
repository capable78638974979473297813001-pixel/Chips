package pool

import (
	"math"
	"slices"
)

// Cold starts and demand signals for autoscaling.
//
// With Options.ColdStart, a request whose model no backend can serve right
// now, but some backend could once it is up (it is unhealthy, stalled or
// has a failed GPU), or whose model is in Options.ColdModels (scaled to
// zero, no backends at all), waits in the queue instead of failing with
// ErrNoBackend. The caller decides how long it waits. Drained backends
// don't count: draining is an operator's decision, not a boot.

// demandModel is the model a request counts toward for scaling: an
// adapter's base model, since that is what a deployment serves.
func demandModel(r Request) string {
	if r.Base != "" {
		return r.Base
	}
	return r.Model
}

// couldServeLocked reports whether some backend not excluded for r would
// serve it if it were up.
func (p *Pool) couldServeLocked(r Request) bool {
	for _, b := range p.backends {
		if !b.drained && !r.Exclude[b.Name] && (r.Only == nil || r.Only[b.Name]) && b.servesRequest(r) && b.fits(r) && (!r.GRPC || b.GRPCURL != nil) {
			return true
		}
	}
	return false
}

// coldLocked reports whether r should wait for a backend to start rather
// than fail, given that no backend can take it now.
func (p *Pool) coldLocked(r Request) bool {
	if !p.opts.ColdStart || !r.Hold {
		return false
	}
	return p.couldServeLocked(r) || p.opts.ColdModels[demandModel(r)]
}

// Cold reports whether r has no backend up to serve it but would be held
// for one (ignoring r.Hold): the moment to ask for a backend to be started.
func (p *Pool) Cold(r Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.opts.ColdStart || p.servableLocked(r) {
		return false
	}
	r.Hold = true
	return p.coldLocked(r)
}

// ModelDemand is the load on one model, for autoscalers.
type ModelDemand struct {
	Model    string `json:"model"`
	Inflight int    `json:"inflight"`
	// Queued waits for a slot; Held is the part of it waiting because no
	// backend for the model is up.
	Queued int `json:"queued"`
	Held   int `json:"held"`
	// ReadyBackends serve the model and are in rotation; Capacity is the
	// sum of their max_concurrency.
	ReadyBackends int `json:"ready_backends"`
	Capacity      int `json:"capacity"`
	// DesiredBackends is how many backends would keep the model's current
	// demand at the target utilization; 0 when there is no demand.
	DesiredBackends int `json:"desired_backends"`
}

// Demand reports every model the router knows: served by a backend that
// lists its models, in use, or in Options.ColdModels. targetUtilization
// and defaultCapacity (per backend, when no ready backend shows one)
// shape DesiredBackends.
func (p *Pool) Demand(targetUtilization float64, defaultCapacity int) []ModelDemand {
	p.mu.Lock()
	defer p.mu.Unlock()
	by := map[string]*ModelDemand{}
	get := func(m string) *ModelDemand {
		d := by[m]
		if d == nil {
			d = &ModelDemand{Model: m}
			by[m] = d
		}
		return d
	}
	for m := range p.opts.ColdModels {
		get(m)
	}
	for m, n := range p.inflightByModel {
		if n > 0 {
			get(m).Inflight = n
		}
	}
	for el := p.waiters.Front(); el != nil; el = el.Next() {
		w := el.Value.(*waiter)
		d := get(demandModel(w.req))
		d.Queued++
		if !p.servableLocked(w.req) {
			d.Held++
		}
	}
	all := map[string]int{} // backends listing each model, ready or not, and their capacity
	allCap := map[string]int{}
	for _, b := range p.backends {
		src := b.models
		if len(src) == 0 {
			src = b.discovered
		}
		for m := range src {
			d := get(m)
			all[m]++
			allCap[m] += b.MaxConcurrency
			if b.healthy && !b.drained && !b.hwFault && !b.stalled {
				d.ReadyBackends++
				d.Capacity += b.MaxConcurrency
			}
		}
	}
	out := make([]ModelDemand, 0, len(by))
	for m, d := range by {
		if m == "" {
			continue // requests that name no model
		}
		per := float64(defaultCapacity)
		switch {
		case d.ReadyBackends > 0:
			per = float64(d.Capacity) / float64(d.ReadyBackends)
		case all[m] > 0:
			per = float64(allCap[m]) / float64(all[m])
		}
		if want := float64(d.Inflight + d.Queued); want > 0 {
			d.DesiredBackends = int(math.Ceil(want / (per * targetUtilization)))
		}
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b ModelDemand) int {
		if a.Model < b.Model {
			return -1
		}
		return 1
	})
	return out
}
