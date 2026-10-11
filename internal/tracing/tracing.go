// Package tracing propagates W3C trace context through the router and,
// when an OTLP endpoint is configured, exports spans for each request to an
// OpenTelemetry collector over OTLP/HTTP in its JSON encoding. It needs no
// OpenTelemetry SDK: a router produces a few simple spans per request.
package tracing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header is the W3C trace context header.
const Header = "traceparent"

type (
	TraceID [16]byte
	SpanID  [8]byte
)

func (t TraceID) String() string { return hex.EncodeToString(t[:]) }
func (s SpanID) String() string  { return hex.EncodeToString(s[:]) }

func newTraceID() (t TraceID) { rand.Read(t[:]); return }
func newSpanID() (s SpanID)   { rand.Read(s[:]); return }

// Context is one request's place in a trace.
type Context struct {
	TraceID TraceID
	Parent  SpanID // the caller's span, zero if the trace starts here
	Sampled bool
}

// Parse reads a traceparent header ("00-<trace>-<span>-<flags>").
func Parse(h string) (Context, bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return Context{}, false
	}
	var c Context
	if _, err := hex.Decode(c.TraceID[:], []byte(parts[1])); err != nil || c.TraceID == (TraceID{}) {
		return Context{}, false
	}
	if _, err := hex.Decode(c.Parent[:], []byte(parts[2])); err != nil || c.Parent == (SpanID{}) {
		return Context{}, false
	}
	flags, err := strconv.ParseUint(parts[3], 16, 8)
	if err != nil {
		return Context{}, false
	}
	c.Sampled = flags&1 == 1
	return c, true
}

// Format writes a traceparent header naming span as the parent.
func Format(trace TraceID, span SpanID, sampled bool) string {
	flags := "00"
	if sampled {
		flags = "01"
	}
	return "00-" + trace.String() + "-" + span.String() + "-" + flags
}

// Span is a finished unit of work.
type Span struct {
	TraceID TraceID
	SpanID  SpanID
	Parent  SpanID
	Name    string
	Client  bool // a call out to a backend, rather than serving a request
	Start   time.Time
	End     time.Time
	Attrs   map[string]any
	Error   string
}

// Tracer starts traces and exports finished spans. A nil *Tracer, or one
// without an endpoint, still propagates trace context but exports nothing.
type Tracer struct {
	endpoint   string
	service    string
	sampleRate float64
	client     *http.Client
	log        *slog.Logger

	mu    sync.Mutex
	queue []Span
	kick  chan struct{}
}

// New returns a tracer exporting to endpoint (an OTLP/HTTP base URL such as
// http://otel-collector:4318), or a propagate-only tracer if endpoint is empty.
func New(endpoint, service string, sampleRate float64, client *http.Client, log *slog.Logger) *Tracer {
	return &Tracer{endpoint: strings.TrimSuffix(endpoint, "/"), service: service, sampleRate: sampleRate,
		client: client, log: log, kick: make(chan struct{}, 1)}
}

// Begin joins the caller's trace from its traceparent header, or starts a
// new trace sampled at the configured rate. It returns the trace context and
// a new span ID for this request.
func (t *Tracer) Begin(h http.Header) (Context, SpanID) {
	c, ok := Parse(h.Get(Header))
	if !ok {
		c = Context{TraceID: newTraceID(), Sampled: t != nil && t.endpoint != "" && mrand.Float64() < t.sampleRate}
	}
	return c, newSpanID()
}

// NewSpanID returns a fresh span ID, e.g. for a child span.
func NewSpanID() SpanID { return newSpanID() }

// Exporting reports whether spans of a sampled trace are sent anywhere.
func (t *Tracer) Exporting() bool { return t != nil && t.endpoint != "" }

// Record queues a finished span for export. Spans of unsampled traces and
// spans recorded without an endpoint are dropped.
func (t *Tracer) Record(c Context, s Span) {
	if !t.Exporting() || !c.Sampled {
		return
	}
	t.mu.Lock()
	if len(t.queue) < 10_000 { // never let a down collector eat memory
		t.queue = append(t.queue, s)
	}
	full := len(t.queue) >= 512
	t.mu.Unlock()
	if full {
		select {
		case t.kick <- struct{}{}:
		default:
		}
	}
}

// Run exports queued spans every interval (or when a batch fills) until ctx
// ends, then flushes what is left.
func (t *Tracer) Run(ctx context.Context, interval time.Duration) {
	if !t.Exporting() {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Flush(flushCtx)
			cancel()
			return
		case <-tick.C:
		case <-t.kick:
		}
		t.Flush(ctx)
	}
}

// Flush sends all queued spans now.
func (t *Tracer) Flush(ctx context.Context) {
	t.mu.Lock()
	spans := t.queue
	t.queue = nil
	t.mu.Unlock()
	if len(spans) == 0 {
		return
	}
	body, err := json.Marshal(t.encode(spans))
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		t.log.Warn("trace export failed", "spans", len(spans), "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.log.Warn("trace export rejected", "spans", len(spans), "status", resp.Status)
	}
}

// OTLP JSON encoding (opentelemetry-proto, ExportTraceServiceRequest).
type (
	otlpRequest struct {
		ResourceSpans []resourceSpans `json:"resourceSpans"`
	}
	resourceSpans struct {
		Resource   resource     `json:"resource"`
		ScopeSpans []scopeSpans `json:"scopeSpans"`
	}
	resource struct {
		Attributes []keyValue `json:"attributes"`
	}
	scopeSpans struct {
		Scope struct {
			Name string `json:"name"`
		} `json:"scope"`
		Spans []otlpSpan `json:"spans"`
	}
	otlpSpan struct {
		TraceID      string     `json:"traceId"`
		SpanID       string     `json:"spanId"`
		ParentSpanID string     `json:"parentSpanId,omitempty"`
		Name         string     `json:"name"`
		Kind         int        `json:"kind"` // 2 server, 3 client
		Start        string     `json:"startTimeUnixNano"`
		End          string     `json:"endTimeUnixNano"`
		Attributes   []keyValue `json:"attributes,omitempty"`
		Status       status     `json:"status"`
	}
	status struct {
		Code    int    `json:"code"` // 1 ok, 2 error
		Message string `json:"message,omitempty"`
	}
	keyValue struct {
		Key   string   `json:"key"`
		Value anyValue `json:"value"`
	}
	anyValue struct {
		String *string  `json:"stringValue,omitempty"`
		Int    *string  `json:"intValue,omitempty"` // int64 is a string in OTLP JSON
		Double *float64 `json:"doubleValue,omitempty"`
		Bool   *bool    `json:"boolValue,omitempty"`
	}
)

func attr(k string, v any) keyValue {
	kv := keyValue{Key: k}
	switch x := v.(type) {
	case string:
		kv.Value.String = &x
	case int:
		s := strconv.Itoa(x)
		kv.Value.Int = &s
	case int64:
		s := strconv.FormatInt(x, 10)
		kv.Value.Int = &s
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			x = 0
		}
		kv.Value.Double = &x
	case bool:
		kv.Value.Bool = &x
	default:
		s := fmt.Sprint(x)
		kv.Value.String = &s
	}
	return kv
}

func (t *Tracer) encode(spans []Span) otlpRequest {
	ss := scopeSpans{}
	ss.Scope.Name = "siliconrouter"
	for _, s := range spans {
		o := otlpSpan{
			TraceID: s.TraceID.String(), SpanID: s.SpanID.String(), Name: s.Name, Kind: 2,
			Start: strconv.FormatInt(s.Start.UnixNano(), 10), End: strconv.FormatInt(s.End.UnixNano(), 10),
			Status: status{Code: 1},
		}
		if s.Parent != (SpanID{}) {
			o.ParentSpanID = s.Parent.String()
		}
		if s.Client {
			o.Kind = 3
		}
		if s.Error != "" {
			o.Status = status{Code: 2, Message: s.Error}
		}
		for k, v := range s.Attrs {
			o.Attributes = append(o.Attributes, attr(k, v))
		}
		ss.Spans = append(ss.Spans, o)
	}
	return otlpRequest{ResourceSpans: []resourceSpans{{
		Resource:   resource{Attributes: []keyValue{attr("service.name", t.service)}},
		ScopeSpans: []scopeSpans{ss},
	}}}
}
