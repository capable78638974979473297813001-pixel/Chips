package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// recorder is a backend that remembers the bodies it was sent.
type recorder struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (rec *recorder) handler(w http.ResponseWriter, r *http.Request) {
	var v map[string]any
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &v)
	rec.mu.Lock()
	rec.bodies = append(rec.bodies, v)
	rec.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
}

func (rec *recorder) last() map[string]any {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) == 0 {
		return nil
	}
	return rec.bodies[len(rec.bodies)-1]
}

func TestFallbackWhenModelIsDown(t *testing.T) {
	dead := newFake(t, nil)
	dead.Close() // connection refused
	var rec recorder
	small := newFake(t, rec.handler)
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].Models = []string{"big"}
		c.Backends[1].Models = []string{"small"}
		c.Models.Fallbacks = map[string][]string{"big": {"small"}}
	}, dead.URL, small.URL)

	resp := chat(t, ts.URL, `{"model":"big","messages":[]}`)
	if resp.StatusCode != 200 || resp.Header.Get(FallbackHeader) != "small" {
		t.Fatalf("status %d, fallback %q", resp.StatusCode, resp.Header.Get(FallbackHeader))
	}
	if m := rec.last()["model"]; m != "small" {
		t.Errorf("fallback backend was asked for model %v", m)
	}
	// A model with a working backend is served by it, with no fallback.
	if resp := chat(t, ts.URL, `{"model":"small"}`); resp.Header.Get(FallbackHeader) != "" {
		t.Error("fallback header on a direct hit")
	}
}

func TestFallbackWhenQueueWaitRunsOut(t *testing.T) {
	block := make(chan struct{})
	busy := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	var rec recorder
	small := newFake(t, rec.handler)
	ts, p := setup(t, func(c *config.Config) {
		c.Backends[0].Models = []string{"big"}
		c.Backends[0].MaxConcurrency = 1
		c.Backends[1].Models = []string{"small"}
		c.Models.Fallbacks = map[string][]string{"big": {"small"}}
		c.Queue.MaxWait = config.Duration(50 * time.Millisecond)
	}, busy.URL, small.URL)
	defer close(block)

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/chat/completions", jsonBody(`{"model":"big"}`))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	waitUntil(t, func() bool { return busy.hits.Load() == 1 && p.QueueDepth() == 0 })
	resp := chat(t, ts.URL, `{"model":"big"}`)
	if resp.StatusCode != 200 || resp.Header.Get(FallbackHeader) != "small" {
		t.Errorf("status %d, fallback %q", resp.StatusCode, resp.Header.Get(FallbackHeader))
	}
}

func TestFallbackRespectsKeyPolicy(t *testing.T) {
	dead := newFake(t, nil)
	dead.Close()
	small := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].Models = []string{"big"}
		c.Backends[1].Models = []string{"small"}
		c.Models.Fallbacks = map[string][]string{"big": {"small"}}
		c.Auth.Keys = []config.APIKey{{Name: "big-only", Key: "k", Models: []string{"big"}}}
	}, dead.URL, small.URL)
	resp := chat(t, ts.URL, `{"model":"big"}`, "Authorization", "Bearer k")
	if resp.StatusCode == 200 || small.hits.Load() != 0 {
		t.Errorf("key fell back to a model it may not use: status %d", resp.StatusCode)
	}
}

func TestKeyPolicy(t *testing.T) {
	var rec recorder
	f := newFake(t, rec.handler)
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{
			{Name: "limited", Key: "lim", Models: []string{"llama-*"}, MaxTokens: 1000},
			{Name: "open", Key: "open"},
		}
		c.Models.Aliases = map[string]string{"gpt-4o": "llama-70b", "other": "mistral"}
	}, f.URL)
	lim := []string{"Authorization", "Bearer lim"}

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"allowed model", "/v1/chat/completions", `{"model":"llama-8b","max_tokens":500}`, 200},
		{"alias to an allowed model", "/v1/chat/completions", `{"model":"gpt-4o"}`, 200},
		{"model not allowed", "/v1/chat/completions", `{"model":"mistral"}`, 403},
		{"alias to a model not allowed", "/v1/chat/completions", `{"model":"other"}`, 403},
		{"no model", "/v1/chat/completions", `{"messages":[]}`, 403},
		{"model in the path too", "/openai/deployments/mistral/chat/completions", `{"model":"llama-8b"}`, 403},
		{"escaped duplicate model", "/v1/chat/completions", `{"model":"llama-8b","model":"mistral"}`, 400},
		{"case-folded duplicate", "/v1/chat/completions", `{"model":"llama-8b","Model":"mistral"}`, 400},
		{"budget over the cap", "/v1/chat/completions", `{"model":"llama-8b","max_tokens":5000}`, 400},
		{"second budget field over the cap", "/v1/chat/completions", `{"model":"llama-8b","max_completion_tokens":10,"max_tokens":5000}`, 400},
		{"unlimited budget", "/api/generate", `{"model":"llama-8b","options":{"num_predict":-1}}`, 400},
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+tc.path, jsonBody(tc.body))
		req.Header.Set(lim[0], lim[1])
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}

	// A request with no budget gets the key's cap stated for it.
	chat(t, ts.URL, `{"model":"llama-8b","messages":[]}`, lim...)
	if got := rec.last()["max_tokens"]; got != float64(1000) {
		t.Errorf("max_tokens sent = %v, want the cap", got)
	}
	// Keys without a policy are untouched.
	chat(t, ts.URL, `{"model":"mistral","max_tokens":99999}`, "Authorization", "Bearer open")
	if got := rec.last(); got["model"] != "mistral" || got["max_tokens"] != float64(99999) {
		t.Errorf("open key's request changed: %v", got)
	}
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }
