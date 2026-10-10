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
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/affinity"
	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/metrics"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
)

// SessionHeader lets clients pin a conversation to one backend explicitly,
// which helps when they trim old turns and the prompt prefix changes.
const SessionHeader = "X-Session-ID"

type Server struct {
	cfg      *config.Config
	pool     *pool.Pool
	affinity *affinity.Table
	client   *http.Client
	log      *slog.Logger
	reg      *metrics.Registry
	mux      *http.ServeMux

	requests     *metrics.CounterVec
	upstreamErrs *metrics.CounterVec
	retries      *metrics.CounterVec
	rejected     *metrics.CounterVec
	cacheLookups *metrics.CounterVec
	duration     *metrics.Histogram
	queueWait    *metrics.Histogram
	firstByte    *metrics.Histogram
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
	}
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
	if len(s.cfg.Auth.APIKeys) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := clientKey(r.Header); got != "" {
			for _, k := range s.cfg.Auth.APIKeys {
				if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
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
	slices.SortFunc(data, func(a, b model) int { return strings.Compare(a.ID, b.ID) })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
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
	info := prompt.Inspect(r.URL.Path, r.Header.Get("Content-Type"), body)

	rc := s.cfg.Routing
	req := pool.Request{
		Model:        info.Model,
		PromptTokens: prompt.EstimateTokens(info.Text, rc.CharsPerToken),
		MaxTokens:    info.MaxTokens,
		Exclude:      map[string]bool{},
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
			s.logRequest(r, info, "", status, attempt, start, false, err)
			return
		}
		s.queueWait.Observe(lease.Waited.Seconds())
		b := lease.Backend
		final := attempt >= s.cfg.Retry.MaxAttempts

		dispatched := time.Now()
		resp, err := s.forward(r, b, body)
		if err != nil {
			lease.Release()
			if r.Context().Err() != nil {
				s.logRequest(r, info, b.Name, 499, attempt, start, lease.Affinity, r.Context().Err())
				return // client went away; nobody to answer
			}
			s.upstreamErrs.Inc(b.Name)
			s.pool.Observe(b, false, true, err.Error())
			lastErr = fmt.Sprintf("%s: %v", b.Name, err)
			if final {
				s.requests.Inc(b.Name, "502")
				writeError(w, http.StatusBadGateway, "upstream_error", "backend request failed: "+lastErr)
				s.logRequest(r, info, b.Name, http.StatusBadGateway, attempt, start, lease.Affinity, err)
				return
			}
			req.Exclude[b.Name] = true
			s.retries.Inc()
			continue
		}
		s.firstByte.Observe(time.Since(dispatched).Seconds())

		if retryable(resp.StatusCode) && !final {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			lease.Release()
			s.upstreamErrs.Inc(b.Name)
			if resp.StatusCode != http.StatusTooManyRequests {
				s.pool.Observe(b, false, true, resp.Status)
			}
			lastErr = fmt.Sprintf("%s: %s", b.Name, resp.Status)
			req.Exclude[b.Name] = true
			s.retries.Inc()
			continue
		}
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

		copyErr := s.relay(w, resp, b.Name, lease.Affinity)
		resp.Body.Close()
		lease.Release()
		s.requests.Inc(b.Name, strconv.Itoa(resp.StatusCode))
		s.duration.Observe(time.Since(start).Seconds())
		s.logRequest(r, info, b.Name, resp.StatusCode, attempt, start, lease.Affinity, copyErr)
		return
	}
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

func (s *Server) forward(r *http.Request, b *pool.Backend, body []byte) (*http.Response, error) {
	u := *b.URL
	u.Path = strings.TrimSuffix(u.Path, "/") + r.URL.Path
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Header.Del(SessionHeader)
	if len(s.cfg.Auth.APIKeys) > 0 {
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
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, backend string, cacheHit bool) error {
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
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			rc.Flush()
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) logRequest(r *http.Request, info prompt.Info, backend string, status, attempts int, start time.Time, cacheHit bool, err error) {
	attrs := []any{
		"method", r.Method, "path", r.URL.Path, "model", info.Model, "stream", info.Stream,
		"backend", backend, "status", status, "attempts", attempts,
		"prefix_cache_hit", cacheHit, "duration_ms", time.Since(start).Milliseconds(),
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
