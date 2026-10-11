package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func adminPost(t *testing.T, url string, headers ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
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

func TestDrainEndpoints(t *testing.T) {
	a, b := newFake(t, nil), newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) { c.Auth.AdminKeys = []string{"admin"} }, a.URL, b.URL)
	admin := []string{"Authorization", "Bearer admin"}

	if resp := adminPost(t, ts.URL+"/admin/backends/b0/drain"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("drain without admin key: %d", resp.StatusCode)
	}
	if resp := adminPost(t, ts.URL+"/admin/backends/missing/drain", admin...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown backend: %d", resp.StatusCode)
	}
	resp := adminPost(t, ts.URL+"/admin/backends/b0/drain", admin...)
	var st struct {
		Name    string `json:"name"`
		Drained bool   `json:"drained"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || resp.StatusCode != 200 || st.Name != "b0" || !st.Drained {
		t.Fatalf("drain: %d %+v %v", resp.StatusCode, st, err)
	}
	for range 5 {
		if r := chat(t, ts.URL, `{"model":"llama"}`); r.Header.Get("X-SiliconRouter-Backend") != "b1" {
			t.Errorf("request went to %q while b0 is drained", r.Header.Get("X-SiliconRouter-Backend"))
		}
	}
	if a.hits.Load() != 0 {
		t.Errorf("drained backend served %d requests", a.hits.Load())
	}

	adminPost(t, ts.URL+"/admin/backends/b1/drain", admin...)
	if code := get(t, ts.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz with everything drained: %d", code)
	}
	if r := chat(t, ts.URL, `{"model":"llama"}`); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("inference with everything drained: %d", r.StatusCode)
	}
	adminPost(t, ts.URL+"/admin/backends/b0/undrain", admin...)
	if code := get(t, ts.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("readyz after undrain: %d", code)
	}
	if r := chat(t, ts.URL, `{"model":"llama"}`); r.Header.Get("X-SiliconRouter-Backend") != "b0" {
		t.Errorf("after undrain: %d from %q", r.StatusCode, r.Header.Get("X-SiliconRouter-Backend"))
	}
}

func TestHighPriorityShedsQueuedBatchWork(t *testing.T) {
	block := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	ts, p := setup(t, func(c *config.Config) {
		c.Backends[0].MaxConcurrency = 1
		c.Queue.MaxWaiting = 1
		c.Queue.PriorityHeader = true
	}, f.URL)
	defer close(block)

	send := func(ctx context.Context, prio string) chan int {
		code := make(chan int, 1)
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{}`))
			req.Header.Set("X-SiliconRouter-Priority", prio)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				code <- 0
				return
			}
			resp.Body.Close()
			code <- resp.StatusCode
		}()
		return code
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	send(ctx, "low") // takes the only slot
	waitUntil(t, func() bool { return p.QueueDepth() == 0 && f.hits.Load() == 1 })
	batch := send(ctx, "low") // waits in the only queue place
	waitUntil(t, func() bool { return p.QueueDepth() == 1 })
	send(ctx, "high") // displaces it

	select {
	case code := <-batch:
		if code != http.StatusTooManyRequests {
			t.Errorf("shed request got %d, want 429", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("low-priority request was not shed")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
