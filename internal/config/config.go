// Package config loads and validates the SiliconRouter YAML configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Tier names a class of hardware. The router prefers heavy nodes for large
// prompts, light nodes for small ones, and only touches reserve nodes when
// everything else is full or down.
const (
	TierHeavy    = "heavy"
	TierStandard = "standard"
	TierLight    = "light"
	TierReserve  = "reserve"
	// TierAuto lets chipagent's hardware report pick heavy, standard or light.
	TierAuto = "auto"
)

// Duration is a time.Duration that unmarshals from strings like "5s" or "30m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	Listen       string    `yaml:"listen"`
	MaxBodyBytes int64     `yaml:"max_body_bytes"`
	Auth         Auth      `yaml:"auth"`
	Backends     []Backend `yaml:"backends"`
	Routing      Routing   `yaml:"routing"`
	Queue        Queue     `yaml:"queue"`
	Retry        Retry     `yaml:"retry"`
	Health       Health    `yaml:"health"`
	Upstream     Upstream  `yaml:"upstream"`
	Hardware     Hardware  `yaml:"hardware"`
	Cache        Cache     `yaml:"cache"`
	Models       Models    `yaml:"models"`
}

// Priority classes order the wait queue: high requests are served before
// normal ones, normal before low.
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

// PriorityRank maps a priority name to its queue rank; ok is false for unknown names.
func PriorityRank(name string) (rank int, ok bool) {
	switch name {
	case PriorityLow:
		return 0, true
	case PriorityNormal, "":
		return 1, true
	case PriorityHigh:
		return 2, true
	}
	return 1, false
}

// APIKey is a named client key with its own queue priority.
type APIKey struct {
	Name     string `yaml:"name"`
	Key      string `yaml:"key"`
	KeyEnv   string `yaml:"key_env"`
	Priority string `yaml:"priority"` // high, normal (default) or low
}

// Cache answers repeated requests from memory instead of a GPU. Only
// requests whose answer cannot change are cached unless Mode is "all":
// embeddings, and generations with temperature 0.
type Cache struct {
	Enabled       bool     `yaml:"enabled"`
	Mode          string   `yaml:"mode"` // deterministic (default) or all
	TTL           Duration `yaml:"ttl"`
	MaxBytes      int64    `yaml:"max_bytes"`
	MaxEntryBytes int64    `yaml:"max_entry_bytes"`
	// ShareAcrossKeys lets clients with different API keys get each other's
	// cached answers. Off by default so tenants never see each other's data.
	ShareAcrossKeys bool `yaml:"share_across_keys"`
}

// Models customizes the model names clients see.
type Models struct {
	// Aliases maps the name a client asks for to the model that serves it,
	// e.g. gpt-4o: llama-3.1-70b, so apps keep working when models change.
	Aliases map[string]string `yaml:"aliases"`
}

// Auth protects the router itself. When no keys are configured the router is open.
type Auth struct {
	APIKeys    []string `yaml:"api_keys"`
	APIKeysEnv string   `yaml:"api_keys_env"` // comma-separated keys read from this env var
	// Keys are named keys with a queue priority. api_keys are normal priority.
	Keys []APIKey `yaml:"keys"`
}

// Enabled reports whether the router requires a key.
func (a Auth) Enabled() bool { return len(a.APIKeys) > 0 || len(a.Keys) > 0 }

type Backend struct {
	Name             string   `yaml:"name"`
	URL              string   `yaml:"url"`
	Tier             string   `yaml:"tier"`
	MaxConcurrency   int      `yaml:"max_concurrency"`
	MaxContextTokens int      `yaml:"max_context_tokens"` // 0 = no limit
	Models           []string `yaml:"models"`             // empty = discover from /v1/models
	APIKeyEnv        string   `yaml:"api_key_env"`        // bearer token sent upstream
	// APIKeyHeader sends the key as a raw header value (e.g. x-api-key,
	// api-key) instead of "Authorization: Bearer".
	APIKeyHeader string `yaml:"api_key_header"`
	// HealthPath overrides health.path for engines with their own health
	// endpoint, e.g. /health (TGI), /v2/health/ready (Triton, KServe), /api/tags (Ollama).
	HealthPath string `yaml:"health_path"`
	// AgentURL points at the chipagent on this backend's machine, which
	// reports its accelerators (NVIDIA, AMD, Intel, Apple) in one schema.
	AgentURL string `yaml:"agent_url"`

	APIKey string `yaml:"-"`
}

type Routing struct {
	// PrefixAffinity sends requests that share a prompt prefix (e.g. the next
	// turn of a chat) to the backend that already holds that prefix in its KV cache.
	PrefixAffinity *bool `yaml:"prefix_affinity"`
	// BlockChars is the granularity of prefix matching.
	BlockChars int `yaml:"block_chars"`
	// MaxBlocks caps how much of a prompt is hashed for matching.
	MaxBlocks        int      `yaml:"max_blocks"`
	AffinityTTL      Duration `yaml:"affinity_ttl"`
	AffinityCapacity int      `yaml:"affinity_capacity"`
	// AffinityMaxImbalance is how much busier (as a fraction of capacity) the
	// cache-holding backend may be than the least busy one before the router
	// gives up the cache hit to keep load even.
	AffinityMaxImbalance float64 `yaml:"affinity_max_imbalance"`
	// LargeRequestTokens is the estimated prompt size above which a request
	// prefers heavy hardware.
	LargeRequestTokens int     `yaml:"large_request_tokens"`
	CharsPerToken      float64 `yaml:"chars_per_token"`
}

func (r Routing) AffinityEnabled() bool { return r.PrefixAffinity == nil || *r.PrefixAffinity }

type Queue struct {
	MaxWaiting int      `yaml:"max_waiting"`
	MaxWait    Duration `yaml:"max_wait"`
	// PriorityHeader lets clients set their priority with the
	// X-SiliconRouter-Priority header. Only enable it for trusted clients.
	PriorityHeader bool `yaml:"priority_header"`
}

type Retry struct {
	MaxAttempts int `yaml:"max_attempts"`
	// FirstTokenTimeout: if a streaming response has not produced its first
	// bytes by then, the request moves to another backend before anything
	// reaches the client. 0 disables it. The last attempt always waits.
	FirstTokenTimeout Duration `yaml:"first_token_timeout"`
}

type Health struct {
	Disabled           bool     `yaml:"disabled"`
	Path               string   `yaml:"path"`
	Interval           Duration `yaml:"interval"`
	Timeout            Duration `yaml:"timeout"`
	UnhealthyThreshold int      `yaml:"unhealthy_threshold"`
	HealthyThreshold   int      `yaml:"healthy_threshold"`
}

type Upstream struct {
	DialTimeout Duration `yaml:"dial_timeout"`
	// ResponseHeaderTimeout bounds time-to-first-byte. Non-streaming requests
	// only send headers once generation finishes, so keep this generous. 0 = none.
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
}

// Hardware controls polling of chipagent telemetry and how `tier: auto`
// backends are classified.
type Hardware struct {
	PollInterval Duration `yaml:"poll_interval"`
	Timeout      Duration `yaml:"timeout"`
	// Total accelerator memory at or above which an auto-tiered node is heavy
	// or standard; below StandardMinMemoryGiB it is light.
	HeavyMinMemoryGiB    float64 `yaml:"heavy_min_memory_gib"`
	StandardMinMemoryGiB float64 `yaml:"standard_min_memory_gib"`
}

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse defaults and validates config bytes. Environment variables named in
// the config are resolved here.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	c.resolveEnv()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 16 << 20
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.Tier == "" {
			b.Tier = TierStandard
		}
		if b.MaxConcurrency == 0 {
			b.MaxConcurrency = 16
		}
	}
	r := &c.Routing
	if r.BlockChars == 0 {
		r.BlockChars = 256
	}
	if r.MaxBlocks == 0 {
		r.MaxBlocks = 256
	}
	if r.AffinityTTL == 0 {
		r.AffinityTTL = Duration(30 * time.Minute)
	}
	if r.AffinityCapacity == 0 {
		r.AffinityCapacity = 200_000
	}
	if r.AffinityMaxImbalance == 0 {
		r.AffinityMaxImbalance = 0.5
	}
	if r.LargeRequestTokens == 0 {
		r.LargeRequestTokens = 2048
	}
	if r.CharsPerToken == 0 {
		r.CharsPerToken = 4
	}
	if c.Queue.MaxWaiting == 0 {
		c.Queue.MaxWaiting = 1024
	}
	if c.Queue.MaxWait == 0 {
		c.Queue.MaxWait = Duration(30 * time.Second)
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 3
	}
	if c.Retry.FirstTokenTimeout == 0 {
		// Generous enough for a very long prompt's prefill; catches a backend
		// that accepted the request and stalled. Chat deployments can go lower.
		c.Retry.FirstTokenTimeout = Duration(60 * time.Second)
	}
	cc := &c.Cache
	if cc.Mode == "" {
		cc.Mode = "deterministic"
	}
	if cc.TTL == 0 {
		cc.TTL = Duration(10 * time.Minute)
	}
	if cc.MaxBytes == 0 {
		cc.MaxBytes = 256 << 20
	}
	if cc.MaxEntryBytes == 0 {
		cc.MaxEntryBytes = 4 << 20
	}
	h := &c.Health
	if h.Path == "" {
		h.Path = "/v1/models"
	}
	if h.Interval == 0 {
		h.Interval = Duration(5 * time.Second)
	}
	if h.Timeout == 0 {
		h.Timeout = Duration(2 * time.Second)
	}
	if h.UnhealthyThreshold == 0 {
		h.UnhealthyThreshold = 3
	}
	if h.HealthyThreshold == 0 {
		h.HealthyThreshold = 2
	}
	if c.Upstream.DialTimeout == 0 {
		c.Upstream.DialTimeout = Duration(5 * time.Second)
	}
	hw := &c.Hardware
	if hw.PollInterval == 0 {
		hw.PollInterval = Duration(10 * time.Second)
	}
	if hw.Timeout == 0 {
		hw.Timeout = Duration(2 * time.Second)
	}
	if hw.HeavyMinMemoryGiB == 0 {
		hw.HeavyMinMemoryGiB = 64
	}
	if hw.StandardMinMemoryGiB == 0 {
		hw.StandardMinMemoryGiB = 20
	}
}

func (c *Config) resolveEnv() {
	if c.Auth.APIKeysEnv != "" {
		for _, k := range strings.Split(os.Getenv(c.Auth.APIKeysEnv), ",") {
			if k = strings.TrimSpace(k); k != "" {
				c.Auth.APIKeys = append(c.Auth.APIKeys, k)
			}
		}
	}
	for i := range c.Auth.Keys {
		if k := &c.Auth.Keys[i]; k.KeyEnv != "" {
			k.Key = os.Getenv(k.KeyEnv)
		}
	}
	for i := range c.Backends {
		if env := c.Backends[i].APIKeyEnv; env != "" {
			c.Backends[i].APIKey = os.Getenv(env)
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if len(c.Backends) == 0 {
		errs = append(errs, errors.New("at least one backend is required"))
	}
	seen := map[string]bool{}
	for i, b := range c.Backends {
		where := fmt.Sprintf("backends[%d]", i)
		if b.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		} else if seen[b.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name %q", where, b.Name))
		}
		seen[b.Name] = true
		u, err := url.Parse(b.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: url must be an absolute http(s) URL, got %q", where, b.URL))
		}
		switch b.Tier {
		case TierHeavy, TierStandard, TierLight, TierReserve:
		case TierAuto:
			if b.AgentURL == "" {
				errs = append(errs, fmt.Errorf("%s: tier auto needs agent_url so chipagent can report the hardware", where))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: tier must be heavy, standard, light, reserve or auto, got %q", where, b.Tier))
		}
		if b.AgentURL != "" {
			if u, err := url.Parse(b.AgentURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("%s: agent_url must be an absolute http(s) URL, got %q", where, b.AgentURL))
			}
		}
		if b.MaxConcurrency < 0 {
			errs = append(errs, fmt.Errorf("%s: max_concurrency must be positive", where))
		}
		if b.MaxContextTokens < 0 {
			errs = append(errs, fmt.Errorf("%s: max_context_tokens must not be negative", where))
		}
		if b.HealthPath != "" && !strings.HasPrefix(b.HealthPath, "/") {
			errs = append(errs, fmt.Errorf("%s: health_path must start with /", where))
		}
		if b.APIKeyEnv != "" && b.APIKey == "" {
			errs = append(errs, fmt.Errorf("%s: api_key_env %q is not set", where, b.APIKeyEnv))
		}
	}
	r := c.Routing
	if r.BlockChars < 16 {
		errs = append(errs, errors.New("routing.block_chars must be at least 16"))
	}
	if r.MaxBlocks < 1 || r.AffinityCapacity < 1 || r.LargeRequestTokens < 1 || r.CharsPerToken <= 0 {
		errs = append(errs, errors.New("routing: max_blocks, affinity_capacity, large_request_tokens and chars_per_token must be positive"))
	}
	if r.AffinityMaxImbalance < 0 || r.AffinityMaxImbalance > 1 {
		errs = append(errs, errors.New("routing.affinity_max_imbalance must be between 0 and 1"))
	}
	if c.Queue.MaxWaiting < 0 {
		errs = append(errs, errors.New("queue.max_waiting must not be negative"))
	}
	if c.Retry.MaxAttempts < 1 {
		errs = append(errs, errors.New("retry.max_attempts must be at least 1"))
	}
	if c.Retry.FirstTokenTimeout < 0 {
		errs = append(errs, errors.New("retry.first_token_timeout must not be negative"))
	}
	for i, k := range c.Auth.Keys {
		where := fmt.Sprintf("auth.keys[%d]", i)
		if k.Key == "" {
			errs = append(errs, fmt.Errorf("%s: key is empty (set key or a key_env that exists)", where))
		}
		if _, ok := PriorityRank(k.Priority); !ok {
			errs = append(errs, fmt.Errorf("%s: priority must be high, normal or low, got %q", where, k.Priority))
		}
	}
	if m := c.Cache.Mode; m != "deterministic" && m != "all" {
		errs = append(errs, fmt.Errorf("cache.mode must be deterministic or all, got %q", m))
	}
	if c.Cache.MaxBytes < 0 || c.Cache.MaxEntryBytes < 0 || c.Cache.MaxEntryBytes > c.Cache.MaxBytes {
		errs = append(errs, errors.New("cache: need 0 <= max_entry_bytes <= max_bytes"))
	}
	for alias, target := range c.Models.Aliases {
		if alias == "" || target == "" {
			errs = append(errs, errors.New("models.aliases: names must not be empty"))
		} else if _, chained := c.Models.Aliases[target]; chained {
			errs = append(errs, fmt.Errorf("models.aliases: %q points to %q, which is itself an alias", alias, target))
		}
	}
	if !strings.HasPrefix(c.Health.Path, "/") {
		errs = append(errs, errors.New("health.path must start with /"))
	}
	if hw := c.Hardware; hw.StandardMinMemoryGiB < 0 || hw.HeavyMinMemoryGiB < hw.StandardMinMemoryGiB {
		errs = append(errs, errors.New("hardware: need 0 <= standard_min_memory_gib <= heavy_min_memory_gib"))
	}
	if c.MaxBodyBytes < 0 {
		errs = append(errs, errors.New("max_body_bytes must not be negative"))
	}
	return errors.Join(errs...)
}
