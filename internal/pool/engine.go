package pool

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/engine"
)

// EngineScraper reads each backend's own load metrics (see
// config.Backend.MetricsPath) on a schedule.
type EngineScraper struct {
	Pool     *Pool
	Client   *http.Client
	Interval time.Duration
	Timeout  time.Duration
}

// Run scrapes immediately, then on every tick until ctx is done. Backends
// without a metrics path are skipped, so it costs nothing when none has one.
func (e *EngineScraper) Run(ctx context.Context) {
	e.ScrapeAll(ctx)
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.ScrapeAll(ctx)
		}
	}
}

// ScrapeAll scrapes every backend with a metrics path in parallel and
// waits for the results.
func (e *EngineScraper) ScrapeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range e.Pool.Backends() {
		if b.MetricsURL == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, e.Timeout)
			defer cancel()
			st, err := engine.Fetch(ctx, e.Client, b.MetricsURL, b.SetAuth)
			e.Pool.SetEngine(b, st, err)
		}()
	}
	wg.Wait()
}

// SetEngine records a scrape of b's engine metrics. A failed scrape keeps
// the last stats until they go stale.
func (p *Pool) SetEngine(b *Backend, st engine.Stats, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if b.engErr == "" {
			p.log.Warn("engine metrics unavailable", "backend", b.Name, "error", err)
		}
		b.engErr = err.Error()
		return
	}
	if b.engErr != "" {
		p.log.Info("engine metrics available", "backend", b.Name, "engine", st.Engine)
	}
	b.eng, b.engErr = &st, ""
	b.engUntil = time.Now().Add(p.opts.EngineMaxAge)
}

// engineStats returns b's engine stats if they are fresh enough to act on.
func (b *Backend) engineStats(now time.Time) (engine.Stats, bool) {
	if b.eng == nil || now.After(b.engUntil) {
		return engine.Stats{}, false
	}
	return *b.eng, true
}

// saturated reports whether b's engine is queueing requests internally or
// its KV cache is nearly full: more work would wait inside the engine, or
// push out other requests' cache, instead of being served.
func (p *Pool) saturated(b *Backend, now time.Time) bool {
	st, ok := b.engineStats(now)
	return ok && (st.Waiting > 0 || st.KVUsage >= p.opts.KVSaturated)
}

// load is how busy b is, 0 for idle and 1 for full: the router's own share
// of its slots, or what its engine reports, whichever is higher. The
// engine sees load the router cannot (other routers, clients that call it
// directly); the router sees its own requests sooner than the next scrape.
func (b *Backend) load(now time.Time) float64 {
	l := b.utilization()
	if st, ok := b.engineStats(now); ok {
		reqs := st.Waiting
		if st.Running > 0 {
			reqs += st.Running
		}
		l = max(l, reqs/float64(b.MaxConcurrency), st.KVUsage)
	}
	return l
}

// EngineStatus is the last engine scrape of a backend.
type EngineStatus struct {
	Engine    string   `json:"engine,omitempty"`
	Running   *float64 `json:"running,omitempty"`
	Waiting   float64  `json:"waiting"`
	KVUsage   *float64 `json:"kv_cache_usage,omitempty"`
	Saturated bool     `json:"saturated"`
	Stale     bool     `json:"stale,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func (p *Pool) engineStatusLocked(b *Backend, now time.Time) *EngineStatus {
	if b.MetricsURL == nil {
		return nil
	}
	es := &EngineStatus{Error: b.engErr}
	if b.eng == nil {
		if es.Error == "" {
			es.Error = "not scraped yet"
		}
		return es
	}
	st := *b.eng
	es.Engine, es.Waiting = st.Engine, st.Waiting
	if st.Running >= 0 {
		es.Running = &st.Running
	}
	if st.KVUsage >= 0 {
		es.KVUsage = &st.KVUsage
	}
	_, fresh := b.engineStats(now)
	es.Stale = !fresh
	es.Saturated = p.saturated(b, now)
	return es
}
