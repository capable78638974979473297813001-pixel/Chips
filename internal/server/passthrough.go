package server

import (
	"context"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
	"github.com/capable78638974979473297813001-pixel/chips/internal/tracing"
)

// isWebSocket reports a WebSocket upgrade request (OpenAI-style realtime APIs).
func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// isGRPC reports a gRPC call (Triton, KServe v2 and other gRPC inference APIs).
func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// newGRPCTransport speaks HTTP/2 to backends: over TLS for https grpc_urls
// and in cleartext (h2c) for http ones, as gRPC servers expect.
func newGRPCTransport(client *http.Client) *http.Transport {
	t := &http.Transport{}
	if base, ok := client.Transport.(*http.Transport); ok {
		t = base.Clone()
	}
	var p http.Protocols
	p.SetHTTP2(true)
	p.SetUnencryptedHTTP2(true)
	t.Protocols = &p
	return t
}

// passthrough relays a WebSocket session or a gRPC call to a backend
// byte for byte. Neither can be retried once it starts, and the router
// cannot read their bodies for routing hints, so the backend is chosen by
// model name (from the URL or a ?model= query) and load, and the slot is
// held for the whole session.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, st *state, client clientInfo, grpc bool) {
	start := time.Now()
	model := r.URL.Query().Get("model")
	if model == "" {
		model = prompt.ModelFromPath(r.URL.Path)
	}
	path, query := r.URL.Path, r.URL.Query()
	if target, ok := st.cfg.Models.Aliases[model]; ok {
		if query.Has("model") {
			query.Set("model", target)
		} else {
			path = prompt.ReplacePathModel(path, target)
		}
		model = target
	}
	kind := "websocket"
	if grpc {
		kind = "grpc"
	}
	rl := &requestLog{r: r, info: prompt.Info{Model: model, Stream: true}, start: start, client: client, priority: client.priority}
	rl.trace, rl.span = s.tracer.Begin(r.Header)

	ctx, cancel := context.WithTimeout(r.Context(), st.cfg.Queue.MaxWait.D())
	lease, err := s.pool.Acquire(ctx, pool.Request{Model: model, GRPC: grpc, Exclude: map[string]bool{}, Priority: client.priority})
	cancel()
	if err != nil {
		if grpc && s.noGRPCBackend() {
			writeError(w, http.StatusNotImplemented, "no_grpc_backend", "no backend has a grpc_url configured")
			return
		}
		status := s.rejectAcquire(w, r, err, 1, "")
		s.logRequest(rl, "", status, 1, false, err)
		return
	}
	defer lease.Release()
	b := lease.Backend
	target := b.URL
	transport := s.client.Transport
	if grpc {
		target, transport = b.GRPCURL, s.grpcTransport
	}
	if transport == nil {
		transport = http.DefaultTransport
	}

	status := 0
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.URL.RawQuery = query.Encode()
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Del(SessionHeader)
			pr.Out.Header.Del(PriorityHeader)
			if st.cfg.Auth.Enabled() {
				for _, h := range clientKeyHeaders {
					pr.Out.Header.Del(h)
				}
			}
			b.SetAuth(pr.Out.Header)
			pr.Out.Header.Set(tracing.Header, tracing.Format(rl.trace.TraceID, rl.span, rl.trace.Sampled))
			pr.SetXForwarded()
		},
		Transport:     transport,
		FlushInterval: -1, // relay every frame as it arrives
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			resp.Header.Set("X-SiliconRouter-Backend", b.Name)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status = http.StatusBadGateway
			s.upstreamErrs.Inc(b.Name)
			s.pool.Observe(b, false, true, err.Error())
			writeError(w, http.StatusBadGateway, "upstream_error", kind+" to "+b.Name+" failed: "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
	s.requests.Inc(b.Name, strconv.Itoa(status))
	s.duration.Observe(time.Since(start).Seconds())
	rl.kind = kind
	s.logRequest(rl, b.Name, status, 1, false, nil)
}

func (s *Server) noGRPCBackend() bool {
	for _, b := range s.pool.Backends() {
		if b.GRPCURL != nil {
			return false
		}
	}
	return true
}
