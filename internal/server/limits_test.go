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
	"github.com/capable78638974979473297813001-pixel/chips/internal/limits"
)

func usageOf(t *testing.T, url, client string, headers ...string) limits.Usage {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/admin/usage", nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Clients []limits.Usage }
	json.NewDecoder(resp.Body).Decode(&out)
	for _, u := range out.Clients {
		if u.Client == client {
			return u
		}
	}
	t.Fatalf("no usage for %q in %+v", client, out.Clients)
	return limits.Usage{}
}

func TestRequestsPerMinuteLimit(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "batch", Key: "k", RequestsPerMinute: 3}}
	}, f.URL)
	for i := range 3 {
		if resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer k"); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer k")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.Contains(string(body), "rate_limited") {
		t.Errorf("4th request: %d %q retry-after %q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if f.hits.Load() != 3 {
		t.Errorf("backend saw %d requests, want 3", f.hits.Load())
	}
}

func TestConcurrencyLimitPerKey(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{
			{Name: "greedy", Key: "g", MaxConcurrent: 1},
			{Name: "other", Key: "o"},
		}
	}, f.URL)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); chat(t, ts.URL, `{}`, "Authorization", "Bearer g") }()
	for f.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer g"); resp.StatusCode != 429 {
		t.Errorf("second concurrent request from greedy: %d, want 429", resp.StatusCode)
	}
	// Another team is unaffected.
	done := make(chan int)
	go func() { done <- chat(t, ts.URL, `{}`, "Authorization", "Bearer o").StatusCode }()
	for f.hits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Errorf("other key: %d", code)
	}
	wg.Wait()
}

func TestDailyQuotaFromReportedUsage(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":60,"completion_tokens":50}}`)
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k", TokensPerDay: 100}}
	}, f.URL)
	if resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer k"); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	resp := chat(t, ts.URL, `{}`, "Authorization", "Bearer k")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || !strings.Contains(string(body), "quota_exceeded") {
		t.Errorf("over quota: %d %s", resp.StatusCode, body)
	}
	u := usageOf(t, ts.URL, "team", "Authorization", "Bearer k")
	if u.PromptTokens != 60 || u.CompletionTokens != 50 || u.EstimatedTokens != 0 || u.TokensToday != 110 || u.Requests != 1 || u.Rejected != 1 {
		t.Errorf("usage = %+v", u)
	}
}

func TestUsageFromStreamsAndEstimates(t *testing.T) {
	var mode string
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		m := mode
		mu.Unlock()
		switch m {
		case "stream-with-usage":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
		case "stream-without-usage":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"t\":\"a\"}\n\ndata: {\"t\":\"b\"}\n\ndata: {\"t\":\"c\"}\n\ndata: [DONE]\n\n")
		case "ollama":
			w.Header().Set("Content-Type", "application/x-ndjson")
			io.WriteString(w, "{\"response\":\"a\"}\n{\"done\":true,\"prompt_eval_count\":11,\"eval_count\":4}\n")
		}
	})
	ts, _ := setup(t, nil, f.URL)
	run := func(m, body string) limits.Usage {
		mu.Lock()
		mode = m
		mu.Unlock()
		resp := chat(t, ts.URL, body)
		io.ReadAll(resp.Body)
		return usageOf(t, ts.URL, "anonymous")
	}
	u := run("stream-with-usage", `{"stream":true}`)
	if u.PromptTokens != 7 || u.CompletionTokens != 2 || u.EstimatedTokens != 0 {
		t.Errorf("reported stream usage: %+v", u)
	}
	// 3 data events (plus [DONE]); the prompt is estimated from its text.
	u = run("stream-without-usage", `{"stream":true,"messages":[{"role":"user","content":"`+strings.Repeat("x", 40)+`"}]}`)
	if u.CompletionTokens != 2+3 || u.EstimatedTokens == 0 {
		t.Errorf("estimated stream usage: %+v", u)
	}
	u = run("ollama", `{"stream":true}`)
	if u.CompletionTokens != 2+3+4 {
		t.Errorf("ollama usage: %+v", u)
	}
}
