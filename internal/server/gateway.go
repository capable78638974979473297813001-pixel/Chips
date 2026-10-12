package server

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/extproc"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
)

// Gateway mode: the router as the endpoint picker of a Kubernetes Gateway
// API Inference Extension InferencePool. The gateway (Envoy-based) asks
// which pod gets each request and sends it there; the router decides
// exactly as it would when proxying (auth, limits, model policy, aliases,
// LoRA adapters, prefix-cache affinity, hardware tiers, engine load, cold
// starts) and follows the response to count tokens, cost and health. What
// it can't do here is retry or fall back to another model mid-request:
// the gateway owns the connection.

// Gateway returns the ext_proc server to listen on for gateway mode.
func (s *Server) Gateway() *extproc.Server {
	return &extproc.Server{
		Picker:  gatewayPicker{s},
		Log:     s.log,
		Ready:   s.Ready,
		MaxBody: int(s.conf().MaxBodyBytes),
	}
}

// Ready reports whether the router should get traffic: not draining, and
// some backend in rotation.
func (s *Server) Ready() bool {
	if s.draining.Load() {
		return false
	}
	for _, st := range s.pool.Snapshot() {
		if st.Healthy && !st.Drained {
			return true
		}
	}
	return false
}

type gatewayPicker struct{ s *Server }

// hostPort is a backend URL's "ip:port", as gateways name endpoints.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443"}[u.Scheme]
		if port == "" {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// capture records an answer the router's usual handlers write, to hand it
// to the gateway as an ImmediateResponse instead.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCapture() *capture                     { return &capture{header: http.Header{}} }
func (c *capture) Header() http.Header         { return c.header }
func (c *capture) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *capture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *capture) decision() extproc.Decision {
	r := &extproc.Reject{Status: c.status, Body: c.body.Bytes()}
	if r.Status == 0 || r.Status == 499 {
		r.Status = http.StatusServiceUnavailable
	}
	if v := c.header.Get("Retry-After"); v != "" {
		r.Header = append(r.Header, [2]string{"retry-after", v})
	}
	return extproc.Decision{Reject: r}
}

func (g gatewayPicker) Pick(ctx context.Context, r *extproc.Request) extproc.Decision {
	s := g.s
	start := time.Now()
	st := s.state.Load()
	u, err := url.ParseRequestURI(r.Path)
	if err != nil {
		u = &url.URL{Path: "/"}
	}
	hr := (&http.Request{Method: r.Method, URL: u, Header: r.Header, Host: r.Authority, Proto: "HTTP/2.0",
		RemoteAddr: r.Header.Get("X-Envoy-External-Address")}).WithContext(ctx)
	rec := newCapture()

	client := clientInfo{priority: 1}
	if st.cfg.Auth.Enabled() {
		c, fail := s.authenticate(hr, st)
		if fail != nil {
			s.refuse(rec, hr, fail)
			return rec.decision()
		}
		client = c
	}
	who := usageName(st, client)
	release, rejected := s.admit(who, client)
	if rejected != nil {
		s.rejected.Inc(rejected.Reason)
		rec.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(rejected.RetryAfter.Seconds()))))
		writeError(rec, http.StatusTooManyRequests, rejected.Reason, rejected.Message)
		return rec.decision()
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	s.clientReqs.Inc(who)

	body, changed := r.Body, false
	if client.restricted() {
		before := len(body)
		var allowed bool
		if body, allowed = s.enforcePolicy(rec, hr, st, client, body); !allowed {
			return rec.decision()
		}
		changed = len(body) != before
	}
	path := u.Path
	info := prompt.Inspect(path, r.Header.Get("Content-Type"), body)
	rl := &requestLog{r: hr, info: info, start: start, client: client, kind: "gateway"}
	rl.trace, rl.span = s.tracer.Begin(r.Header)
	if target, isAlias := st.cfg.Models.Aliases[info.Model]; isAlias {
		if rewritten, done := prompt.ReplaceModel(body, target); done {
			body, changed = rewritten, true
			rl.alias = info.Model
			info.Model, rl.info.Model = target, target
		}
	}
	priority := client.priority
	if st.cfg.Queue.PriorityHeader {
		if rank, valid := config.PriorityRank(r.Header.Get(PriorityHeader)); valid && r.Header.Get(PriorityHeader) != "" {
			priority = rank
		}
	}
	rl.priority = priority

	rc := st.cfg.Routing
	req := pool.Request{Model: info.Model, PromptTokens: prompt.EstimateTokens(info.Text, rc.CharsPerToken),
		MaxTokens: info.MaxTokens, Exclude: map[string]bool{}, Priority: priority}
	adapter, _ := setAdapter(&req, st.cfg.Models)
	if r.Subset != nil {
		req.Only = map[string]bool{}
		allowed := map[string]bool{}
		for _, ep := range r.Subset {
			allowed[ep] = true
		}
		for _, b := range s.pool.Backends() {
			if allowed[hostPort(b.URL)] {
				req.Only[b.Name] = true
			}
		}
	}
	var keys []uint64
	if rc.AffinityEnabled() {
		keys = prompt.BlockHashes(info.Text, rc.BlockChars, rc.MaxBlocks)
		if sid := r.Header.Get(SessionHeader); sid != "" {
			keys = append(keys, prompt.SessionKey(sid))
		}
		req.Preferred, _ = s.affinity.Lookup(keys)
	}

	wait := &queueWait{parent: ctx, max: st.cfg.Queue.MaxWait.D()}
	wait.reset()
	defer func() { wait.cancel() }()
	s.prepareHold(st, &req, wait, 0, rl)

	var lastErr string
	for attempt := 1; ; attempt++ {
		lease, err := s.pool.Acquire(wait.ctx, req)
		if err != nil {
			if rl.coldStart && ctx.Err() == nil && wait.ctx.Err() != nil {
				s.rejected.Inc("cold_start_timeout")
				rec.Header().Set("Retry-After", "10")
				writeError(rec, http.StatusServiceUnavailable, "cold_start_timeout",
					"no backend for this model started within autoscaling.cold_start_wait; retry shortly")
			} else {
				s.rejectAcquire(rec, hr, err, attempt, lastErr)
			}
			s.logRequest(rl, "", rec.status, attempt, false, err)
			return rec.decision()
		}
		s.queueWait.Observe(lease.Waited.Seconds())
		b := lease.Backend
		if lease.NeedsLoad {
			err := s.loadAdapter(ctx, b, adapter)
			s.pool.AdapterLoadDone(b, adapter.Name)
			if err != nil {
				lease.Release()
				s.loraRouting.Inc("load_failed")
				lastErr = fmt.Sprintf("%s: load LoRA adapter %s: %v", b.Name, adapter.Name, err)
				req.Exclude[b.Name] = true
				if attempt < st.cfg.Retry.MaxAttempts {
					continue
				}
				writeError(rec, http.StatusBadGateway, "adapter_load_failed", lastErr)
				s.logRequest(rl, b.Name, http.StatusBadGateway, attempt, false, err)
				return rec.decision()
			}
		}
		ok = true
		ex := &gatewayExchange{s: s, st: st, lease: lease, rl: rl, who: who, client: client, keys: keys,
			path: path, info: info, req: req, release: release, attempts: attempt}
		d := extproc.Decision{Endpoint: hostPort(b.URL), Exchange: ex}
		if changed {
			d.Body = body
		}
		return d
	}
}

// gatewayExchange follows a request the gateway sent to the backend the
// router picked, and releases the slot when it is over.
type gatewayExchange struct {
	s        *Server
	st       *state
	lease    *pool.Lease
	rl       *requestLog
	who      string
	client   clientInfo
	keys     []uint64
	path     string
	info     prompt.Info
	req      pool.Request
	release  func()
	attempts int

	status int
	meter  *usageMeter
	once   sync.Once
}

func (x *gatewayExchange) ResponseHeaders(status int, h http.Header) {
	x.status = status
	x.meter = newUsageMeter(h.Get("Content-Type"))
	s, b := x.s, x.lease.Backend
	switch {
	case status < 500:
		s.pool.Observe(b, true, true, "")
	case retryable(status):
		s.upstreamErrs.Inc(b.Name)
		s.pool.Observe(b, false, true, "HTTP "+strconv.Itoa(status)+" via gateway")
	}
	if status < 400 && len(x.keys) > 0 {
		s.affinity.Record(x.keys, b.Name)
		if s.cluster != nil {
			s.cluster.Note(x.keys, b.Name)
		}
	}
	if len(x.keys) > 0 {
		switch {
		case x.lease.Affinity:
			s.cacheLookups.Inc("hit")
		case len(x.req.Preferred) > 0:
			s.cacheLookups.Inc("rebalanced")
		default:
			s.cacheLookups.Inc("miss")
		}
	}
	if x.req.Base != "" {
		switch {
		case x.lease.AdapterLoaded:
			s.loraRouting.Inc("loaded")
		case x.lease.NeedsLoad:
			s.loraRouting.Inc("loaded_on_demand")
		default:
			s.loraRouting.Inc("unknown")
		}
		if status < 400 {
			s.pool.NoteAdapter(b, x.req.Model)
		}
	}
}

func (x *gatewayExchange) ResponseBody(chunk []byte, _ bool) {
	if x.meter != nil {
		x.meter.write(chunk)
	}
}

func (x *gatewayExchange) Close() {
	x.once.Do(func() {
		s, b := x.s, x.lease.Backend
		x.lease.Release()
		var in, out int64
		if x.status >= 200 && x.status < 400 && x.meter != nil {
			in, out = s.recordUsage(x.who, x.path, x.info, x.meter, x.st.cfg.Routing.CharsPerToken)
			if x.client.user != "" {
				s.limits.Record(userName(x.client.user), in, out, false)
			}
			x.rl.promptTokens, x.rl.completionTokens = in, out
		}
		s.charge(x.rl, x.who, x.lease, in, out)
		code := strconv.Itoa(x.status)
		if x.status == 0 {
			code = "unknown" // the gateway doesn't send responses to the picker
		}
		s.requests.Inc(b.Name, code)
		s.duration.Observe(time.Since(x.rl.start).Seconds())
		s.logRequest(x.rl, b.Name, x.status, x.attempts, x.lease.Affinity, nil)
		x.release()
	})
}
