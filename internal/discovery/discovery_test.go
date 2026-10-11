package discovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

type fakeResolver struct {
	mu   sync.Mutex
	ips  map[string][]string
	srv  map[string][]*net.SRV
	err  error
	hits int
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	var out []net.IPAddr
	for _, s := range f.ips[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func (f *fakeResolver) LookupSRV(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.err != nil {
		return "", nil, f.err
	}
	return name, f.srv[name], nil
}

func (f *fakeResolver) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

type sink struct {
	calls [][]config.Backend
	err   error
}

func (s *sink) apply(b []config.Backend) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, b)
	return nil
}

func (s *sink) names() []string {
	var out []string
	for _, b := range s.calls[len(s.calls)-1] {
		out = append(out, b.Name)
	}
	return out
}

func newWatcher(r Resolver, s *sink, sources ...config.Discovery) *Watcher {
	return &Watcher{Resolver: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sources: func() []config.Discovery { return sources }, Apply: s.apply}
}

func vllm() config.Discovery {
	return config.Discovery{Name: "vllm", DNS: "vllm.svc", Port: 8000, Scheme: "http", Interval: config.Duration(time.Hour),
		AgentPort: 9400, Backend: config.Backend{Tier: config.TierHeavy, MaxConcurrency: 8, Models: []string{"llama"}}}
}

func TestDNSDiscovery(t *testing.T) {
	r := &fakeResolver{ips: map[string][]string{"vllm.svc": {"10.0.0.2", "10.0.0.1", "fd00::1", "10.0.0.1"}}}
	var s sink
	w := newWatcher(r, &s, vllm())
	w.Refresh(context.Background(), true)

	if want := []string{"vllm-10.0.0.1:8000", "vllm-10.0.0.2:8000", "vllm-[fd00::1]:8000"}; !slices.Equal(s.names(), want) {
		t.Fatalf("names = %v, want %v", s.names(), want)
	}
	b := s.calls[0][2]
	if b.URL != "http://[fd00::1]:8000" || b.AgentURL != "http://[fd00::1]:9400" || b.Tier != config.TierHeavy || b.MaxConcurrency != 8 {
		t.Errorf("backend = %+v", b)
	}

	// Unchanged answers apply nothing; not-yet-due sources are not looked up.
	hits := r.hits
	w.Refresh(context.Background(), false)
	if len(s.calls) != 1 || r.hits != hits {
		t.Errorf("idle refresh: %d applies, %d lookups", len(s.calls), r.hits-hits)
	}

	// A scale-down is applied.
	r.set(func() { r.ips["vllm.svc"] = []string{"10.0.0.2"} })
	w.Refresh(context.Background(), true)
	if want := []string{"vllm-10.0.0.2:8000"}; !slices.Equal(s.names(), want) {
		t.Errorf("after scale-down: %v", s.names())
	}

	// A DNS outage keeps the last known backends.
	r.set(func() { r.err = errors.New("SERVFAIL") })
	w.Refresh(context.Background(), true)
	if len(s.calls) != 2 {
		t.Errorf("a failed lookup changed the backends: %v", s.names())
	}
	// So does an empty answer.
	r.set(func() { r.err = nil; r.ips["vllm.svc"] = nil })
	w.Refresh(context.Background(), true)
	if len(s.calls) != 2 {
		t.Errorf("an empty answer changed the backends: %v", s.names())
	}
}

func TestSRVDiscovery(t *testing.T) {
	r := &fakeResolver{srv: map[string][]*net.SRV{"_http._tcp.tgi.svc": {
		{Target: "tgi-1.tgi.svc.", Port: 8080}, {Target: "tgi-0.tgi.svc.", Port: 8081},
	}}}
	var s sink
	w := newWatcher(r, &s, config.Discovery{Name: "tgi", SRV: "_http._tcp.tgi.svc", Scheme: "https", Interval: config.Duration(time.Hour)})
	w.Refresh(context.Background(), true)
	if want := []string{"tgi-tgi-0.tgi.svc:8081", "tgi-tgi-1.tgi.svc:8080"}; !slices.Equal(s.names(), want) {
		t.Fatalf("names = %v", s.names())
	}
	if u := s.calls[0][0].URL; u != "https://tgi-0.tgi.svc:8081" {
		t.Errorf("url = %s", u)
	}
}

func TestRemovedSourceDropsItsBackendsAndFailedApplyRetries(t *testing.T) {
	r := &fakeResolver{ips: map[string][]string{"vllm.svc": {"10.0.0.1"}}}
	s := sink{err: errors.New("pool rejected it")}
	sources := []config.Discovery{vllm()}
	w := &Watcher{Resolver: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sources: func() []config.Discovery { return sources }, Apply: s.apply}
	w.Refresh(context.Background(), true)
	s.err = nil
	w.Refresh(context.Background(), false) // nothing due, but the last apply failed
	if len(s.calls) != 1 || len(s.calls[0]) != 1 {
		t.Fatalf("failed apply was not retried: %v", s.calls)
	}
	sources = nil
	w.Refresh(context.Background(), false)
	if len(s.calls) != 2 || len(s.calls[1]) != 0 {
		t.Errorf("removed source kept its backends: %v", s.calls)
	}
}
