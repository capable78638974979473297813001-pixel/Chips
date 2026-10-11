package tracing

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseFormat(t *testing.T) {
	h := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	c, ok := Parse(h)
	if !ok || !c.Sampled || c.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" || c.Parent.String() != "00f067aa0ba902b7" {
		t.Fatalf("parse = %+v %v", c, ok)
	}
	if got := Format(c.TraceID, c.Parent, true); got != h {
		t.Errorf("format = %s", got)
	}
	for _, bad := range []string{"", "garbage", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4bf92f3577b34da6a3ce929d0e0e47zz-00f067aa0ba902b7-01"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestBeginJoinsOrStarts(t *testing.T) {
	tr := New("", "svc", 1, nil, nil)
	h := http.Header{}
	h.Set(Header, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	c, span := tr.Begin(h)
	if c.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" || span == (SpanID{}) || span == c.Parent {
		t.Errorf("join = %+v %v", c, span)
	}
	c2, _ := tr.Begin(http.Header{})
	if c2.TraceID == (TraceID{}) || c2.Sampled {
		t.Errorf("new trace without an exporter = %+v (should exist, unsampled)", c2)
	}
}

func TestExportsOTLPJSON(t *testing.T) {
	got := make(chan map[string]any, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("collector got %s %s", r.URL.Path, r.Header.Get("Content-Type"))
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		got <- body
	}))
	defer collector.Close()
	tr := New(collector.URL, "siliconrouter", 1, collector.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c, root := tr.Begin(http.Header{})
	if !c.Sampled {
		t.Fatal("sample rate 1 did not sample")
	}
	start := time.Unix(100, 0)
	tr.Record(c, Span{TraceID: c.TraceID, SpanID: root, Name: "POST /v1/chat/completions", Start: start, End: start.Add(time.Second),
		Attrs: map[string]any{"llm.model": "llama", "http.status_code": 200, "cache.hit": false}})
	tr.Record(c, Span{TraceID: c.TraceID, SpanID: NewSpanID(), Parent: root, Name: "upstream gpu1", Client: true, Start: start, End: start.Add(time.Second), Error: "boom"})
	tr.Record(Context{TraceID: c.TraceID}, Span{Name: "unsampled"})
	tr.Flush(context.Background())

	body := <-got
	rs := body["resourceSpans"].([]any)[0].(map[string]any)
	if svc := rs["resource"].(map[string]any)["attributes"].([]any)[0].(map[string]any); svc["key"] != "service.name" {
		t.Errorf("resource = %v", svc)
	}
	spans := rs["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2 (unsampled dropped)", len(spans))
	}
	s0, s1 := spans[0].(map[string]any), spans[1].(map[string]any)
	if s0["traceId"] != c.TraceID.String() || s0["kind"] != 2.0 || s0["startTimeUnixNano"] != "100000000000" || s0["endTimeUnixNano"] != "101000000000" {
		t.Errorf("server span = %v", s0)
	}
	if s1["parentSpanId"] != root.String() || s1["kind"] != 3.0 || s1["status"].(map[string]any)["code"] != 2.0 {
		t.Errorf("client span = %v", s1)
	}
}
