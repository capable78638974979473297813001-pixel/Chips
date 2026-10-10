package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`
backends:
  - name: a
    url: http://10.0.0.1:8000
`))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Backends[0]
	if b.Tier != TierStandard || b.MaxConcurrency != 16 {
		t.Errorf("backend defaults = %+v", b)
	}
	if c.Listen != ":8080" || c.Retry.MaxAttempts != 3 || c.Queue.MaxWait.D() != 30*time.Second {
		t.Errorf("top-level defaults wrong: %+v", c)
	}
	if !c.Routing.AffinityEnabled() || c.Routing.AffinityMaxImbalance != 0.5 {
		t.Errorf("routing defaults wrong: %+v", c.Routing)
	}
}

func TestAffinityCanBeDisabled(t *testing.T) {
	c, err := Parse([]byte(`
backends: [{name: a, url: "http://h:1"}]
routing: {prefix_affinity: false}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Routing.AffinityEnabled() {
		t.Error("prefix_affinity: false was ignored")
	}
}

func TestEnvResolution(t *testing.T) {
	t.Setenv("SR_KEYS", " k1 ,k2,, ")
	t.Setenv("SR_BACKEND_KEY", "secret")
	c, err := Parse([]byte(`
auth: {api_keys_env: SR_KEYS}
backends: [{name: a, url: "http://h:1", api_key_env: SR_BACKEND_KEY}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Auth.APIKeys, "|"); got != "k1|k2" {
		t.Errorf("api keys = %q", got)
	}
	if c.Backends[0].APIKey != "secret" {
		t.Errorf("backend key not resolved")
	}
}

func TestAutoTier(t *testing.T) {
	c, err := Parse([]byte(`
backends: [{name: a, url: "http://h:8000", tier: auto, agent_url: "http://h:9400"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].Tier != TierAuto || c.Hardware.PollInterval.D() != 10*time.Second || c.Hardware.HeavyMinMemoryGiB != 64 {
		t.Errorf("auto tier config = %+v, hardware = %+v", c.Backends[0], c.Hardware)
	}
}

func TestSpeedAndCustomizationDefaults(t *testing.T) {
	t.Setenv("SR_CHAT_KEY", "secret")
	c, err := Parse([]byte(`
backends: [{name: a, url: "http://h:1"}]
auth: {keys: [{name: chat, key_env: SR_CHAT_KEY, priority: high}]}
cache: {enabled: true}
models: {aliases: {gpt-4o: llama}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Retry.FirstTokenTimeout.D() != 60*time.Second || c.Cache.Mode != "deterministic" || c.Cache.TTL.D() != 10*time.Minute {
		t.Errorf("defaults: retry %+v cache %+v", c.Retry, c.Cache)
	}
	if k := c.Auth.Keys[0]; k.Key != "secret" || !c.Auth.Enabled() {
		t.Errorf("named key = %+v", k)
	}
	if r, _ := PriorityRank("high"); r != 2 {
		t.Errorf("rank(high) = %d", r)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"no backends":     `listen: ":1"`,
		"duplicate name":  `backends: [{name: a, url: "http://h:1"}, {name: a, url: "http://h:2"}]`,
		"bad url":         `backends: [{name: a, url: "h:1"}]`,
		"bad tier":        `backends: [{name: a, url: "http://h:1", tier: turbo}]`,
		"unknown field":   `backends: [{name: a, url: "http://h:1", wieght: 2}]`,
		"bad duration":    "backends: [{name: a, url: \"http://h:1\"}]\nqueue: {max_wait: soon}",
		"missing env key": `backends: [{name: a, url: "http://h:1", api_key_env: SR_DEFINITELY_UNSET}]`,
		"bad imbalance":   "backends: [{name: a, url: \"http://h:1\"}]\nrouting: {affinity_max_imbalance: 2}",
		"bad priority":    "backends: [{name: a, url: \"http://h:1\"}]\nauth: {keys: [{name: x, key: k, priority: urgent}]}",
		"key env unset":   "backends: [{name: a, url: \"http://h:1\"}]\nauth: {keys: [{name: x, key_env: SR_DEFINITELY_UNSET}]}",
		"bad cache mode":  "backends: [{name: a, url: \"http://h:1\"}]\ncache: {mode: sometimes}",
		"chained alias":   "backends: [{name: a, url: \"http://h:1\"}]\nmodels: {aliases: {a: b, b: c}}",
		"negative ttft":   "backends: [{name: a, url: \"http://h:1\"}]\nretry: {first_token_timeout: -1s}",
		"bad health path": `backends: [{name: a, url: "http://h:1", health_path: "health"}]`,
		"auto no agent":   `backends: [{name: a, url: "http://h:1", tier: auto}]`,
		"bad agent url":   `backends: [{name: a, url: "http://h:1", agent_url: "h:9400"}]`,
		"bad thresholds":  "backends: [{name: a, url: \"http://h:1\"}]\nhardware: {heavy_min_memory_gib: 10, standard_min_memory_gib: 40}",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
