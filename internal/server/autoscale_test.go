package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

func TestHoldsRequestWhileBackendStarts(t *testing.T) {
	var hooks atomic.Int64
	var hookBody atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hookBody.Store(string(b) + " auth=" + r.Header.Get("Authorization"))
		hooks.Add(1)
	}))
	t.Cleanup(hook.Close)
	t.Setenv("HOOK_SECRET", "s3cret")

	f := newFake(t, nil)
	ts, p := setup(t, func(c *config.Config) {
		c.Autoscaling.ColdStartWait = config.Duration(5 * time.Second)
		c.Autoscaling.Webhook.URL = hook.URL
		c.Autoscaling.Webhook.Secret = "s3cret"
		c.Backends[0].Models = []string{"llama"}
	}, f.URL)
	b := p.Backends()[0]
	p.Observe(b, false, false, "booting")

	done := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"llama","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				done <- 0
				return
			}
			resp.Body.Close()
			done <- resp.StatusCode
		}()
	}
	waitUntil(t, func() bool { return p.QueueDepth() == 2 })

	resp, err := http.Get(ts.URL + "/autoscale/llama")
	if err != nil {
		t.Fatal(err)
	}
	var d pool.ModelDemand
	json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if d.Held != 2 || d.ReadyBackends != 0 || d.DesiredBackends != 1 {
		t.Errorf("demand while booting = %+v", d)
	}
	waitUntil(t, func() bool { return hooks.Load() >= 1 })
	if body := hookBody.Load().(string); !strings.Contains(body, `"model":"llama"`) || !strings.Contains(body, "auth=Bearer s3cret") {
		t.Errorf("webhook got %s", body)
	}

	p.Observe(b, true, false, "")
	p.Observe(b, true, false, "")
	for range 2 {
		select {
		case code := <-done:
			if code != 200 {
				t.Errorf("held request answered %d", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("held request not served after the backend came up")
		}
	}
	if n := hooks.Load(); n != 1 {
		t.Errorf("webhook called %d times, want 1 (rate-limited per model)", n)
	}
}

func TestColdStartTimesOut(t *testing.T) {
	ts, p := setup(t, func(c *config.Config) {
		c.Autoscaling.ColdStartWait = config.Duration(50 * time.Millisecond)
		c.Autoscaling.ScaleToZeroModels = []string{"big"}
		c.Backends[0].Models = []string{"llama"}
	}, newFake(t, nil).URL)
	_ = p
	resp := chat(t, ts.URL, `{"model":"big","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "cold_start_timeout") || resp.Header.Get("Retry-After") == "" {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	models, _ := http.Get(ts.URL + "/v1/models")
	mb, _ := io.ReadAll(models.Body)
	models.Body.Close()
	if !strings.Contains(string(mb), `"big"`) {
		t.Errorf("scale-to-zero model missing from /v1/models: %s", mb)
	}
}
