package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

const vllm = `# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 12.0
vllm:num_requests_running{engine="1",model_name="meta-llama/Llama-3.1-8B-Instruct"} 9.0
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 3.0
vllm:num_requests_waiting{engine="1",model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.0
# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.82
vllm:kv_cache_usage_perc{engine="1",model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.4
vllm:prompt_tokens_total{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 123456.0
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{engine="0",model_name="meta-llama/Llama-3.1-8B-Instruct"} 5000.0
vllm:generation_tokens_total{engine="1",model_name="meta-llama/Llama-3.1-8B-Instruct"} 1200.0
vllm:e2e_request_latency_seconds_bucket{engine="0",le="0.3",model_name="m"} 4.0
`

const sglang = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{model_name="Qwen/Qwen2.5-7B"} 31.0
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{model_name="Qwen/Qwen2.5-7B"} 0.0
# TYPE sglang:token_usage gauge
sglang:token_usage{model_name="Qwen/Qwen2.5-7B"} 0.27
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total{model_name="Qwen/Qwen2.5-7B"} 777.0
`

const tgi = `# TYPE tgi_queue_size gauge
tgi_queue_size 5
# TYPE tgi_batch_current_size gauge
tgi_batch_current_size 32
tgi_request_count 1000
`

const llamacpp = `# HELP llamacpp:kv_cache_usage_ratio KV-cache usage. 1 means 100 percent usage.
# TYPE llamacpp:kv_cache_usage_ratio gauge
llamacpp:kv_cache_usage_ratio 0.125
llamacpp:requests_processing 2
llamacpp:requests_deferred 1
`

const triton = `nv_inference_pending_request_count{model="resnet",version="1"} 4
nv_inference_pending_request_count{model="bert",version="2"} 1
`

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want Stats
	}{
		"vllm":     {vllm, Stats{Engine: "vllm", Running: 21, Waiting: 3, KVUsage: 0.82, Generated: 6200}},
		"sglang":   {sglang, Stats{Engine: "sglang", Running: 31, Waiting: 0, KVUsage: 0.27, Generated: 777}},
		"tgi":      {tgi, Stats{Engine: "tgi", Running: 32, Waiting: 5, KVUsage: -1, Generated: -1}},
		"llamacpp": {llamacpp, Stats{Engine: "llamacpp", Running: 2, Waiting: 1, KVUsage: 0.125, Generated: -1}},
		"triton":   {triton, Stats{Engine: "triton", Running: -1, Waiting: 5, KVUsage: -1, Generated: -1}},
		"vllm v0 name, timestamps and odd labels": {
			"vllm:gpu_cache_usage_perc{model_name=\"a } b\",x=\"q\\\"}\"} 0.5 1712345678000\nvllm:num_requests_waiting 2\n",
			Stats{Engine: "vllm", Running: -1, Waiting: 2, KVUsage: 0.5, Generated: -1}},
		"bad values ignored": {"vllm:num_requests_waiting NaN\nvllm:num_requests_running -3\nvllm:num_requests_running 4\n",
			Stats{Engine: "vllm", Running: 4, Waiting: 0, KVUsage: -1, Generated: -1}},
	} {
		got, err := Parse(strings.NewReader(tc.body))
		tc.want.MaxLoRA = -1
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
	if _, err := Parse(strings.NewReader("go_goroutines 12\nprocess_cpu_seconds_total 3\n")); !errors.Is(err, ErrNoMetrics) {
		t.Errorf("unknown metrics: err = %v", err)
	}
}

// TestRealVLLM parses /metrics as vLLM 0.31 served it on an RTX 4090
// (docs/VALIDATION.md): about 55 KB of counters, histograms and gauges.
func TestRealVLLM(t *testing.T) {
	f, err := os.Open("testdata/vllm-0.31-rtx4090-idle.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := Parse(f)
	if want := (Stats{Engine: "vllm", Running: 0, Waiting: 0, KVUsage: 0, Generated: 0, MaxLoRA: -1}); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(sglang))
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL + "/metrics")
	auth := func(h http.Header) { h.Set("Authorization", "Bearer k") }
	if st, err := Fetch(context.Background(), ts.Client(), u, auth); err != nil || st.Running != 31 {
		t.Errorf("Fetch = %+v, %v", st, err)
	}
	if _, err := Fetch(context.Background(), ts.Client(), u, nil); err == nil {
		t.Error("401 not reported as an error")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{vllm, sglang, tgi, llamacpp, triton, "a{b=\"\\\"} 1", "x{"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		st, err := Parse(strings.NewReader(s))
		if err != nil {
			return
		}
		if st.KVUsage > 1 || st.Waiting < 0 || (st.Running < 0 && st.Running != -1) || (st.KVUsage < 0 && st.KVUsage != -1) {
			t.Fatalf("out of range: %+v from %q", st, s)
		}
	})
}

// vLLM reports LoRA use as one gauge series per adapter set, valued with
// the time it was current.
func TestParsesLoRAAdapters(t *testing.T) {
	text := vllm + `# HELP vllm:lora_requests_info Running stats on lora requests.
# TYPE vllm:lora_requests_info gauge
vllm:lora_requests_info{max_lora="4",running_lora_adapters="old",waiting_lora_adapters=""} 1.7e9
vllm:lora_requests_info{max_lora="4",running_lora_adapters="sql-bot,support-v3",waiting_lora_adapters="legal, sql-bot"} 1.7600000001e9
vllm:lora_requests_info{max_lora="4",running_lora_adapters="",waiting_lora_adapters=""} 1.75e9
`
	st, err := Parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.Adapters, ","); got != "sql-bot,support-v3,legal" || st.MaxLoRA != 4 {
		t.Errorf("adapters %q max %d, want sql-bot,support-v3,legal and 4", got, st.MaxLoRA)
	}
	if st.Running != 21 {
		t.Errorf("other metrics still parsed: running %v", st.Running)
	}
	plain, _ := Parse(strings.NewReader(vllm))
	if plain.Adapters != nil || plain.MaxLoRA != -1 {
		t.Errorf("without LoRA info: %v %d", plain.Adapters, plain.MaxLoRA)
	}
}

func TestLabels(t *testing.T) {
	got := labels(`m{a="x,y",b="q\"uote\\",c=""} 1`)
	if got["a"] != "x,y" || got["b"] != `q"uote\` || got["c"] != "" || len(got) != 3 {
		t.Errorf("labels = %#v", got)
	}
	if labels("m 1") != nil {
		t.Error("no labels should be nil")
	}
}
