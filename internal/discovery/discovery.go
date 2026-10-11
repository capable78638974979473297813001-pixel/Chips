// Package discovery finds backends in DNS: the A/AAAA records of a name
// (a Kubernetes headless service, round-robin DNS) or its SRV records
// (Consul, Kubernetes named ports).
package discovery

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// Resolver is the part of *net.Resolver discovery uses.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// Watcher keeps the discovered backends current.
type Watcher struct {
	Resolver Resolver
	Log      *slog.Logger
	// Sources returns the current discovery config; it is read on every
	// round, so a config reload takes effect without a restart.
	Sources func() []config.Discovery
	// Apply receives the full list of discovered backends whenever it
	// changes. An error leaves the list to be applied again next round.
	Apply func([]config.Backend) error
	// Timeout bounds one lookup (default 5s).
	Timeout time.Duration

	mu      sync.Mutex
	known   map[string][]endpoint // last good answer per query
	due     map[string]time.Time  // next lookup per query
	failing map[string]bool       // queries whose last lookup failed, to log once
	applied []config.Backend
	ok      bool // applied has been accepted at least once
}

type endpoint struct {
	host string
	port int
}

// query identifies what a source looks up; sources that differ only in
// the backend settings share an answer.
func query(d config.Discovery) string {
	if d.SRV != "" {
		return "srv " + d.SRV
	}
	return fmt.Sprintf("dns %s port %d", d.DNS, d.Port)
}

// Run refreshes until ctx is done, checking every second which sources
// are due for a lookup.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Refresh(ctx, false)
		}
	}
}

// Refresh looks up every source that is due (all of them with force) and
// applies the result if the discovered backends changed.
func (w *Watcher) Refresh(ctx context.Context, force bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.known == nil {
		w.known, w.due, w.failing = map[string][]endpoint{}, map[string]time.Time{}, map[string]bool{}
	}
	sources := w.Sources()
	now := time.Now()
	live := map[string]bool{}
	for _, d := range sources {
		q := query(d)
		live[q] = true
		if !force && now.Before(w.due[q]) {
			continue
		}
		w.due[q] = now.Add(d.Interval.D())
		eps, err := w.lookup(ctx, d)
		if err != nil {
			if !w.failing[q] {
				w.Log.Warn("backend discovery lookup failed; keeping the last known backends", "source", d.Name, "query", q, "error", err)
			}
			w.failing[q] = true
			continue
		}
		if w.failing[q] {
			w.Log.Info("backend discovery lookup recovered", "source", d.Name, "query", q)
		}
		w.failing[q] = false
		w.known[q] = eps
	}
	for q := range w.known { // forget sources removed from the config
		if !live[q] {
			delete(w.known, q)
			delete(w.due, q)
			delete(w.failing, q)
		}
	}

	var backends []config.Backend
	seen := map[string]bool{}
	for _, d := range sources {
		for _, ep := range w.known[query(d)] {
			b := d.Expand(ep.host, ep.port)
			if !seen[b.Name] {
				seen[b.Name] = true
				backends = append(backends, b)
			}
		}
	}
	if w.ok && reflect.DeepEqual(backends, w.applied) {
		return
	}
	if err := w.Apply(backends); err != nil {
		w.Log.Error("applying discovered backends failed", "error", err)
		return
	}
	w.applied, w.ok = backends, true
}

func (w *Watcher) lookup(ctx context.Context, d config.Discovery) ([]endpoint, error) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var eps []endpoint
	if d.SRV != "" {
		_, records, err := w.Resolver.LookupSRV(ctx, "", "", d.SRV)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			if host := strings.TrimSuffix(r.Target, "."); host != "" && r.Port != 0 {
				eps = append(eps, endpoint{host, int(r.Port)})
			}
		}
	} else {
		addrs, err := w.Resolver.LookupIPAddr(ctx, d.DNS)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			eps = append(eps, endpoint{a.IP.String(), d.Port})
		}
	}
	if len(eps) == 0 {
		return nil, fmt.Errorf("no records for %s", query(d))
	}
	slices.SortFunc(eps, func(a, b endpoint) int { return cmp.Or(strings.Compare(a.host, b.host), a.port-b.port) })
	return slices.Compact(eps), nil
}
