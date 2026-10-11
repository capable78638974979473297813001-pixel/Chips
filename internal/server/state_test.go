package server

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func TestIdenticalRequestsShareOneBackendCall(t *testing.T) {
	var calls atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond) // a slow generation, so the others arrive meanwhile
		io.WriteString(w, `{"answer":42}`)
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Cache.Enabled = true
		c.Backends[0].MaxConcurrency = 64
	}, f.URL)
	body := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"same question"}]}`
	var wg sync.WaitGroup
	var mu sync.Mutex
	kinds := map[string]int{}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, b := post(t, ts.URL+"/v1/chat/completions", body)
			if b != `{"answer":42}` {
				t.Errorf("body %q", b)
			}
			mu.Lock()
			kinds[r.Header.Get("X-SiliconRouter-Cache")]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("backend called %d times for 10 identical requests, want 1", calls.Load())
	}
	if kinds["miss"] != 1 || kinds["hit"] != 9 {
		t.Errorf("cache results = %v, want 1 miss and 9 hits", kinds)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"usage":{"prompt_tokens":70,"completion_tokens":20}}`)
	})
	mutate := func(c *config.Config) {
		c.Cache.Enabled = true
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k", TokensPerDay: 100}}
	}
	auth := []string{"Authorization", "Bearer k"}

	before, _ := setup(t, mutate, f.URL)
	post(t, before.URL+"/v1/embeddings", `{"input":"x"}`, auth...) // 90 tokens, cached
	if err := before.Config.Handler.(*Server).SaveState(dir); err != nil {
		t.Fatal(err)
	}

	after, _ := setup(t, mutate, f.URL) // a fresh process
	if n, err := after.Config.Handler.(*Server).LoadState(dir); err != nil || n != 1 {
		t.Fatalf("LoadState: %d %v", n, err)
	}
	r, _ := post(t, after.URL+"/v1/embeddings", `{"input":"x"}`, auth...)
	if r.Header.Get("X-SiliconRouter-Cache") != "hit" || calls.Load() != 1 {
		t.Errorf("cache not restored: %q, backend calls %d", r.Header.Get("X-SiliconRouter-Cache"), calls.Load())
	}
	post(t, after.URL+"/v1/embeddings", `{"input":"y"}`, auth...) // 90 more: 180 of 100
	if r, _ := post(t, after.URL+"/v1/embeddings", `{"input":"z"}`, auth...); r.StatusCode != 429 {
		t.Errorf("quota reset by the restart: %d", r.StatusCode)
	}
	if u := usageOf(t, after.URL, "team", auth...); u.TokensToday != 180 || u.Requests != 3 || u.Rejected != 1 {
		t.Errorf("usage after restart = %+v", u)
	}
}
