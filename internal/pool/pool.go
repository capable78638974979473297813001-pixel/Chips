// Package pool tracks a fleet of inference backends and hands out
// concurrency slots on them. When every suitable backend is full, callers
// wait in a single FIFO line instead of piling onto an overloaded GPU.
package pool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/engine"
	"github.com/capable78638974979473297813001-pixel/chips/internal/hardware"
)

var (
	// ErrQueueFull means too many requests are already waiting.
	ErrQueueFull = errors.New("request queue is full")
	// ErrShed means the request was waiting in a full queue and gave its
	// place to a higher-priority request.
	ErrShed = errors.New("request displaced from the queue by higher-priority traffic")
	// ErrNoBackend means no healthy backend can serve the request at all
	// (wrong model, prompt too long, or everything is down), so waiting is pointless.
	ErrNoBackend = errors.New("no healthy backend can serve this request")
)

// Backend is one inference server. Mutable fields are guarded by Pool.mu.
type Backend struct {
	Name string
	URL  *url.URL
	// Tier is guarded by Pool.mu when AutoTier is set, since chipagent
	// reports can change it.
	Tier             string
	MaxConcurrency   int
	MaxContextTokens int
	APIKey           string
	APIKeyHeader     string
	HealthPath       string
	Tokenize         string   // engine tokenize API: vllm, llamacpp, tgi or ""
	GRPCURL          *url.URL // gRPC endpoint, or nil
	// AgentURL is the chipagent on this backend's machine, or nil.
	AgentURL *url.URL
	// MetricsURL is the engine's Prometheus endpoint, or nil.
	MetricsURL  *url.URL
	metricsPath string
	// AutoTier means Tier comes from the hardware chipagent reports.
	AutoTier bool
	index    int
	models   map[string]bool

	inflight   int
	drained    bool // out of rotation for maintenance; in-flight requests finish
	checked    bool
	healthy    bool
	okStreak   int
	failStreak int
	discovered map[string]bool
	// discoveredAt is when discovered was last refreshed.
	discoveredAt time.Time
	// dynamicLoRA: the engine loads adapters at runtime. adapters records
	// when this router last loaded or used each adapter here (see lora.go).
	dynamicLoRA bool
	adapters    map[string]time.Time
	lastErr     string
	hw          *hardware.Report
	hwErr       string
	hwFault     bool // chipagent reports a failed accelerator: no new work
	// ttft is a moving average of time to first token on streaming
	// requests, in seconds (0 = no samples yet).
	ttft float64
	// The engine's own load, from its metrics (see EngineScraper).
	eng      *engine.Stats
	engUntil time.Time // eng is stale after this
	engErr   string
	// Recent generated-token counts while requests ran, for stall detection.
	progress []progressSample
	stalled  bool
	// Cost accounting (see cost.go).
	price     pricing
	costClock float64 // dollars charged so far to one request running throughout
	idleCost  float64 // dollars of running cost with nothing running
	costAt    time.Time
}

// Request is what the scheduler needs to know to place a request.
type Request struct {
	Model        string
	PromptTokens int
	MaxTokens    int
	// Preferred names the backend believed to hold this prompt's prefix in cache.
	Preferred string
	// Exclude lists backends that already failed this request.
	Exclude map[string]bool
	// GRPC requires a backend with a gRPC endpoint.
	GRPC bool
	// Priority orders the wait queue: higher ranks are served first
	// (see config.PriorityRank). Requests of equal priority stay FIFO.
	Priority int
	// Base is set when Model is a LoRA adapter: the model it adapts.
	Base string
	// Hold lets the request wait for a backend to start when none is up
	// (see Options.ColdStart) instead of failing with ErrNoBackend.
	Hold bool
	// Only, when non-nil, restricts the request to these backends (a
	// gateway's subset hint).
	Only map[string]bool
}

// Lease is a held concurrency slot. Release must be called exactly once
// when the upstream response is finished; extra calls are ignored.
type Lease struct {
	Backend *Backend
	// Affinity is true when the request went to its Preferred backend.
	Affinity bool
	// Waited is how long the request queued for the slot.
	Waited time.Duration
	// For a request naming a LoRA adapter: AdapterLoaded says the backend
	// was known to have it, and NeedsLoad that it must be loaded first
	// (the backend has dynamic_lora and isn't known to have it).
	AdapterLoaded bool
	NeedsLoad     bool
	pool          *Pool
	once          sync.Once
	// Cost accounting: the backend's cost clock at grant, its prices and
	// cost-report name then, and the time cost charged at release.
	model     string // demandModel of the request, for per-model in-flight counts
	costStart float64
	price     pricing
	hardware  string
	timeCost  float64
}

func (l *Lease) Release() {
	l.once.Do(func() {
		p := l.pool
		p.mu.Lock()
		l.Backend.advanceCostLocked(p.now())
		l.timeCost = l.Backend.costClock - l.costStart
		l.Backend.inflight--
		p.inflightByModel[l.model]--
		p.dispatchLocked(false)
		p.mu.Unlock()
	})
}

type Options struct {
	MaxWaiting           int
	AffinityMaxImbalance float64
	LargeRequestTokens   int
	UnhealthyThreshold   int
	HealthyThreshold     int
	// PassiveHealth lets request failures mark a backend down. It should only
	// be on when active checks run, otherwise a backend could never recover.
	PassiveHealth bool
	// Tiers classifies `tier: auto` backends; zero means hardware.DefaultThresholds.
	Tiers hardware.Thresholds
	// EngineMaxAge is how long an engine metrics scrape stays usable, and
	// KVSaturated the KV-cache usage at which an engine counts as full.
	EngineMaxAge time.Duration
	KVSaturated  float64
	// StallAfter and StallMinRate configure stall detection (see
	// config.EngineMetrics); StallAfter <= 0 turns it off.
	StallAfter   time.Duration
	StallMinRate float64
	// ColdStart holds Hold requests for a model with no backend up, when a
	// backend could serve it once up or the model is in ColdModels.
	ColdStart  bool
	ColdModels map[string]bool
}

type Pool struct {
	mu       sync.Mutex
	backends []*Backend
	byName   map[string]*Backend
	waiters  *list.List
	opts     Options
	log      *slog.Logger
	now      func() time.Time // the cost clock's time source
	// inflightByModel counts leases by demandModel, for autoscaling.
	inflightByModel map[string]int
}

type waiter struct {
	req   Request
	start time.Time
	el    *list.Element
	ch    chan result
}

type result struct {
	lease *Lease
	err   error
}

// OptionsFromConfig derives pool options from a router configuration.
func (o Options) withDefaults() Options {
	if o.Tiers == (hardware.Thresholds{}) {
		o.Tiers = hardware.DefaultThresholds
	}
	if o.EngineMaxAge == 0 {
		o.EngineMaxAge = 10 * time.Second
	}
	if o.KVSaturated == 0 {
		o.KVSaturated = 0.95
	}
	return o
}

func OptionsFromConfig(cfg *config.Config) Options {
	return Options{
		MaxWaiting:           cfg.Queue.MaxWaiting,
		AffinityMaxImbalance: cfg.Routing.AffinityMaxImbalance,
		LargeRequestTokens:   cfg.Routing.LargeRequestTokens,
		UnhealthyThreshold:   cfg.Health.UnhealthyThreshold,
		HealthyThreshold:     cfg.Health.HealthyThreshold,
		PassiveHealth:        !cfg.Health.Disabled,
		Tiers: hardware.Thresholds{
			HeavyMinBytes:    uint64(cfg.Hardware.HeavyMinMemoryGiB * (1 << 30)),
			StandardMinBytes: uint64(cfg.Hardware.StandardMinMemoryGiB * (1 << 30)),
		},
		// Three missed scrapes and the stats no longer steer routing.
		EngineMaxAge: 3*cfg.EngineMetrics.Interval.D() + cfg.EngineMetrics.Timeout.D(),
		KVSaturated:  cfg.EngineMetrics.KVSaturated,
		StallAfter:   cfg.EngineMetrics.StallAfter.D(),
		StallMinRate: cfg.EngineMetrics.StallMinTokensPerSecond,
		ColdStart:    cfg.Autoscaling.ColdStartWait > 0,
		ColdModels:   coldModels(cfg.Autoscaling.ScaleToZeroModels),
	}
}

func coldModels(models []string) map[string]bool {
	out := map[string]bool{}
	for _, m := range models {
		out[m] = true
	}
	return out
}

func New(backends []config.Backend, opts Options, log *slog.Logger) (*Pool, error) {
	opts = opts.withDefaults()
	p := &Pool{byName: map[string]*Backend{}, waiters: list.New(), opts: opts, log: log, now: time.Now, inflightByModel: map[string]int{}}
	for i, cb := range backends {
		b, err := newBackend(cb, i)
		if err != nil {
			return nil, err
		}
		p.backends = append(p.backends, b)
		p.byName[b.Name] = b
	}
	return p, nil
}

func newBackend(cb config.Backend, index int) (*Backend, error) {
	u, err := url.Parse(cb.URL)
	if err != nil {
		return nil, err
	}
	var agent *url.URL
	if cb.AgentURL != "" {
		if agent, err = url.Parse(cb.AgentURL); err != nil {
			return nil, err
		}
	}
	var grpcURL *url.URL
	if cb.GRPCURL != "" {
		if grpcURL, err = url.Parse(cb.GRPCURL); err != nil {
			return nil, err
		}
	}
	var metricsURL *url.URL
	if cb.MetricsPath != "" { // like health checks: the path goes under the backend URL's
		m := *u
		m.Path = strings.TrimSuffix(m.Path, "/") + cb.MetricsPath
		m.RawPath, m.RawQuery = "", ""
		metricsURL = &m
	}
	b := &Backend{
		GRPCURL:      grpcURL,
		Name:         cb.Name,
		URL:          u,
		APIKey:       cb.APIKey,
		APIKeyHeader: cb.APIKeyHeader,
		HealthPath:   cb.HealthPath,
		Tokenize:     cb.Tokenize,
		AgentURL:     agent,
		MetricsURL:   metricsURL,
		metricsPath:  cb.MetricsPath,
		index:        index,
		healthy:      true, // optimistic until the first check says otherwise
	}
	b.setCapacity(cb)
	return b, nil
}

// setCapacity applies the settings that may change in place (under Pool.mu
// once the backend is in a pool).
func (b *Backend) setCapacity(cb config.Backend) {
	b.setPricingLocked(pricingOf(cb), time.Now())
	b.MaxConcurrency = cb.MaxConcurrency
	b.MaxContextTokens = cb.MaxContextTokens
	b.dynamicLoRA = cb.DynamicLoRA
	b.models = map[string]bool{}
	for _, m := range cb.Models {
		b.models[m] = true
	}
	switch {
	case cb.Tier != config.TierAuto:
		b.Tier, b.AutoTier = cb.Tier, false
	case !b.AutoTier:
		// Until chipagent answers, treat the node as middle of the road.
		b.AutoTier, b.Tier = true, config.TierStandard
	}
}

// sameConnection reports whether b already talks to cb's endpoint with cb's
// credentials. These fields are read without the pool lock, so a change to
// any of them replaces the Backend instead of editing it.
func (b *Backend) sameConnection(cb config.Backend) bool {
	agent := ""
	if b.AgentURL != nil {
		agent = b.AgentURL.String()
	}
	grpcURL := ""
	if b.GRPCURL != nil {
		grpcURL = b.GRPCURL.String()
	}
	return b.URL.String() == cb.URL && grpcURL == cb.GRPCURL && b.APIKey == cb.APIKey && b.APIKeyHeader == cb.APIKeyHeader &&
		b.HealthPath == cb.HealthPath && agent == cb.AgentURL && b.Tokenize == cb.Tokenize && b.metricsPath == cb.MetricsPath
}

// Update applies a new backend list and options without dropping traffic.
// New backends join, removed ones stop receiving requests (in-flight ones
// finish), and changed ones are updated, keeping their health, load and
// hardware state unless their endpoint or credentials changed. It returns a
// description of each change.
func (p *Pool) Update(backends []config.Backend, opts Options) ([]string, error) {
	opts = opts.withDefaults()
	p.mu.Lock()
	defer p.mu.Unlock()
	var changes []string
	next := make([]*Backend, 0, len(backends))
	byName := make(map[string]*Backend, len(backends))
	for i, cb := range backends {
		old := p.byName[cb.Name]
		switch {
		case old == nil:
			b, err := newBackend(cb, i)
			if err != nil {
				return nil, err
			}
			next = append(next, b)
			changes = append(changes, "added backend "+cb.Name)
		case !old.sameConnection(cb):
			b, err := newBackend(cb, i)
			if err != nil {
				return nil, err
			}
			b.drained = old.drained // a drain is an operator decision; a config edit keeps it
			next = append(next, b)
			changes = append(changes, "replaced backend "+cb.Name+" (endpoint or credentials changed)")
		default:
			before := fmt.Sprint(old.Tier, old.AutoTier, old.MaxConcurrency, old.MaxContextTokens, sortedKeys(old.models), old.price, old.dynamicLoRA)
			old.setCapacity(cb)
			old.index = i
			if fmt.Sprint(old.Tier, old.AutoTier, old.MaxConcurrency, old.MaxContextTokens, sortedKeys(old.models), old.price, old.dynamicLoRA) != before {
				changes = append(changes, "updated backend "+cb.Name)
			}
			next = append(next, old)
		}
		byName[cb.Name] = next[len(next)-1]
	}
	for _, b := range p.backends {
		if byName[b.Name] == nil {
			changes = append(changes, "removed backend "+b.Name)
		}
	}
	// Re-tier auto backends from their last hardware report, under the
	// (possibly changed) thresholds.
	for _, b := range next {
		if b.AutoTier && b.hw != nil {
			b.Tier = opts.Tiers.Tier(b.hw)
		}
	}
	p.backends, p.byName, p.opts = next, byName, opts
	// Capacity or membership changed: hand out new slots, and fail waiters
	// that no remaining backend can serve.
	p.dispatchLocked(true)
	return changes, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Backends returns the current backends in config order.
func (p *Pool) Backends() []*Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.backends)
}

// Acquire returns a slot on the best backend for req, waiting in line if
// every suitable backend is at capacity. It gives up when ctx is done.
func (p *Pool) Acquire(ctx context.Context, req Request) (*Lease, error) {
	start := time.Now()
	p.mu.Lock()
	if p.waiters.Len() == 0 {
		b, aff, err := p.pick(req)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if b != nil {
			l := p.grantLocked(b, req, aff, start)
			p.mu.Unlock()
			return l, nil
		}
	} else if !p.servableLocked(req) && !p.coldLocked(req) {
		p.mu.Unlock()
		return nil, ErrNoBackend
	}
	if p.waiters.Len() >= p.opts.MaxWaiting && !p.shedLocked(req.Priority) {
		p.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &waiter{req: req, start: start, ch: make(chan result, 1)}
	w.el = p.enqueueLocked(w)
	p.dispatchLocked(false)
	p.mu.Unlock()

	select {
	case r := <-w.ch:
		return r.lease, r.err
	case <-ctx.Done():
		p.mu.Lock()
		if w.el != nil {
			p.waiters.Remove(w.el)
			w.el = nil
			p.mu.Unlock()
			return nil, ctx.Err()
		}
		p.mu.Unlock()
		// A slot was granted while we were giving up; hand it back.
		if r := <-w.ch; r.lease != nil {
			r.lease.Release()
		}
		return nil, ctx.Err()
	}
}

// shedLocked makes room in a full queue for a request of priority prio by
// dropping the newest waiter of the lowest priority, if that is lower than
// prio. Under overload, interactive traffic displaces batch work instead of
// being turned away. It reports whether a place was freed.
func (p *Pool) shedLocked(prio int) bool {
	back := p.waiters.Back() // the queue is ordered by priority, so this is the lowest, newest
	if back == nil {
		return false
	}
	w := back.Value.(*waiter)
	if w.req.Priority >= prio {
		return false
	}
	p.waiters.Remove(back)
	w.el = nil
	w.ch <- result{err: ErrShed}
	return true
}

// SetDrained takes a backend out of rotation (or puts it back). New requests
// go elsewhere while its in-flight requests finish; queued requests that only
// it could serve fail rather than wait for it.
func (p *Pool) SetDrained(name string, drained bool) (Status, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.byName[name]
	if !ok {
		return Status{}, false
	}
	if b.drained != drained {
		b.drained = drained
		if drained {
			p.log.Info("backend drained", "backend", name, "inflight", b.inflight)
		} else {
			p.log.Info("backend back in rotation", "backend", name)
		}
		p.dispatchLocked(true)
	}
	return p.statusLocked(b), true
}

// enqueueLocked inserts w behind every waiter of equal or higher priority,
// so priorities jump the line but each priority stays first come, first served.
func (p *Pool) enqueueLocked(w *waiter) *list.Element {
	for el := p.waiters.Back(); el != nil; el = el.Prev() {
		if el.Value.(*waiter).req.Priority >= w.req.Priority {
			return p.waiters.InsertAfter(w, el)
		}
	}
	return p.waiters.PushFront(w)
}

// QueueDepth reports how many requests are waiting for a slot.
func (p *Pool) QueueDepth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiters.Len()
}

func (p *Pool) grantLocked(b *Backend, r Request, affinity bool, start time.Time) *Lease {
	now := p.now()
	b.advanceCostLocked(now)
	b.inflight++
	p.inflightByModel[demandModel(r)]++
	l := &Lease{Backend: b, Affinity: affinity, Waited: time.Since(start), pool: p, model: demandModel(r),
		costStart: b.costClock, price: b.price, hardware: b.hardwareNameLocked()}
	if r.Base != "" {
		l.AdapterLoaded = b.hasAdapterLocked(r.Model, now)
		l.NeedsLoad = !l.AdapterLoaded && b.dynamicLoRA
	}
	return l
}

// dispatchLocked hands freed slots to waiters in arrival order. A waiter that
// cannot use any free backend is skipped, so one request needing a big GPU
// does not hold up small requests that fit elsewhere. With checkAll, waiters
// are also re-checked for backends having become unservable.
func (p *Pool) dispatchLocked(checkAll bool) {
	for el := p.waiters.Front(); el != nil; {
		next := el.Next()
		if !checkAll && !p.anyFreeLocked() {
			return
		}
		w := el.Value.(*waiter)
		b, aff, err := p.pick(w.req)
		switch {
		case err != nil:
			p.waiters.Remove(el)
			w.el = nil
			w.ch <- result{err: err}
		case b != nil:
			p.waiters.Remove(el)
			w.el = nil
			w.ch <- result{lease: p.grantLocked(b, w.req, aff, w.start)}
		}
		el = next
	}
}

func (p *Pool) anyFreeLocked() bool {
	for _, b := range p.backends {
		if b.healthy && !b.drained && b.inflight < b.MaxConcurrency {
			return true
		}
	}
	return false
}

// NearContextLimit reports whether r's estimated size is within margin (a
// fraction) of the context limit of any backend that could serve it, and
// returns a healthy backend for r's model that can count tokens exactly.
// Routing on an estimate is fine far from every limit; near one, a wrong
// guess means the backend rejects the prompt or a bigger one goes unused.
func (p *Pool) NearContextLimit(r Request, margin float64) (counter *Backend, near bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	need := float64(r.PromptTokens + r.MaxTokens)
	for _, b := range p.backends {
		if !b.healthy || b.drained || r.Exclude[b.Name] || !b.servesRequest(r) {
			continue
		}
		if b.Tokenize != "" && counter == nil {
			counter = b
		}
		if limit := float64(b.MaxContextTokens); limit > 0 && need >= limit*(1-margin) && need <= limit*(1+margin) {
			near = true
		}
	}
	return counter, near
}

// SetAuth adds this backend's credentials to an outgoing request.
func (b *Backend) SetAuth(h http.Header) {
	switch {
	case b.APIKey == "":
	case b.APIKeyHeader == "" || strings.EqualFold(b.APIKeyHeader, "Authorization"):
		h.Set("Authorization", "Bearer "+b.APIKey)
	default:
		h.Set(b.APIKeyHeader, b.APIKey)
	}
}

func (b *Backend) serves(model string) bool {
	if model == "" {
		return true
	}
	if len(b.models) > 0 {
		return b.models[model]
	}
	if len(b.discovered) > 0 {
		return b.discovered[model]
	}
	return true // unknown model list: let the backend decide
}

func (b *Backend) fits(r Request) bool {
	return b.MaxContextTokens == 0 || r.PromptTokens+r.MaxTokens <= b.MaxContextTokens
}

// gpuBusy is the latest accelerator busy percentage from chipagent, if fresh.
func (b *Backend) gpuBusy() (float64, bool) {
	if b.hw == nil || b.hwErr != "" {
		return 0, false
	}
	return b.hw.Utilization()
}

func (b *Backend) utilization() float64 {
	return float64(b.inflight) / float64(b.MaxConcurrency)
}

func (p *Pool) eligible(b *Backend, r Request) bool {
	return b.healthy && !b.drained && !b.hwFault && !b.stalledNow(time.Now()) && !r.Exclude[b.Name] && (r.Only == nil || r.Only[b.Name]) && b.servesRequest(r) && b.fits(r) && (!r.GRPC || b.GRPCURL != nil)
}

func (p *Pool) servableLocked(r Request) bool {
	for _, b := range p.backends {
		if p.eligible(b, r) {
			return true
		}
	}
	return false
}

var (
	largeOrder = []string{config.TierHeavy, config.TierStandard, config.TierLight, config.TierReserve}
	smallOrder = []string{config.TierLight, config.TierStandard, config.TierHeavy, config.TierReserve}
)

// pick chooses a backend with a free slot. It returns (nil, false, nil) when
// suitable backends exist but are all full, and ErrNoBackend when none exist.
//
// Order of preference:
//  1. The backend holding the prompt's prefix cache, unless it is noticeably
//     busier than the least-loaded alternative.
//  2. The right class of hardware for the request size: heavy nodes for big
//     prompts, light nodes for small ones, reserve nodes only as a last resort.
//  3. Within a class, the least utilised backend.
func (p *Pool) pick(r Request) (*Backend, bool, error) {
	var free []*Backend
	servable := false
	for _, b := range p.backends {
		if !p.eligible(b, r) {
			continue
		}
		servable = true
		if b.inflight < b.MaxConcurrency {
			free = append(free, b)
		}
	}
	if !servable {
		if p.coldLocked(r) {
			return nil, false, nil // wait for a backend to come up
		}
		return nil, false, ErrNoBackend
	}
	if len(free) == 0 {
		return nil, false, nil
	}

	// Engines that report queueing or a nearly full KV cache come last:
	// they get new work only when nothing else has room.
	now := time.Now()
	var ready, saturated []*Backend
	for _, b := range free {
		if p.saturated(b, now) {
			saturated = append(saturated, b)
		} else {
			ready = append(ready, b)
		}
	}
	if len(ready) > 0 {
		free = ready
	} else {
		free = saturated
	}
	loads := make(map[*Backend]float64, len(free))
	minLoad := math.Inf(1)
	for _, b := range free {
		loads[b] = b.load(now)
		minLoad = min(minLoad, loads[b])
	}
	if r.Preferred != "" {
		for _, b := range free {
			if b.Name == r.Preferred && loads[b] <= minLoad+p.opts.AffinityMaxImbalance {
				return b, true, nil
			}
		}
	}
	if r.Base != "" {
		free = p.loraCandidatesLocked(free, r.Model, loads, minLoad, now)
	}

	order := smallOrder
	if r.PromptTokens >= p.opts.LargeRequestTokens {
		order = largeOrder
	}
	for _, tier := range order {
		var best *Backend
		for _, b := range free {
			if b.Tier == tier && (best == nil || better(b, best, loads)) {
				best = b
			}
		}
		if best != nil {
			return best, false, nil
		}
	}
	return nil, false, nil // unreachable: every free backend has a known tier
}

// gpuBusyMargin is how many points apart two nodes' accelerator busy
// percentages must be before it decides between them; smaller gaps are noise.
const gpuBusyMargin = 10

// latencyMargin is how much faster (as a fraction) one backend's recent time
// to first token must be before it wins a tie on load.
const latencyMargin = 0.3

func better(a, b *Backend, loads map[*Backend]float64) bool {
	if la, lb := loads[a], loads[b]; la != lb {
		return la < lb
	}
	// Equal load: prefer the backend that has been answering faster. This
	// steers around a node that is thermally throttled, on a shared GPU, or
	// otherwise degraded, while small differences are left to the rules below.
	if a.ttft > 0 && b.ttft > 0 {
		if a.ttft < b.ttft*(1-latencyMargin) {
			return true
		}
		if b.ttft < a.ttft*(1-latencyMargin) {
			return false
		}
	}
	// Equal router load: prefer the node whose accelerators are less busy.
	// This catches load the router cannot see, such as a training job or
	// another service sharing the GPUs.
	if ga, okA := a.gpuBusy(); okA {
		if gb, okB := b.gpuBusy(); okB && math.Abs(ga-gb) >= gpuBusyMargin {
			return ga < gb
		}
	}
	if fa, fb := a.MaxConcurrency-a.inflight, b.MaxConcurrency-b.inflight; fa != fb {
		return fa > fb
	}
	return a.index < b.index
}

// Observe records a health signal for b: from an active check or, when
// passive is true, from a real request. Consecutive failures past the
// threshold take it out of rotation; consecutive successes bring it back.
func (p *Pool) Observe(b *Backend, ok, passive bool, errMsg string) {
	if passive && !p.opts.PassiveHealth {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	was := b.healthy
	if !ok {
		b.lastErr = errMsg
	}
	switch {
	case !b.checked && !passive:
		b.checked = true
		b.healthy = ok
	case ok:
		b.failStreak = 0
		b.okStreak++
		if !b.healthy && b.okStreak >= p.opts.HealthyThreshold {
			b.healthy = true
		}
	default:
		b.okStreak = 0
		b.failStreak++
		if b.healthy && b.failStreak >= p.opts.UnhealthyThreshold {
			b.healthy = false
		}
	}
	if b.healthy != was {
		if b.healthy {
			p.log.Info("backend is healthy", "backend", b.Name)
		} else {
			p.log.Warn("backend is unhealthy", "backend", b.Name, "error", b.lastErr)
		}
		p.dispatchLocked(true)
	}
}

// ttftWeight is the weight of a new sample in the time-to-first-token
// average; about the last ten streaming requests dominate it.
const ttftWeight = 0.2

// ObserveTTFT records how long b took to produce the first bytes of a
// streaming response.
func (p *Pool) ObserveTTFT(b *Backend, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.ttft == 0 {
		b.ttft = d.Seconds()
	} else {
		b.ttft += ttftWeight * (d.Seconds() - b.ttft)
	}
}

// SetDiscoveredModels records the models a backend reported serving.
func (p *Pool) SetDiscoveredModels(b *Backend, models []string) {
	set := make(map[string]bool, len(models))
	for _, m := range models {
		set[m] = true
	}
	p.mu.Lock()
	b.discovered, b.discoveredAt = set, time.Now()
	p.mu.Unlock()
}

// SetHardware records a chipagent poll. A successful report re-tiers an
// auto-tiered backend. A failed poll keeps the last inventory and tier but
// stops using its stale utilization for routing.
func (p *Pool) SetHardware(b *Backend, r *hardware.Report, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if b.hwErr == "" {
			p.log.Warn("chipagent unreachable", "backend", b.Name, "error", err)
		}
		b.hwErr = err.Error()
		return
	}
	if r.Stale && len(r.Devices) == 0 {
		b.hwErr = "chipagent has no readings yet: " + r.CollectError
		return // keep what we had
	}
	b.hw, b.hwErr = r, ""
	if r.Stale {
		b.hwErr = "stale readings: " + r.CollectError
	}
	if faults := r.Faults(); (len(faults) > 0) != b.hwFault {
		b.hwFault = len(faults) > 0
		if b.hwFault {
			p.log.Warn("chipagent reports a failed accelerator; out of rotation until it recovers",
				"backend", b.Name, "device", faults[0].Name, "error", faults[0].Error, "failed", len(faults))
		} else {
			p.log.Info("accelerators healthy again; back in rotation", "backend", b.Name)
		}
		p.dispatchLocked(true)
	}
	if !b.AutoTier {
		return
	}
	if tier := p.opts.Tiers.Tier(r); tier != b.Tier {
		p.log.Info("backend tier set from hardware", "backend", b.Name, "tier", tier, "was", b.Tier,
			"vendors", r.Vendors(), "accelerators", len(r.Accelerators()), "memory_gib", r.MemoryTotal()>>30)
		b.Tier = tier
	}
}

// HardwareStatus summarizes what chipagent reported for a backend.
type HardwareStatus struct {
	Hostname           string            `json:"hostname,omitempty"`
	Host               *hardware.Host    `json:"host,omitempty"`
	Vendors            []string          `json:"vendors"`
	Accelerators       int               `json:"accelerators"`
	MemoryTotalBytes   uint64            `json:"memory_total_bytes"`
	MemoryUsedBytes    uint64            `json:"memory_used_bytes"`
	UtilizationPercent *float64          `json:"utilization_percent,omitempty"`
	Devices            []hardware.Device `json:"devices"`
	AgentError         string            `json:"agent_error,omitempty"`
	// Faults lists failing accelerators; while any are, the backend gets no new work.
	Faults []string `json:"faults,omitempty"`
}

func (b *Backend) hardwareStatus() *HardwareStatus {
	if b.AgentURL == nil {
		return nil
	}
	hs := &HardwareStatus{Vendors: []string{}, Devices: []hardware.Device{}, AgentError: b.hwErr}
	if b.hw == nil {
		if hs.AgentError == "" {
			hs.AgentError = "not polled yet"
		}
		return hs
	}
	hs.Hostname = b.hw.Hostname
	hs.Host = b.hw.Host
	if v := b.hw.Vendors(); v != nil {
		hs.Vendors = v
	}
	hs.Accelerators = len(b.hw.Accelerators())
	hs.MemoryTotalBytes = b.hw.MemoryTotal()
	hs.MemoryUsedBytes = b.hw.MemoryUsed()
	if u, ok := b.gpuBusy(); ok {
		hs.UtilizationPercent = &u
	}
	hs.Devices = append(hs.Devices, b.hw.Devices...)
	for _, d := range b.hw.Faults() {
		hs.Faults = append(hs.Faults, d.Name+": "+d.Error)
	}
	return hs
}

// Status is a point-in-time view of one backend.
type Status struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Tier string `json:"tier"`
	// TierSource is "auto" when chipagent's report chose the tier.
	TierSource       string   `json:"tier_source"`
	Healthy          bool     `json:"healthy"`
	Drained          bool     `json:"drained,omitempty"`
	Inflight         int      `json:"inflight"`
	MaxConcurrency   int      `json:"max_concurrency"`
	MaxContextTokens int      `json:"max_context_tokens,omitempty"`
	Models           []string `json:"models"`
	LastError        string   `json:"last_error,omitempty"`
	// TTFTMillis is the recent average time to first token on streaming
	// requests (0 until one has been served).
	TTFTMillis float64         `json:"ttft_ms"`
	Hardware   *HardwareStatus `json:"hardware,omitempty"`
	Engine     *EngineStatus   `json:"engine,omitempty"`
	// LoRAAdapters are adapters known to be loaded here (see lora.go).
	LoRAAdapters []string `json:"lora_adapters,omitempty"`
	DynamicLoRA  bool     `json:"dynamic_lora,omitempty"`
}

func (p *Pool) Snapshot() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Status, 0, len(p.backends))
	for _, b := range p.backends {
		out = append(out, p.statusLocked(b))
	}
	return out
}

func (p *Pool) statusLocked(b *Backend) Status {
	src := b.models
	if len(src) == 0 {
		src = b.discovered
	}
	models := make([]string, 0, len(src))
	for m := range src {
		models = append(models, m)
	}
	slices.Sort(models)
	source := "config"
	if b.AutoTier {
		source = "auto"
	}
	return Status{
		Name: b.Name, URL: b.URL.Redacted(), Tier: b.Tier, TierSource: source, Healthy: b.healthy,
		Drained:  b.drained,
		Inflight: b.inflight, MaxConcurrency: b.MaxConcurrency,
		MaxContextTokens: b.MaxContextTokens, Models: models, LastError: b.lastErr,
		TTFTMillis: math.Round(b.ttft * 1000),
		Hardware:   b.hardwareStatus(),
		Engine:     p.engineStatusLocked(b, time.Now()),

		LoRAAdapters: b.knownAdaptersLocked(time.Now()),
		DynamicLoRA:  b.dynamicLoRA,
	}
}
