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
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/affinity"
	"github.com/capable78638974979473297813001-pixel/chips/internal/cache"
	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
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
	cfg      *config.Config
	pool     *pool.Pool
	affinity *affinity.Table
	cache    *cache.Cache // nil when disabled
	keys     map[string]clientInfo
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
	duration      *metrics.Histogram
	queueWait     *metrics.Histogram
	firstByte     *metrics.Histogram
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
		cfg:      cfg,
		pool:     p,
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
	}
	s.keys = map[string]clientInfo{}
	for _, k := range cfg.Auth.APIKeys {
		s.keys[k] = clientInfo{key: k, priority: 1}
	}
	for _, k := range cfg.Auth.Keys {
		rank, _ := config.PriorityRank(k.Priority)
		s.keys[k.Key] = clientInfo{name: k.Name, key: k.Key, priority: rank}
	}
	if cfg.Cache.Enabled {
		s.cache = cache.New(cfg.Cache.MaxBytes, cfg.Cache.TTL.D())
		reg.NewGaugeFunc("siliconrouter_response_cache_bytes", "Bytes held by the response cache.", nil, func(emit func(float64, ...string)) {
			_, n := s.cache.Stats()
			emit(float64(n))
		})
	}
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
	s.mux.Handle("GET /admin/backends", s.requireAuth(http.HandlerFunc(s.handleBackends)))
	s.mux.Handle("GET /v1/models", s.requireAuth(http.HandlerFunc(s.handleModels)))
	// Everything else is forwarded, so any HTTP inference API works: OpenAI,
	// Anthropic, Ollama, TGI, Triton/KServe, Gemini-style, or a custom one.
	s.mux.Handle("/", s.requireAuth(http.HandlerFunc(s.handleInference)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) requireAuth(next http.Handler) http.Handler {
	if !s.cfg.Auth.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := clientKey(r.Header); got != "" {
			// Compare against every key in constant time, so timing does not
			// reveal how much of a guess was right.
			var match clientInfo
			found := false
			for k, c := range s.keys {
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
	for alias, target := range s.cfg.Models.Aliases {
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
	body, err := readBody(w, r, s.cfg.MaxBodyBytes)
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
	rl := &requestLog{r: r, info: info, start: start, client: clientFrom(r.Context())}

	// Model aliases: the client's name for a model becomes the real one,
	// in the body or, for APIs that put it there, in the URL.
	if target, ok := s.cfg.Models.Aliases[info.Model]; ok {
		if rewritten, ok := prompt.ReplaceModel(body, target); ok {
			body = rewritten
		} else {
			path = prompt.ReplacePathModel(path, target)
		}
		rl.alias = info.Model
		info.Model, rl.info.Model = target, target
	}

	priority := rl.client.priority
	if s.cfg.Queue.PriorityHeader {
		if rank, ok := config.PriorityRank(r.Header.Get(PriorityHeader)); ok && r.Header.Get(PriorityHeader) != "" {
			priority = rank
		}
	}
	rl.priority = priority

	var cacheKey cache.Key
	cacheable := s.cacheable(r, path, info)
	switch {
	case cacheable:
		cacheKey = s.cacheKey(r, path, rl.client, body)
		if e, ok := s.cache.Get(cacheKey); ok {
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
	case s.cache != nil:
		s.responseCache.Inc("skip")
	}

	rc := s.cfg.Routing
	req := pool.Request{
		Model:        info.Model,
		PromptTokens: prompt.EstimateTokens(info.Text, rc.CharsPerToken),
		MaxTokens:    info.MaxTokens,
		Exclude:      map[string]bool{},
		Priority:     priority,
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

	acquireCtx, cancel := context.WithTimeout(r.Context(), s.cfg.Queue.MaxWait.D())
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
		final := attempt >= s.cfg.Retry.MaxAttempts

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
			capture = &captureBuffer{limit: s.cfg.Cache.MaxEntryBytes}
			w.Header().Set("X-SiliconRouter-Cache", "miss")
		}
		copyErr := s.relay(w, resp, b.Name, lease.Affinity, res.first, capture)
		resp.Body.Close()
		res.cancel()
		lease.Release()
		if capture != nil && copyErr == nil && !capture.overflow {
			s.cache.Put(cacheKey, &cache.Entry{Status: resp.StatusCode, Header: cacheableHeader(resp.Header), Body: capture.buf.Bytes(), Backend: b.Name})
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
	timeout := s.cfg.Retry.FirstTokenTimeout.D()
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
func (s *Server) cacheable(r *http.Request, path string, info prompt.Info) bool {
	if s.cache == nil || r.Method != http.MethodPost {
		return false
	}
	if cc := r.Header.Get("Cache-Control"); strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") {
		return false
	}
	// Embeddings never vary; generations only at temperature 0.
	return s.cfg.Cache.Mode == "all" || info.Deterministic || strings.Contains(path, "embed")
}

func (s *Server) cacheKey(r *http.Request, path string, c clientInfo, body []byte) cache.Key {
	who := c.key
	if s.cfg.Cache.ShareAcrossKeys {
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
	if s.cfg.Auth.Enabled() {
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

// first holds body bytes already read from resp; capture, when non-nil,
// receives a copy of the body for the cache.
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, backend string, cacheHit bool, first []byte, capture *captureBuffer) error {
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
	r        *http.Request
	info     prompt.Info
	start    time.Time
	client   clientInfo
	priority int
	alias    string
	cache    string
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
