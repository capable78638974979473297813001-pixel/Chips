package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// loraFake is a vLLM-like backend with runtime adapter loading.
type loraFake struct {
	*fakeBackend
	loads  atomic.Int64
	fail   bool
	mu     sync.Mutex
	loaded map[string]bool
	served []string // models of the inference requests it answered
}

func newLoRAFake(t *testing.T, fail bool) *loraFake {
	l := &loraFake{loaded: map[string]bool{}, fail: fail}
	l.fakeBackend = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			LoRAName string `json:"lora_name"`
			LoRAPath string `json:"lora_path"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.URL.Path == "/v1/load_lora_adapter" {
			l.loads.Add(1)
			time.Sleep(20 * time.Millisecond)
			switch {
			case l.fail:
				http.Error(w, "adapter not found", http.StatusNotFound)
			case l.loaded[body.LoRAName]:
				http.Error(w, "The lora adapter '"+body.LoRAName+"' has already been loaded.", http.StatusBadRequest)
			case body.LoRAPath == "":
				http.Error(w, "missing path", http.StatusBadRequest)
			default:
				l.loaded[body.LoRAName] = true
				io.WriteString(w, "Success")
			}
			return
		}
		if body.Model != "llama" && !l.loaded[body.Model] {
			http.Error(w, `{"error":"model does not exist"}`, http.StatusNotFound)
			return
		}
		l.served = append(l.served, body.Model)
		io.WriteString(w, `{"choices":[]}`)
	})
	return l
}

func loraConfig(c *config.Config) {
	c.Models.Adapters = []config.Adapter{{Name: "support", Base: "llama", Path: "/loras/support"}}
	for i := range c.Backends {
		c.Backends[i].DynamicLoRA = true
	}
}

func TestLoadsAdapterOnDemandOnce(t *testing.T) {
	f := newLoRAFake(t, false)
	ts, _ := setup(t, loraConfig, f.URL)
	var wg sync.WaitGroup
	for range 3 { // concurrent first requests share one load
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"support","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	if resp := chat(t, ts.URL, `{"model":"support","messages":[{"role":"user","content":"again"}]}`); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n := f.loads.Load(); n != 1 {
		t.Errorf("adapter loaded %d times, want 1", n)
	}
	mresp, _ := http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if got := grepLines(string(body), `siliconrouter_lora_routing_total{result="loaded"}`); got == "" {
		t.Errorf("no loaded placements counted:\n%s", grepLines(string(body), "lora"))
	}
}

// Concurrent first requests for an adapter on several backends all go to
// the backend loading it, instead of each loading it on another backend. On
// real GPUs a burst like this used to load every adapter everywhere.
func TestConcurrentFirstRequestsShareOneBackend(t *testing.T) {
	a, b := newLoRAFake(t, false), newLoRAFake(t, false)
	ts, _ := setup(t, func(c *config.Config) {
		loraConfig(c)
		for i := range c.Backends { // room enough that load never outweighs the adapter
			c.Backends[i].MaxConcurrency = 16
		}
	}, a.URL, b.URL)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp := chat(t, ts.URL, `{"model":"support","messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != 200 {
				t.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	if n := a.loads.Load() + b.loads.Load(); n != 1 {
		t.Errorf("adapter loaded %d times (a %d, b %d), want once", n, a.loads.Load(), b.loads.Load())
	}
}

func TestAdapterLoadFailureTriesAnotherBackend(t *testing.T) {
	bad, good := newLoRAFake(t, true), newLoRAFake(t, false)
	ts, _ := setup(t, loraConfig, bad.URL, good.URL)
	resp := chat(t, ts.URL, `{"model":"support","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-SiliconRouter-Backend") != "b1" {
		t.Fatalf("status %d via %q, want 200 via b1", resp.StatusCode, resp.Header.Get("X-SiliconRouter-Backend"))
	}
	// The failed load must not count against the backend's health.
	if resp := chat(t, ts.URL, `{"model":"llama","messages":[{"role":"user","content":"base"}]}`); resp.StatusCode != 200 {
		t.Fatalf("base model status %d", resp.StatusCode)
	}
}

func TestAdapterLoadFailsEverywhere(t *testing.T) {
	ts, _ := setup(t, loraConfig, newLoRAFake(t, true).URL)
	resp := chat(t, ts.URL, `{"model":"support","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !json.Valid(body) || !strings.Contains(string(body), "load LoRA adapter support: 404") {
		t.Errorf("body %s", body)
	}
}
