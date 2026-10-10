package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Checker actively probes every backend on an interval.
type Checker struct {
	Pool     *Pool
	Client   *http.Client
	Path     string
	Interval time.Duration
	Timeout  time.Duration
}

// Run checks all backends immediately, then on every tick until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	c.CheckAll(ctx)
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.CheckAll(ctx)
		}
	}
}

// CheckAll probes every backend in parallel and waits for the results.
func (c *Checker) CheckAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range c.Pool.Backends() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, err := c.check(ctx, b)
			if err != nil {
				c.Pool.Observe(b, false, false, err.Error())
				return
			}
			if models != nil {
				c.Pool.SetDiscoveredModels(b, models)
			}
			c.Pool.Observe(b, true, false, "")
		}()
	}
	wg.Wait()
}

func (c *Checker) check(ctx context.Context, b *Backend) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	path := c.Path
	if b.HealthPath != "" {
		path = b.HealthPath
	}
	u := *b.URL
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	b.SetAuth(req.Header)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("health check returned %s", resp.Status)
	}
	return parseModels(body), nil
}

// parseModels understands the OpenAI /v1/models shape (vLLM, SGLang,
// llama.cpp, LM Studio, Ollama's OpenAI layer), Ollama's native /api/tags
// shape and TGI's /info. It returns nil for anything else (an empty Triton
// readiness reply, plain "ok"), which leaves the backend's model list unknown.
func parseModels(body []byte) []string {
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		ModelID string `json:"model_id"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var out []string
	if v.ModelID != "" {
		out = append(out, v.ModelID)
	}
	for _, m := range v.Data {
		out = append(out, m.ID)
	}
	for _, m := range v.Models {
		out = append(out, m.Name)
	}
	return out
}
