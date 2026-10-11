// Package engine reads the load an inference engine reports about itself
// on its Prometheus /metrics endpoint: requests running, requests queued
// inside the engine, and how full its KV cache is. The router's own
// in-flight count cannot see traffic from other routers or clients, and
// accelerator memory says nothing about KV pressure, because engines
// reserve their memory up front.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Stats is one scrape of an engine's load.
type Stats struct {
	Engine  string  // vllm, sglang, tgi, llamacpp or triton
	Running float64 // requests being generated (-1 if not reported)
	Waiting float64 // requests queued inside the engine
	// KVUsage is the fraction of KV cache in use, 0 to 1, or -1 when the
	// engine does not report it.
	KVUsage float64
	// Generated is the engine's running total of generated tokens, or -1
	// when it does not count them as it goes. Its growth between scrapes
	// shows whether an engine with running requests is making progress.
	Generated float64
}

type role int

const (
	running role = iota
	waiting
	kvUsage
	generated
)

// metric maps a known metric name to what it measures.
type metric struct {
	engine string
	role   role
}

// known lists the metrics each engine exposes. Counts are summed over
// label sets (several models, data-parallel ranks); KV usage takes the
// fullest one, since one full rank is enough to stall requests on it.
var known = map[string]metric{
	// vLLM (v0 and v1)
	"vllm:num_requests_running":          {"vllm", running},
	"vllm:num_requests_waiting":          {"vllm", waiting},
	"vllm:gpu_cache_usage_perc":          {"vllm", kvUsage}, // a 0-1 fraction despite the name
	"vllm:kv_cache_usage_perc":           {"vllm", kvUsage},
	"sglang:num_running_reqs":            {"sglang", running},
	"sglang:num_queue_reqs":              {"sglang", waiting},
	"sglang:token_usage":                 {"sglang", kvUsage},
	"tgi_batch_current_size":             {"tgi", running},
	"tgi_queue_size":                     {"tgi", waiting},
	"llamacpp:requests_processing":       {"llamacpp", running},
	"llamacpp:requests_deferred":         {"llamacpp", waiting},
	"llamacpp:kv_cache_usage_ratio":      {"llamacpp", kvUsage},
	"nv_inference_pending_request_count": {"triton", waiting},
	// Counted per decoding step, so they grow while requests generate.
	"vllm:generation_tokens_total":   {"vllm", generated},
	"sglang:generation_tokens_total": {"sglang", generated},
}

// ErrNoMetrics means the endpoint answered but exposes none of the known
// engine metrics.
var ErrNoMetrics = errors.New("no known inference engine metrics (vLLM, SGLang, TGI, llama.cpp, Triton)")

// Parse reads Prometheus text exposition format.
func Parse(r io.Reader) (Stats, error) {
	st := Stats{Running: -1, KVUsage: -1, Generated: -1}
	found := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, value, ok := sample(line)
		if !ok {
			continue
		}
		m, ok := known[name]
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			continue
		}
		found = true
		if st.Engine == "" {
			st.Engine = m.engine
		}
		switch m.role {
		case running:
			st.Running = max(st.Running, 0) + value
		case waiting:
			st.Waiting += value
		case kvUsage:
			st.KVUsage = max(st.KVUsage, min(value, 1))
		case generated:
			st.Generated = max(st.Generated, 0) + value
		}
	}
	if err := sc.Err(); err != nil {
		return Stats{}, err
	}
	if !found {
		return Stats{}, ErrNoMetrics
	}
	return st, nil
}

// sample splits `name{labels} value [timestamp]`.
func sample(line string) (name string, value float64, ok bool) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return "", 0, false
	}
	name, rest := line[:i], line[i:]
	if rest[0] == '{' {
		end := labelsEnd(rest)
		if end < 0 {
			return "", 0, false
		}
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	return name, v, err == nil
}

// labelsEnd finds the } closing a label set, skipping quoted values.
func labelsEnd(s string) int {
	quoted := false
	for i := 1; i < len(s); i++ {
		switch {
		case quoted && s[i] == '\\':
			i++
		case s[i] == '"':
			quoted = !quoted
		case !quoted && s[i] == '}':
			return i
		}
	}
	return -1
}

// maxBody bounds a scrape; engines' metrics are a few hundred KiB at most.
const maxBody = 8 << 20

// Fetch scrapes u.
func Fetch(ctx context.Context, client *http.Client, u *url.URL, apiKey func(http.Header)) (Stats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Stats{}, err
	}
	req.Header.Set("Accept", "text/plain")
	if apiKey != nil {
		apiKey(req.Header)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Stats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Stats{}, fmt.Errorf("metrics endpoint answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Stats{}, err
	}
	if len(body) > maxBody {
		return Stats{}, errors.New("metrics response larger than 8 MiB")
	}
	return Parse(bytes.NewReader(body))
}
