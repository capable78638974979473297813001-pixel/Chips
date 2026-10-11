package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// vllmFake answers inference like newFake and serves vLLM metrics.
func vllmFake(t *testing.T, waiting int) *fakeBackend {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			io.WriteString(w, "vllm:num_requests_running{model_name=\"llama\"} 1\n")
			io.WriteString(w, "vllm:num_requests_waiting{model_name=\"llama\"} "+strconv.Itoa(waiting)+"\n")
			io.WriteString(w, "vllm:kv_cache_usage_perc{model_name=\"llama\"} 0.4\n")
			return
		}
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
}

func TestRoutesAroundQueueingEngine(t *testing.T) {
	busy, idle := vllmFake(t, 1), vllmFake(t, 0)
	ts, p := setup(t, func(c *config.Config) {
		c.Backends[0].MetricsPath = "/metrics"
		c.Backends[1].MetricsPath = "/metrics"
	}, busy.URL, idle.URL)
	scraper := &pool.EngineScraper{Pool: p, Client: http.DefaultClient, Interval: time.Hour, Timeout: time.Second}
	scraper.ScrapeAll(context.Background())

	for range 5 {
		if got := chat(t, ts.URL, `{"model":"llama"}`).Header.Get("X-SiliconRouter-Backend"); got != "b1" {
			t.Errorf("request went to %s, whose engine is queueing", got)
		}
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`siliconrouter_backend_engine_waiting{backend="b0"} 1`,
		`siliconrouter_backend_engine_kv_cache_usage{backend="b1"} 0.4`,
		`siliconrouter_backend_engine_saturated{backend="b0"} 1`,
		`siliconrouter_backend_engine_saturated{backend="b1"} 0`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
