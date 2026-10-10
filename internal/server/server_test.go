package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// fakeBackend is a minimal OpenAI-style upstream that records what it saw.
type fakeBackend struct {
	*httptest.Server
	hits     atomic.Int64
	lastAuth atomic.Value
	handler  http.HandlerFunc
}

func newFake(t testing.TB, h http.HandlerFunc) *fakeBackend {
	f := &fakeBackend{handler: h}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"llama"}]}`)
			return
		}
		f.hits.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func setup(t testing.TB, mutate func(*config.Config), urls ...string) (*httptest.Server, *pool.Pool) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("backends:\n")
	for i, u := range urls {
		fmt.Fprintf(&sb, "  - {name: b%d, url: %q, max_concurrency: 4}\n", i, u)
	}
	cfg, err := config.Parse([]byte(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routing.BlockChars = 16
	if mutate != nil {
		mutate(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pool.New(cfg.Backends, pool.Options{
		MaxWaiting: cfg.Queue.MaxWaiting, AffinityMaxImbalance: cfg.Routing.AffinityMaxImbalance,
		LargeRequestTokens: cfg.Routing.LargeRequestTokens, UnhealthyThreshold: cfg.Health.UnhealthyThreshold,
		HealthyThreshold: cfg.Health.HealthyThreshold, PassiveHealth: true,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	ts := httptest.NewServer(New(cfg, p, client, log))
	t.Cleanup(ts.Close)
	return ts, p
}

func chat(t *testing.T, base string, body string, headers ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func conversation(turns int) string {
	var msgs []string
	for i := range turns {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":"question number %d with plenty of text"}`, i))
		msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":"answer number %d"}`, i))
	}
	msgs = msgs[:len(msgs)-1]
	return `{"model":"llama","messages":[` + strings.Join(msgs, ",") + `]}`
}

func TestProxiesAndTagsResponse(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, nil, f.URL)
	resp := chat(t, ts.URL, `{"model":"llama","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-SiliconRouter-Backend") != "b0" {
		t.Errorf("backend header = %q", resp.Header.Get("X-SiliconRouter-Backend"))
	}
}

func TestConversationSticksToOneBackend(t *testing.T) {
	a, b := newFake(t, nil), newFake(t, nil)
	ts, _ := setup(t, nil, a.URL, b.URL)
	first := chat(t, ts.URL, conversation(1)).Header.Get("X-SiliconRouter-Backend")
	for turns := 2; turns <= 6; turns++ {
		resp := chat(t, ts.URL, conversation(turns))
		if got := resp.Header.Get("X-SiliconRouter-Backend"); got != first {
			t.Fatalf("turn %d went to %s, conversation started on %s", turns, got, first)
		}
		if resp.Header.Get("X-SiliconRouter-Prefix-Cache") != "hit" {
			t.Errorf("turn %d not reported as a prefix cache hit", turns)
		}
	}
}

func TestSessionHeaderPinsBackend(t *testing.T) {
	a, b := newFake(t, nil), newFake(t, nil)
	ts, _ := setup(t, nil, a.URL, b.URL)
	first := chat(t, ts.URL, `{"messages":[{"role":"user","content":"x"}]}`, SessionHeader, "s1").Header.Get("X-SiliconRouter-Backend")
	for i := range 5 {
		body := fmt.Sprintf(`{"messages":[{"role":"user","content":"unrelated %d"}]}`, i)
		if got := chat(t, ts.URL, body, SessionHeader, "s1").Header.Get("X-SiliconRouter-Backend"); got != first {
			t.Fatalf("session moved from %s to %s", first, got)
		}
	}
}

func TestFailsOverOnDeadAndOverloadedBackends(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused
	busy := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	good := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) { c.Retry.MaxAttempts = 3 }, deadURL, busy.URL, good.URL)

	resp := chat(t, ts.URL, `{"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-SiliconRouter-Backend") != "b2" {
		t.Fatalf("status %d via %s", resp.StatusCode, resp.Header.Get("X-SiliconRouter-Backend"))
	}
}

func TestRelaysLastErrorWhenAttemptsRunOut(t *testing.T) {
	busy := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	ts, _ := setup(t, func(c *config.Config) { c.Retry.MaxAttempts = 1 }, busy.URL)
	if resp := chat(t, ts.URL, `{}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: [DONE]\n\n")
	})
	ts, _ := setup(t, nil, f.URL)
	resp := chat(t, ts.URL, `{"stream":true}`)
	defer close(release)

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case got := <-line:
		if got != "data: first\n" {
			t.Errorf("first line = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event was buffered by the proxy")
	}
}

func TestOverloadReturns429(t *testing.T) {
	block := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	ts, p := setup(t, func(c *config.Config) {
		c.Backends[0].MaxConcurrency = 1
		c.Queue.MaxWaiting = 1
	}, f.URL)
	defer close(block)

	for range 2 { // one in flight, one queued
		go func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{}`))
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.QueueDepth() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	resp := chat(t, ts.URL, `{}`)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("status = %d, Retry-After = %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestQueueTimeout(t *testing.T) {
	block := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].MaxConcurrency = 1
		c.Queue.MaxWait = config.Duration(50 * time.Millisecond)
	}, f.URL)
	defer close(block)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if resp := chat(t, ts.URL, `{}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) { c.Auth.APIKeys = []string{"router-key"} }, f.URL)
	if resp := chat(t, ts.URL, `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status %d", resp.StatusCode)
	}
	if resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong key: status %d", resp.StatusCode)
	}
	if resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer router-key"); resp.StatusCode != 200 {
		t.Errorf("good key: status %d", resp.StatusCode)
	}
	if got := f.lastAuth.Load(); got != "" {
		t.Errorf("router key leaked upstream: %q", got)
	}
}

func TestBackendKeyIsSent(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) { c.Backends[0].APIKey = "gpu-key" }, f.URL)
	chat(t, ts.URL, `{}`, "Authorization", "Bearer client")
	if got := f.lastAuth.Load(); got != "Bearer gpu-key" {
		t.Errorf("upstream auth = %q", got)
	}
}

func TestRejectsOversizedBody(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) { c.MaxBodyBytes = 64 }, f.URL)
	if resp := chat(t, ts.URL, `{"x":"`+strings.Repeat("a", 100)+`"}`); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("big body: %d", resp.StatusCode)
	}
	if f.hits.Load() != 0 {
		t.Error("oversized request reached the backend")
	}
}

// Bodies the router cannot read (audio, images, binary tensors) are not its
// business to reject: they are forwarded byte for byte.
func TestForwardsNonJSONBodies(t *testing.T) {
	var got atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(r.Method + " " + r.URL.Path + " " + r.Header.Get("Content-Type") + " " + string(b))
	})
	ts, _ := setup(t, nil, f.URL)
	resp, err := http.Post(ts.URL+"/v2/models/yolo/infer", "application/octet-stream", strings.NewReader("\x00raw"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %v err %v", resp.Status, err)
	}
	resp.Body.Close()
	if want := "POST /v2/models/yolo/infer application/octet-stream \x00raw"; got.Load() != want {
		t.Errorf("backend saw %q, want %q", got.Load(), want)
	}
}

// Native, non-OpenAI APIs are proxied too, and a model named in the URL or
// body steers the request to a backend that serves it.
func TestRoutesNativeAPIsByModel(t *testing.T) {
	resnet := newFake(t, nil)
	llama := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].Models = []string{"resnet50"}
		c.Backends[1].Models = []string{"llama3"}
	}, resnet.URL, llama.URL)
	for i := range 4 {
		post := func(path, body string) {
			resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%s: %v %v", path, resp.Status, err)
			}
			resp.Body.Close()
		}
		post("/v2/models/resnet50/infer", `{"inputs":[]}`)                                                  // Triton/KServe
		post("/api/chat", fmt.Sprintf(`{"model":"llama3","messages":[{"role":"user","content":"%d"}]}`, i)) // Ollama
		post("/v1/messages", `{"model":"llama3","max_tokens":5,"messages":[]}`)                             // Anthropic-style
	}
	if resnet.hits.Load() != 4 || llama.hits.Load() != 8 {
		t.Errorf("resnet got %d, llama got %d; want 4 and 8", resnet.hits.Load(), llama.hits.Load())
	}
}

func TestAlternativeKeyHeaders(t *testing.T) {
	var seen atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("X-Api-Key") + "|" + r.Header.Get("Api-Key") + "|" + r.Header.Get("Authorization"))
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.APIKeys = []string{"router-key"}
		c.Backends[0].APIKey = "upstream-key"
		c.Backends[0].APIKeyHeader = "x-api-key"
	}, f.URL)
	// Anthropic SDKs send x-api-key; Azure OpenAI SDKs send api-key.
	for _, h := range []string{"X-Api-Key", "Api-Key"} {
		if resp := chat(t, ts.URL, `{}`, h, "router-key"); resp.StatusCode != 200 {
			t.Errorf("%s: status %d", h, resp.StatusCode)
		}
		if got := seen.Load(); got != "upstream-key||" {
			t.Errorf("%s: upstream saw %q; want only the backend key in x-api-key", h, got)
		}
	}
	if resp := chat(t, ts.URL, `{}`, "X-Api-Key", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong x-api-key: status %d", resp.StatusCode)
	}
}

func TestPerBackendHealthPath(t *testing.T) {
	var paths atomic.Value
	paths.Store("")
	tgi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.Store(paths.Load().(string) + r.URL.Path + " ")
		if r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"model_id":"mistral-7b"}`)
	}))
	defer tgi.Close()
	_, p := setup(t, func(c *config.Config) { c.Backends[0].HealthPath = "/info" }, tgi.URL)
	(&pool.Checker{Pool: p, Client: http.DefaultClient, Path: "/v1/models", Timeout: time.Second}).CheckAll(context.Background())
	st := p.Snapshot()[0]
	if !st.Healthy || len(st.Models) != 1 || st.Models[0] != "mistral-7b" || paths.Load() != "/info " {
		t.Errorf("status %+v, paths %q", st, paths.Load())
	}
}

func TestModelsAndAdminEndpoints(t *testing.T) {
	f := newFake(t, nil)
	ts, p := setup(t, nil, f.URL)
	(&pool.Checker{Pool: p, Client: http.DefaultClient, Path: "/v1/models", Timeout: time.Second}).CheckAll(context.Background())

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var models struct{ Data []struct{ ID string } }
	json.NewDecoder(resp.Body).Decode(&models)
	if len(models.Data) != 1 || models.Data[0].ID != "llama" {
		t.Errorf("models = %+v", models)
	}

	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/admin/backends"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: %v %v", path, err, resp.Status)
		}
		resp.Body.Close()
	}
}
