package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

const streamBody = `{"model":"llama","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func stalling(stallBeforeHeaders bool, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !stallBeforeHeaders {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: late\n\n")
	}
}

func streaming(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, "data: fast\n\ndata: [DONE]\n\n")
}

func TestFirstTokenTimeoutFailsOver(t *testing.T) {
	for _, beforeHeaders := range []bool{false, true} {
		release := make(chan struct{})
		stuck := newFake(t, stalling(beforeHeaders, release))
		good := newFake(t, streaming)
		ts, p := setup(t, func(c *config.Config) {
			c.Retry.FirstTokenTimeout = config.Duration(150 * time.Millisecond)
		}, stuck.URL, good.URL)

		start := time.Now()
		resp := chat(t, ts.URL, streamBody)
		body, _ := io.ReadAll(resp.Body)
		close(release)
		if resp.StatusCode != 200 || string(body) != "data: fast\n\ndata: [DONE]\n\n" {
			t.Fatalf("beforeHeaders=%v: status %d body %q", beforeHeaders, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-SiliconRouter-Backend"); got != "b1" {
			t.Errorf("beforeHeaders=%v: answered by %s, want b1", beforeHeaders, got)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("beforeHeaders=%v: took %v", beforeHeaders, d)
		}
		// The stalled backend's average now reflects the timeout.
		if ms := p.Snapshot()[0].TTFTMillis; ms != 150 {
			t.Errorf("beforeHeaders=%v: stuck backend ttft_ms = %v, want 150", beforeHeaders, ms)
		}
	}
}

func TestLastAttemptWaitsInsteadOfFailing(t *testing.T) {
	slow := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		streaming(w, r)
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Retry.FirstTokenTimeout = config.Duration(50 * time.Millisecond)
		c.Retry.MaxAttempts = 1
	}, slow.URL)
	resp := chat(t, ts.URL, streamBody)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "fast") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

func post(t *testing.T, url, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestResponseCache(t *testing.T) {
	var n atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"n":%d}`, n.Add(1))
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Cache.Enabled = true
		c.Auth.APIKeys = []string{"key-a", "key-b"}
	}, f.URL)
	auth := func(k string) []string { return []string{"Authorization", "Bearer " + k} }

	cases := []struct {
		name, path, body string
		headers          []string
		wantCache        string // X-SiliconRouter-Cache on the second identical request
	}{
		{"embeddings", "/v1/embeddings", `{"model":"e","input":"x"}`, nil, "hit"},
		{"temperature 0", "/v1/chat/completions", `{"model":"m","temperature":0,"messages":[]}`, nil, "hit"},
		{"sampled", "/v1/chat/completions", `{"model":"m","temperature":0.8,"messages":[]}`, nil, ""},
		{"no temperature", "/v1/chat/completions", `{"model":"m","messages":[]}`, nil, ""},
		{"client opts out", "/v1/embeddings", `{"model":"e","input":"y"}`, []string{"Cache-Control", "no-cache"}, ""},
	}
	for _, c := range cases {
		h := append(auth("key-a"), c.headers...)
		r1, b1 := post(t, ts.URL+c.path, c.body, h...)
		r2, b2 := post(t, ts.URL+c.path, c.body, h...)
		if got := r2.Header.Get("X-SiliconRouter-Cache"); got != c.wantCache {
			t.Errorf("%s: second request cache header %q, want %q", c.name, got, c.wantCache)
		}
		if c.wantCache == "hit" {
			if b1 != b2 || r1.Header.Get("X-SiliconRouter-Cache") != "miss" || r2.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s: replay differs: %q vs %q, headers %v", c.name, b1, b2, r2.Header)
			}
		} else if b1 == b2 {
			t.Errorf("%s: answered from cache although not cacheable", c.name)
		}
	}

	// Another tenant's key does not see key-a's cached answer.
	before := n.Load()
	if r, _ := post(t, ts.URL+"/v1/embeddings", `{"model":"e","input":"x"}`, auth("key-b")...); r.Header.Get("X-SiliconRouter-Cache") != "miss" || n.Load() != before+1 {
		t.Errorf("key-b got key-a's cached response")
	}
}

func TestStreamsAreCachedWhole(t *testing.T) {
	var n atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		streaming(w, r)
	})
	ts, _ := setup(t, func(c *config.Config) { c.Cache.Enabled = true }, f.URL)
	body := `{"model":"m","stream":true,"temperature":0,"messages":[]}`
	_, b1 := post(t, ts.URL+"/v1/chat/completions", body)
	r2, b2 := post(t, ts.URL+"/v1/chat/completions", body)
	if b1 != b2 || r2.Header.Get("X-SiliconRouter-Cache") != "hit" || n.Load() != 1 {
		t.Errorf("stream replay: %q vs %q, header %q, backend calls %d", b1, b2, r2.Header.Get("X-SiliconRouter-Cache"), n.Load())
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	var n atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, "bad input", http.StatusBadRequest)
	})
	ts, _ := setup(t, func(c *config.Config) { c.Cache.Enabled = true }, f.URL)
	post(t, ts.URL+"/v1/embeddings", `{"input":"x"}`)
	post(t, ts.URL+"/v1/embeddings", `{"input":"x"}`)
	if n.Load() != 2 {
		t.Errorf("a 400 answer was cached")
	}
}

func TestModelAliases(t *testing.T) {
	var seen atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.Store(r.URL.Path + " " + string(b))
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].Models = []string{"llama-3.1-70b", "resnet50"}
		c.Models.Aliases = map[string]string{"gpt-4o": "llama-3.1-70b", "vision": "resnet50", "ghost": "missing"}
	}, f.URL)

	post(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`)
	if got := seen.Load(); got != `/v1/chat/completions {"model":"llama-3.1-70b","messages":[]}` {
		t.Errorf("body alias: backend saw %q", got)
	}
	post(t, ts.URL+"/v2/models/vision/infer", `{"inputs":[]}`)
	if got := seen.Load(); got != `/v2/models/resnet50/infer {"inputs":[]}` {
		t.Errorf("path alias: backend saw %q", got)
	}

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`"gpt-4o"`, `"vision"`, `"llama-3.1-70b"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("/v1/models missing %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), "ghost") {
		t.Errorf("alias to an unserved model is listed: %s", b)
	}
}

func TestNamedKeysAndPriorityHeader(t *testing.T) {
	var seen atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get(PriorityHeader) + "|" + r.Header.Get("Authorization"))
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "chat-app", Key: "k1", Priority: "high"}}
		c.Queue.PriorityHeader = true
	}, f.URL)
	if r, _ := post(t, ts.URL+"/v1/chat/completions", `{}`, "Authorization", "Bearer k1", PriorityHeader, "low"); r.StatusCode != 200 {
		t.Fatalf("named key rejected: %d", r.StatusCode)
	}
	if got := seen.Load(); got != "|" {
		t.Errorf("priority header or client key leaked upstream: %q", got)
	}
	if r, _ := post(t, ts.URL+"/v1/chat/completions", `{}`, "Authorization", "Bearer nope"); r.StatusCode != 401 {
		t.Errorf("unknown key accepted: %d", r.StatusCode)
	}
}

func TestOversizedResponsesAreNotCached(t *testing.T) {
	var n atomic.Int64
	big := strings.Repeat("x", 2000)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		io.WriteString(w, big)
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Cache.Enabled = true
		c.Cache.MaxEntryBytes = 1000
	}, f.URL)
	_, b1 := post(t, ts.URL+"/v1/embeddings", `{"input":"x"}`)
	r2, b2 := post(t, ts.URL+"/v1/embeddings", `{"input":"x"}`)
	if b1 != big || b2 != big || n.Load() != 2 || r2.Header.Get("X-SiliconRouter-Cache") == "hit" {
		t.Errorf("oversized answer cached: second body %d bytes, backend calls %d, header %q",
			len(b2), n.Load(), r2.Header.Get("X-SiliconRouter-Cache"))
	}
}
