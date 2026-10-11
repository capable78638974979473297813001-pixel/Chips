package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/tracing"
)

func TestTracePropagationAndExport(t *testing.T) {
	var spans []map[string]any
	var mu sync.Mutex
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResourceSpans []struct {
				ScopeSpans []struct{ Spans []map[string]any }
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, rs := range body.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
	}))
	defer collector.Close()

	var upstream atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { upstream.Store(r.Header.Get("traceparent")) })
	ts, _ := setup(t, func(c *config.Config) { c.Tracing.OTLPEndpoint = collector.URL }, f.URL)
	srv := ts.Config.Handler.(*Server)
	srv.tracer = tracing.New(collector.URL, "siliconrouter", 1, collector.Client(), srv.log)

	const callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	chat(t, ts.URL, `{"model":"llama","messages":[]}`, "traceparent", "00-"+callerTrace+"-00f067aa0ba902b7-01")

	up, ok := tracing.Parse(upstream.Load().(string))
	if !ok || up.TraceID.String() != callerTrace || up.Parent.String() == "00f067aa0ba902b7" {
		t.Fatalf("backend got traceparent %q: want the caller's trace with the router's span as parent", upstream.Load())
	}
	srv.tracer.Flush(context.Background())
	mu.Lock()
	defer mu.Unlock()
	byName := map[string]map[string]any{}
	for _, s := range spans {
		byName[strings.SplitN(s["name"].(string), " ", 2)[0]] = s
	}
	root, upSpan := byName["POST"], byName["upstream"]
	if root == nil || upSpan == nil {
		t.Fatalf("spans = %v", spans)
	}
	if root["traceId"] != callerTrace || root["parentSpanId"] != "00f067aa0ba902b7" {
		t.Errorf("root span not joined to the caller: %v", root)
	}
	if upSpan["parentSpanId"] != root["spanId"] || upSpan["spanId"] != up.Parent.String() {
		t.Errorf("upstream span %v should be the root's child and the backend's parent", upSpan)
	}
}

func TestRequestLogSampling(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fail") {
			http.Error(w, "bad", 400)
		}
	})
	ts, _ := setup(t, func(c *config.Config) { c.Logging.RequestSampleRate = 0 }, f.URL)
	var buf bytes.Buffer
	var mu sync.Mutex
	ts.Config.Handler.(*Server).log = slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, nil))
	for range 5 {
		chat(t, ts.URL, `{}`)
	}
	post(t, ts.URL+"/v1/fail", `{}`)
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(buf.String(), "msg=request"); n != 1 || !strings.Contains(buf.String(), "status=400") {
		t.Errorf("logged %d request lines, want only the failed one:\n%s", n, buf.String())
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
