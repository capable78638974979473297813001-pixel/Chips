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
}

// Auth protects the router itself. When no keys are configured the router is open.
type Auth struct {
	APIKeys    []string `yaml:"api_keys"`
	APIKeysEnv string   `yaml:"api_keys_env"` // comma-separated keys read from this env var
}

type Backend struct {
	Name             string   `yaml:"name"`
	URL              string   `yaml:"url"`
	Tier             string   `yaml:"tier"`
	MaxConcurrency   int      `yaml:"max_concurrency"`
	MaxContextTokens int      `yaml:"max_context_tokens"` // 0 = no limit
	Models           []string `yaml:"models"`             // empty = discover from /v1/models
	APIKeyEnv        string   `yaml:"api_key_env"`        // bearer token sent upstream

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
}

type Retry struct {
	MaxAttempts int `yaml:"max_attempts"`
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
}

func (c *Config) resolveEnv() {
	if c.Auth.APIKeysEnv != "" {
		for _, k := range strings.Split(os.Getenv(c.Auth.APIKeysEnv), ",") {
			if k = strings.TrimSpace(k); k != "" {
				c.Auth.APIKeys = append(c.Auth.APIKeys, k)
			}
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
		default:
			errs = append(errs, fmt.Errorf("%s: tier must be heavy, standard, light or reserve, got %q", where, b.Tier))
		}
		if b.MaxConcurrency < 0 {
			errs = append(errs, fmt.Errorf("%s: max_concurrency must be positive", where))
		}
		if b.MaxContextTokens < 0 {
			errs = append(errs, fmt.Errorf("%s: max_context_tokens must not be negative", where))
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
	if !strings.HasPrefix(c.Health.Path, "/") {
		errs = append(errs, errors.New("health.path must start with /"))
	}
	if c.MaxBodyBytes < 0 {
		errs = append(errs, errors.New("max_body_bytes must not be negative"))
	}
	return errors.Join(errs...)
}
