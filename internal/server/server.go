// Package server is SiliconRouter's HTTP front end: an OpenAI-compatible
// reverse proxy that places each request on the best backend in the pool.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/affinity"
	"github.com/capable78638974979473297813001-pixel/chips/internal/cache"
	"github.com/capable78638974979473297813001-pixel/chips/internal/cluster"
	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/limits"
	"github.com/capable78638974979473297813001-pixel/chips/internal/metrics"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
)

// SessionHeader lets clients pin a conversation to one backend explicitly,
// which helps when they trim old turns and the prompt prefix changes.
const SessionHeader = "X-Session-ID"

// PriorityHeader sets a request's queue priority (high, normal, low) when
// queue.priority_header is enabled.
const PriorityHeader = "X-SiliconRouter-Priority"

type Server struct {
	// state holds everything a config reload can change; requests load it
	// once and see one consistent version.
	state    atomic.Pointer[state]
	applyMu  sync.Mutex // serializes Apply
	reloader atomic.Pointer[func() ([]string, error)]
	pool     *pool.Pool
	affinity *affinity.Table
	limits   *limits.Tracker
	cluster  *cluster.Node // nil when this router runs alone
	client   *http.Client
	log      *slog.Logger
	reg      *metrics.Registry
	mux      *http.ServeMux

	requests      *metrics.CounterVec
	upstreamErrs  *metrics.CounterVec
	retries       *metrics.CounterVec
	rejected      *metrics.CounterVec
	cacheLookups  *metrics.CounterVec
	responseCache *metrics.CounterVec
	ttftTimeouts  *metrics.CounterVec
	clientReqs    *metrics.CounterVec
	clientTokens  *metrics.CounterVec
	duration      *metrics.Histogram
	queueWait     *metrics.Histogram
	firstByte     *metrics.Histogram
}

// state is an immutable snapshot of the reloadable configuration.
type state struct {
	cfg       *config.Config
	keys      map[string]clientInfo
	adminKeys []string
	cache     *cache.Cache // nil when disabled
}

func newState(cfg *config.Config, prev *state) *state {
	st := &state{cfg: cfg, keys: map[string]clientInfo{}, adminKeys: cfg.Auth.AdminKeys}
	for _, k := range cfg.Auth.APIKeys {
		st.keys[k] = clientInfo{key: k, priority: 1}
	}
	for _, k := range cfg.Auth.Keys {
		rank, _ := config.PriorityRank(k.Priority)
		st.keys[k.Key] = clientInfo{name: k.Name, key: k.Key, priority: rank}
	}
	if cfg.Cache.Enabled {
		// Keep cached answers across a reload unless the cache's shape changed.
		if prev != nil && prev.cache != nil && prev.cfg.Cache == cfg.Cache {
			st.cache = prev.cache
		} else {
			st.cache = cache.New(cfg.Cache.MaxBytes, cfg.Cache.TTL.D())
		}
	}
	return st
}

func (s *Server) conf() *config.Config { return s.state.Load().cfg }

func limitsFrom(cfg *config.Config) map[string]limits.Limits {
	out := map[string]limits.Limits{}
	for _, k := range cfg.Auth.Keys {
		if k.Name != "" {
			out[k.Name] = limits.Limits{MaxConcurrent: k.MaxConcurrent, RequestsPerMinute: k.RequestsPerMinute, TokensPerDay: k.TokensPerDay}
		}
	}
	return out
}

// usageName is the name a client's usage is counted under.
func usageName(st *state, c clientInfo) string {
	switch {
	case c.name != "":
		return c.name
	case st.cfg.Auth.Enabled():
		return "default" // an unnamed key from api_keys
	default:
		return "anonymous"
	}
}

func (s *Server) handleUsage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"clients": s.limits.Snapshot()})
}

// clientInfo is who sent a request, from the API key it used.
type clientInfo struct {
	name     string
	key      string
	priority int
}

type clientKeyCtx struct{}

func clientFrom(ctx context.Context) clientInfo {
	if c, ok := ctx.Value(clientKeyCtx{}).(clientInfo); ok {
		return c
	}
	return clientInfo{priority: 1}
}

func New(cfg *config.Config, p *pool.Pool, client *http.Client, log *slog.Logger) *Server {
	reg := &metrics.Registry{}
	s := &Server{
		pool:     p,
		limits:   limits.New(),
		affinity: affinity.New(cfg.Routing.AffinityCapacity, cfg.Routing.AffinityTTL.D()),
		client:   client,
		log:      log,
		reg:      reg,
		mux:      http.NewServeMux(),

		requests:     reg.NewCounterVec("siliconrouter_requests_total", "Proxied requests by backend and HTTP status.", "backend", "code"),
		upstreamErrs: reg.NewCounterVec("siliconrouter_upstream_errors_total", "Failed upstream attempts (connection errors and retryable statuses).", "backend"),
		retries:      reg.NewCounterVec("siliconrouter_retries_total", "Requests re-sent to another backend after a failure."),
		rejected:     reg.NewCounterVec("siliconrouter_rejected_total", "Requests rejected before reaching a backend.", "reason"),
		cacheLookups: reg.NewCounterVec("siliconrouter_prefix_cache_routing_total", "Routing decisions by prefix-cache outcome: hit (sent to the backend holding the prefix), rebalanced (holder too busy), miss (unknown prefix).", "result"),
		duration:     reg.NewHistogram("siliconrouter_request_duration_seconds", "End-to-end request latency.", metrics.LatencyBuckets),
		queueWait:    reg.NewHistogram("siliconrouter_queue_wait_seconds", "Time spent waiting for a backend slot.", metrics.LatencyBuckets),
		firstByte:    reg.NewHistogram("siliconrouter_upstream_ttfb_seconds", "Time from dispatch to first upstream response headers.", metrics.LatencyBuckets),

		responseCache: reg.NewCounterVec("siliconrouter_response_cache_total", "Response cache lookups: hit (answered from memory), miss, or skip (request not cacheable).", "result"),
		ttftTimeouts:  reg.NewCounterVec("siliconrouter_first_token_timeouts_total", "Streaming requests moved to another backend because the first one produced no output within retry.first_token_timeout.", "backend"),
		clientReqs:    reg.NewCounterVec("siliconrouter_client_requests_total", "Admitted requests per client (API key name).", "client"),
		clientTokens:  reg.NewCounterVec("siliconrouter_client_tokens_total", "Tokens used per client: type is prompt or completion; source is reported (by the backend) or estimated (by the router).", "client", "type", "source"),
	}
	s.limits.SetLimits(limitsFrom(cfg))
	s.state.Store(newState(cfg, nil))
	if c := cfg.Cluster; c.Enabled() {
		s.cluster = cluster.New(c.NodeName, c.Peers, c.Secret, client, s.affinity, s.limits, log)
		s.mux.Handle("POST "+cluster.SyncPath, s.cluster)
	}
	reg.NewGaugeFunc("siliconrouter_response_cache_bytes", "Bytes held by the response cache.", nil, func(emit func(float64, ...string)) {
		if c := s.state.Load().cache; c != nil {
			_, n := c.Stats()
			emit(float64(n))
		}
	})
	reg.NewGaugeFunc("siliconrouter_backend_ttft_seconds", "Recent average time to first token on streaming requests, per backend.", []string{"backend"}, func(emit func(float64, ...string)) {
		for _, st := range p.Snapshot() {
			if st.TTFTMillis > 0 {
				emit(st.TTFTMillis/1000, st.Name)
			}
		}
	})
	reg.NewGaugeFunc("siliconrouter_backend_inflight", "Requests in flight per backend.", []string{"backend"}, func(emit func(float64, ...string)) {
		for _, st := range p.Snapshot() {
			emit(float64(st.Inflight), st.Name)
		}
	})
	reg.NewGaugeFunc("siliconrouter_backend_healthy", "1 if the backend is in rotation.", []string{"backend", "tier"}, func(emit func(float64, ...string)) {
		for _, st := range p.Snapshot() {
			emit(boolFloat(st.Healthy), st.Name, st.Tier)
		}
	})
	reg.NewGaugeFunc("siliconrouter_backend_accelerators", "Devices chipagent reports per backend, by vendor and kind (gpu, npu, accelerator, display).", []string{"backend", "vendor", "kind"}, func(emit func(float64, ...string)) {
		for _, st := range p.Snapshot() {
			if st.Hardware == nil {
				continue
			}
			type key struct{ vendor, kind string }
			count := map[key]int{}
			var keys []key
			for _, d := range st.Hardware.Devices {
				k := key{d.Vendor, d.Kind}
				if count[k] == 0 {
					keys = append(keys, k)
				}
				count[k]++
			}
			for _, k := range keys {
				emit(float64(count[k]), st.Name, k.vendor, k.kind)
			}
		}
	})
	reg.NewGaugeFunc("siliconrouter_backend_accelerator_memory_bytes", "Accelerator memory per backend from chipagent.", []string{"backend", "state"}, func(emit func(float64, ...string)) {
		for _, st := range p.Snapshot() {
			if st.Hardware != nil && st.Hardware.Accelerators > 0 {
				emit(float64(st.Hardware.MemoryTotalBytes), st.Name, "total")
				emit(float64(st.Hardware.MemoryUsedBytes), st.Name, "used")
			}
		}
	})
	reg.NewGaugeFunc("siliconrouter_backend_accelerator_utilization_percent", "Mean accelerator busy percentage per backend from chipagent.", []string{"backend"}, func(emit func(float64, ...string)) {
		for _, st := range p.Snapshot() {
			if st.Hardware != nil && st.Hardware.UtilizationPercent != nil {
				emit(*st.Hardware.UtilizationPercent, st.Name)
			}
		}
	})
	reg.NewGaugeFunc("siliconrouter_queue_depth", "Requests waiting for a backend slot.", nil, func(emit func(float64, ...string)) {
		emit(float64(p.QueueDepth()))
	})
	reg.NewGaugeFunc("siliconrouter_prefix_table_entries", "Prompt prefixes tracked for cache-aware routing.", nil, func(emit func(float64, ...string)) {
		emit(float64(s.affinity.Len()))
	})

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		reg.Write(w)
	})
	s.mux.Handle("GET /admin/backends", s.requireAdmin(http.HandlerFunc(s.handleBackends)))
	s.mux.Handle("GET /admin/usage", s.requireAdmin(http.HandlerFunc(s.handleUsage)))
	s.mux.Handle("POST /admin/reload", s.requireAdmin(http.HandlerFunc(s.handleReload)))
	s.mux.Handle("GET /v1/models", s.requireAuth(http.HandlerFunc(s.handleModels)))
	// Everything else is forwarded, so any HTTP inference API works: OpenAI,
	// Anthropic, Ollama, TGI, Triton/KServe, Gemini-style, or a custom one.
	s.mux.Handle("/", s.requireAuth(http.HandlerFunc(s.handleInference)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// RunCluster keeps this router in step with its peers until ctx is done.
// It returns at once when no cluster is configured.
func (s *Server) RunCluster(ctx context.Context) {
	if s.cluster != nil {
		s.cluster.Run(ctx, s.conf().Cluster.SyncInterval.D())
	}
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.state.Load()
		if !st.cfg.Auth.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if got := clientKey(r.Header); got != "" {
			// Compare against every key in constant time, so timing does not
			// reveal how much of a guess was right.
			var match clientInfo
			found := false
			for k, c := range st.keys {
				if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
					match, found = c, true
				}
			}
			if found {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKeyCtx{}, match)))
				return
			}
		}
		s.rejected.Inc("unauthorized")
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
	})
}

// requireAdmin guards /admin/*: only admin keys when any are configured,
// otherwise any client key (or nothing, when auth is off).
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	clientAuth := s.requireAuth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.state.Load()
		if len(st.adminKeys) == 0 {
			clientAuth.ServeHTTP(w, r)
			return
		}
		got := clientKey(r.Header)
		ok := false
		for _, k := range st.adminKeys {
			if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
				ok = true
			}
		}
		if !ok {
			s.rejected.Inc("unauthorized")
			writeError(w, http.StatusUnauthorized, "invalid_admin_key", "this endpoint needs an admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKeyHeaders carry a caller's API key: OpenAI-style bearer tokens,
// Anthropic's x-api-key and Azure OpenAI's api-key.
var clientKeyHeaders = []string{"Authorization", "X-Api-Key", "Api-Key"}

func clientKey(h http.Header) string {
	if k, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer "); ok {
		return k
	}
	if k := h.Get("X-Api-Key"); k != "" {
		return k
	}
	return h.Get("Api-Key")
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	for _, st := range s.pool.Snapshot() {
		if st.Healthy {
			io.WriteString(w, "ready\n")
			return
		}
	}
	http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
}

func (s *Server) handleBackends(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"backends":    s.pool.Snapshot(),
		"queue_depth": s.pool.QueueDepth(),
	})
}

// handleModels lists the union of models served by healthy backends.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	seen := map[string]bool{}
	data := []model{}
	for _, st := range s.pool.Snapshot() {
		if !st.Healthy {
			continue
		}
		for _, m := range st.Models {
			if !seen[m] {
				seen[m] = true
				data = append(data, model{ID: m, Object: "model", OwnedBy: "siliconrouter"})
			}
		}
	}
	for alias, target := range s.conf().Models.Aliases {
		if seen[target] && !seen[alias] {
			seen[alias] = true
			data = append(data, model{ID: alias, Object: "model", OwnedBy: "siliconrouter"})
		}
	}
	slices.SortFunc(data, func(a, b model) int { return strings.Compare(a.ID, b.ID) })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// readBody reads a request body into a buffer of exactly Content-Length
// bytes when the client sent one, instead of growing a buffer repeatedly.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, limit)
	if n := r.ContentLength; n > 0 && n <= limit {
		buf := make([]byte, n)
		if _, err := io.ReadFull(body, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(body)
}

// relayBuffers are reused across responses; streams copy through them chunk by chunk.
var relayBuffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	st := s.state.Load()
	client := clientFrom(r.Context())
	who := usageName(st, client)
	release, rejected := s.limits.Admit(who)
	if rejected != nil {
		s.rejected.Inc(rejected.Reason)
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(rejected.RetryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, rejected.Reason, rejected.Message)
		return
	}
	defer release()
	s.clientReqs.Inc(who)

	body, err := readBody(w, r, st.cfg.MaxBodyBytes)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.rejected.Inc("body_too_large")
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds max_body_bytes")
			return
		}
		s.rejected.Inc("bad_request")
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
		return
	}
	path := r.URL.Path
	info := prompt.Inspect(path, r.Header.Get("Content-Type"), body)
	rl := &requestLog{r: r, info: info, start: start, client: client}

	// Model aliases: the client's name for a model becomes the real one,
	// in the body or, for APIs that put it there, in the URL.
	if target, ok := st.cfg.Models.Aliases[info.Model]; ok {
		if rewritten, ok := prompt.ReplaceModel(body, target); ok {
			body = rewritten
		} else {
			path = prompt.ReplacePathModel(path, target)
		}
		rl.alias = info.Model
		info.Model, rl.info.Model = target, target
	}

	priority := rl.client.priority
	if st.cfg.Queue.PriorityHeader {
		if rank, ok := config.PriorityRank(r.Header.Get(PriorityHeader)); ok && r.Header.Get(PriorityHeader) != "" {
			priority = rank
		}
	}
	rl.priority = priority

	var cacheKey cache.Key
	cacheable := cacheable(st, r, path, info)
	switch {
	case cacheable:
		cacheKey = cacheKeyFor(st, r, path, rl.client, body)
		if e, ok := st.cache.Get(cacheKey); ok {
			s.responseCache.Inc("hit")
			rl.cache = "hit"
			serveCached(w, e)
			s.requests.Inc("cache", strconv.Itoa(e.Status))
			s.duration.Observe(time.Since(start).Seconds())
			s.logRequest(rl, e.Backend, e.Status, 0, false, nil)
			return
		}
		s.responseCache.Inc("miss")
		rl.cache = "miss"
	case st.cache != nil:
		s.responseCache.Inc("skip")
	}

	rc := st.cfg.Routing
	req := pool.Request{
		Model:        info.Model,
		PromptTokens: prompt.EstimateTokens(info.Text, rc.CharsPerToken),
		MaxTokens:    info.MaxTokens,
		Exclude:      map[string]bool{},
		Priority:     priority,
	}
	// Near a backend's context limit, an estimate is not good enough: count
	// exactly, so the prompt neither goes to a backend that must reject it
	// nor skips one it would fit.
	if counter, near := s.pool.NearContextLimit(req, rc.ExactTokensMargin); near && counter != nil {
		if n, ok := s.exactTokens(r.Context(), counter, info.Model, body, info); ok {
			rl.exactTokens = n
			req.PromptTokens = n
		}
	}

	var keys []uint64
	if rc.AffinityEnabled() {
		keys = prompt.BlockHashes(info.Text, rc.BlockChars, rc.MaxBlocks)
		if sid := r.Header.Get(SessionHeader); sid != "" {
			// Deepest position, so an explicit session wins over prefix matches.
			keys = append(keys, prompt.SessionKey(sid))
		}
		req.Preferred, _ = s.affinity.Lookup(keys)
	}

	acquireCtx, cancel := context.WithTimeout(r.Context(), st.cfg.Queue.MaxWait.D())
	defer cancel()

	var lastErr string
	for attempt := 1; ; attempt++ {
		lease, err := s.pool.Acquire(acquireCtx, req)
		if err != nil {
			status := s.rejectAcquire(w, r, err, attempt, lastErr)
			s.logRequest(rl, "", status, attempt, false, err)
			return
		}
		s.queueWait.Observe(lease.Waited.Seconds())
		b := lease.Backend
		final := attempt >= st.cfg.Retry.MaxAttempts

		res := s.attempt(r, path, body, b, info.Stream, final)
		if res.err != nil || (res.resp != nil && retryable(res.resp.StatusCode) && !final) {
			if res.resp != nil {
				io.Copy(io.Discard, io.LimitReader(res.resp.Body, 64<<10))
				res.resp.Body.Close()
			}
			res.cancel()
			lease.Release()
			if r.Context().Err() != nil {
				s.logRequest(rl, b.Name, 499, attempt, lease.Affinity, r.Context().Err())
				return // client went away; nobody to answer
			}
			s.upstreamErrs.Inc(b.Name)
			if res.err != nil {
				lastErr = fmt.Sprintf("%s: %v", b.Name, res.err)
				s.pool.Observe(b, false, true, res.err.Error())
			} else {
				lastErr = fmt.Sprintf("%s: %s", b.Name, res.resp.Status)
				if res.resp.StatusCode != http.StatusTooManyRequests {
					s.pool.Observe(b, false, true, res.resp.Status)
				}
			}
			if res.err != nil && final {
				s.requests.Inc(b.Name, "502")
				writeError(w, http.StatusBadGateway, "upstream_error", "backend request failed: "+lastErr)
				s.logRequest(rl, b.Name, http.StatusBadGateway, attempt, lease.Affinity, res.err)
				return
			}
			req.Exclude[b.Name] = true
			s.retries.Inc()
			continue
		}
		resp := res.resp
		if resp.StatusCode < 500 {
			s.pool.Observe(b, true, true, "")
		}
		if resp.StatusCode < 400 && len(keys) > 0 {
			s.affinity.Record(keys, b.Name)
			if s.cluster != nil {
				s.cluster.Note(keys, b.Name)
			}
		}
		if len(keys) > 0 {
			switch {
			case lease.Affinity:
				s.cacheLookups.Inc("hit")
			case req.Preferred != "":
				s.cacheLookups.Inc("rebalanced")
			default:
				s.cacheLookups.Inc("miss")
			}
		}

		var capture *captureBuffer
		if cacheable && resp.StatusCode == http.StatusOK {
			capture = &captureBuffer{limit: st.cfg.Cache.MaxEntryBytes}
			w.Header().Set("X-SiliconRouter-Cache", "miss")
		}
		meter := newUsageMeter(resp.Header.Get("Content-Type"))
		copyErr := s.relay(w, resp, b.Name, lease.Affinity, res.first, capture, meter)
		resp.Body.Close()
		res.cancel()
		lease.Release()
		if resp.StatusCode < 400 {
			s.recordUsage(who, path, info, meter, st.cfg.Routing.CharsPerToken)
		}
		if capture != nil && copyErr == nil && !capture.overflow {
			st.cache.Put(cacheKey, &cache.Entry{Status: resp.StatusCode, Header: cacheableHeader(resp.Header), Body: capture.buf.Bytes(), Backend: b.Name})
		}
		s.requests.Inc(b.Name, strconv.Itoa(resp.StatusCode))
		s.duration.Observe(time.Since(start).Seconds())
		s.logRequest(rl, b.Name, resp.StatusCode, attempt, lease.Affinity, copyErr)
		return
	}
}

// attemptResult is one try at a backend. On success the caller must call
// cancel after the response has been relayed.
type attemptResult struct {
	resp   *http.Response
	first  []byte // the first bytes of a streamed body, already read
	err    error
	cancel context.CancelFunc
}

var errFirstTokenTimeout = errors.New("no output within retry.first_token_timeout")

// attempt sends the request to b. For a streaming request it also waits for
// the first bytes of the body before returning, so that a backend that
// accepts a request and then stalls can be abandoned, and the request tried
// elsewhere, while nothing has reached the client yet. The final attempt has
// no timeout: a slow answer is better than none.
func (s *Server) attempt(r *http.Request, path string, body []byte, b *pool.Backend, stream, final bool) attemptResult {
	ctx, cancel := context.WithCancel(r.Context())
	var timer *time.Timer
	timeout := s.conf().Retry.FirstTokenTimeout.D()
	if stream && timeout > 0 && !final {
		timer = time.AfterFunc(timeout, cancel)
	}
	// stopTimer reports whether the deadline had already passed. Call it once.
	stopTimer := func() (expired bool) { return timer != nil && !timer.Stop() }
	fail := func(err error, expired bool) attemptResult {
		cancel()
		if expired {
			s.ttftTimeouts.Inc(b.Name)
			s.pool.ObserveTTFT(b, timeout)
			err = errFirstTokenTimeout
		}
		return attemptResult{err: err, cancel: cancel}
	}

	dispatched := time.Now()
	resp, err := s.forward(ctx, r, path, b, body)
	if err != nil {
		return fail(err, stopTimer())
	}
	s.firstByte.Observe(time.Since(dispatched).Seconds())
	if !stream || resp.StatusCode != http.StatusOK {
		stopTimer()
		return attemptResult{resp: resp, cancel: cancel}
	}

	buf := make([]byte, 4<<10)
	n, rerr := resp.Body.Read(buf)
	expired := stopTimer()
	if expired || (n == 0 && rerr != nil && rerr != io.EOF) {
		resp.Body.Close()
		return fail(rerr, expired)
	}
	s.pool.ObserveTTFT(b, time.Since(dispatched))
	return attemptResult{resp: resp, first: buf[:n], cancel: cancel}
}

// cacheable reports whether a request's answer can be served from, and
// stored in, the response cache.
func cacheable(st *state, r *http.Request, path string, info prompt.Info) bool {
	if st.cache == nil || r.Method != http.MethodPost {
		return false
	}
	if cc := r.Header.Get("Cache-Control"); strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") {
		return false
	}
	// Embeddings never vary; generations only at temperature 0.
	return st.cfg.Cache.Mode == "all" || info.Deterministic || strings.Contains(path, "embed")
}

func cacheKeyFor(st *state, r *http.Request, path string, c clientInfo, body []byte) cache.Key {
	who := c.key
	if st.cfg.Cache.ShareAcrossKeys {
		who = ""
	}
	return cache.NewKey([]byte(r.Method), []byte(path), []byte(r.URL.RawQuery), []byte(who), body)
}

// cacheableHeader keeps the response headers worth replaying.
func cacheableHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range hopHeaders {
		out.Del(k)
	}
	out.Del("Content-Length")
	out.Del("Date")
	out.Del("Set-Cookie")
	return out
}

func serveCached(w http.ResponseWriter, e *cache.Entry) {
	h := w.Header()
	for k, v := range e.Header {
		h[k] = v
	}
	h.Set("X-SiliconRouter-Backend", e.Backend)
	h.Set("X-SiliconRouter-Cache", "hit")
	h.Set("Content-Length", strconv.Itoa(len(e.Body)))
	w.WriteHeader(e.Status)
	w.Write(e.Body)
}

func retryable(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// rejectAcquire answers a request that never got a backend slot and returns the status sent.
func (s *Server) rejectAcquire(w http.ResponseWriter, r *http.Request, err error, attempt int, lastErr string) int {
	switch {
	case errors.Is(err, pool.ErrQueueFull):
		s.rejected.Inc("queue_full")
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "overloaded", "all backends are busy and the queue is full; retry shortly")
		return http.StatusTooManyRequests
	case errors.Is(err, pool.ErrNoBackend) && attempt > 1:
		s.rejected.Inc("all_backends_failed")
		writeError(w, http.StatusBadGateway, "upstream_error", "every eligible backend failed; last error: "+lastErr)
		return http.StatusBadGateway
	case errors.Is(err, pool.ErrNoBackend):
		s.rejected.Inc("no_backend")
		writeError(w, http.StatusServiceUnavailable, "no_backend", "no healthy backend serves this model with enough context")
		return http.StatusServiceUnavailable
	case r.Context().Err() != nil:
		s.rejected.Inc("client_closed")
		return 499
	default:
		s.rejected.Inc("queue_timeout")
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "queue_timeout", "timed out waiting for a free backend")
		return http.StatusServiceUnavailable
	}
}

// Hop-by-hop headers are meaningful only for a single connection (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func (s *Server) forward(ctx context.Context, r *http.Request, path string, b *pool.Backend, body []byte) (*http.Response, error) {
	u := *b.URL
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Header.Del(SessionHeader)
	out.Header.Del(PriorityHeader)
	if s.conf().Auth.Enabled() {
		for _, h := range clientKeyHeaders {
			out.Header.Del(h) // the router's key is not the backend's
		}
	}
	b.SetAuth(out.Header)
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			ip = prior + ", " + ip
		}
		out.Header.Set("X-Forwarded-For", ip)
	}
	out.ContentLength = int64(len(body))
	return s.client.Do(out)
}

// relay copies the upstream response to the client, flushing as bytes
// arrive so streamed tokens reach the user without buffering.
// captureBuffer copies a response body for the cache, giving up (and
// releasing the memory) once it passes limit bytes.
type captureBuffer struct {
	buf      bytes.Buffer
	limit    int64
	overflow bool
}

func (c *captureBuffer) write(p []byte) {
	if c.overflow {
		return
	}
	if int64(c.buf.Len()+len(p)) > c.limit {
		c.overflow = true
		c.buf = bytes.Buffer{}
		return
	}
	c.buf.Write(p)
}

// usageTail is how much of a response's end is kept to find its token
// usage, which every API reports at or near the end.
const usageTail = 64 << 10

// usageMeter watches a response go by to count the tokens it used.
type usageMeter struct {
	tail   []byte
	bytes  int64
	events int64 // SSE data events or NDJSON lines, about one token each
	sse    bool
	ndjson bool
}

func newUsageMeter(contentType string) *usageMeter {
	return &usageMeter{
		sse:    strings.HasPrefix(contentType, "text/event-stream"),
		ndjson: strings.Contains(contentType, "ndjson"),
	}
}

func (m *usageMeter) write(p []byte) {
	m.bytes += int64(len(p))
	switch {
	case m.sse:
		m.events += int64(bytes.Count(p, []byte("data:")))
	case m.ndjson:
		m.events += int64(bytes.Count(p, []byte("\n")))
	}
	m.tail = append(m.tail, p...)
	if len(m.tail) > 2*usageTail { // trim rarely, keeping the last usageTail bytes
		m.tail = append(m.tail[:0], m.tail[len(m.tail)-usageTail:]...)
	}
}

// recordUsage counts a finished request's tokens: as the backend reported
// them when it did, otherwise estimated.
func (s *Server) recordUsage(who, path string, info prompt.Info, m *usageMeter, charsPerToken float64) {
	u := prompt.ParseUsage(m.tail)
	in, out := u.PromptTokens, u.CompletionTokens
	inSource, outSource := "reported", "reported"
	if in < 0 {
		in, inSource = int64(prompt.EstimateTokens(info.Text, charsPerToken)), "estimated"
	}
	if out < 0 {
		outSource = "estimated"
		switch {
		case strings.Contains(path, "embed") || u.PromptTokens >= 0:
			out = 0 // embeddings generate nothing; a usage block without completions means none
		case m.sse || m.ndjson:
			out = m.events - int64(bytes.Count(m.tail, []byte("[DONE]")))
		default:
			out = int64(float64(m.bytes) / charsPerToken)
		}
		if out < 0 {
			out = 0
		}
	}
	s.limits.Record(who, in, out, inSource == "estimated" || outSource == "estimated")
	s.clientTokens.Add(float64(in), who, "prompt", inSource)
	s.clientTokens.Add(float64(out), who, "completion", outSource)
}

// first holds body bytes already read from resp; capture, when non-nil,
// receives a copy of the body for the cache.
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, backend string, cacheHit bool, first []byte, capture *captureBuffer, meter *usageMeter) error {
	h := w.Header()
	for k, v := range resp.Header {
		h[k] = v
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
	h.Del("Content-Length") // we may re-chunk; let net/http decide framing
	h.Set("X-SiliconRouter-Backend", backend)
	if cacheHit {
		h.Set("X-SiliconRouter-Prefix-Cache", "hit")
	} else {
		h.Set("X-SiliconRouter-Prefix-Cache", "miss")
	}
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	write := func(p []byte) error {
		if _, err := w.Write(p); err != nil {
			return err
		}
		rc.Flush()
		if capture != nil {
			capture.write(p)
		}
		meter.write(p)
		return nil
	}
	if len(first) > 0 {
		if err := write(first); err != nil {
			return err
		}
	}
	bp := relayBuffers.Get().(*[]byte)
	defer relayBuffers.Put(bp)
	buf := *bp
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if werr := write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// requestLog collects what the request log line reports.
type requestLog struct {
	r           *http.Request
	info        prompt.Info
	start       time.Time
	client      clientInfo
	priority    int
	alias       string
	cache       string
	exactTokens int
}

var priorityNames = [...]string{"low", "normal", "high"}

func (s *Server) logRequest(l *requestLog, backend string, status, attempts int, cacheHit bool, err error) {
	attrs := []any{
		"method", l.r.Method, "path", l.r.URL.Path, "model", l.info.Model, "stream", l.info.Stream,
		"backend", backend, "status", status, "attempts", attempts,
		"prefix_cache_hit", cacheHit, "duration_ms", time.Since(l.start).Milliseconds(),
		"priority", priorityNames[l.priority],
	}
	if l.client.name != "" {
		attrs = append(attrs, "client", l.client.name)
	}
	if l.alias != "" {
		attrs = append(attrs, "alias", l.alias)
	}
	if l.cache != "" {
		attrs = append(attrs, "response_cache", l.cache)
	}
	if l.exactTokens > 0 {
		attrs = append(attrs, "prompt_tokens_exact", l.exactTokens)
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.log.Info("request", attrs...)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "siliconrouter_error", "code": code},
	})
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
