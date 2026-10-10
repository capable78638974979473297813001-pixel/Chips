package pool

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/hardware"
)

// AgentPoller reads chipagent telemetry for every backend that has an
// agent_url, so the pool knows what hardware each node runs and how busy it is.
type AgentPoller struct {
	Pool     *Pool
	Client   *http.Client
	Interval time.Duration
	Timeout  time.Duration
}

// HasAgents reports whether any backend has a chipagent to poll.
func (a *AgentPoller) HasAgents() bool {
	for _, b := range a.Pool.Backends() {
		if b.AgentURL != nil {
			return true
		}
	}
	return false
}

// Run polls immediately, then on every tick until ctx is done.
func (a *AgentPoller) Run(ctx context.Context) {
	a.PollAll(ctx)
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.PollAll(ctx)
		}
	}
}

// PollAll queries every agent in parallel and waits for the results.
func (a *AgentPoller) PollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range a.Pool.Backends() {
		if b.AgentURL == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, a.Timeout)
			defer cancel()
			r, err := hardware.Fetch(ctx, a.Client, b.AgentURL)
			a.Pool.SetHardware(b, r, err)
		}()
	}
	wg.Wait()
}
