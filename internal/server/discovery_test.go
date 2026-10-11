package server

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

type staticResolver []string

func (r staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	var out []net.IPAddr
	for _, s := range r {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func (staticResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func TestDiscoveredBackendsServeAndSurviveReload(t *testing.T) {
	static := newFake(t, nil)
	found := newFake(t, nil)
	u, _ := url.Parse(found.URL)
	port, _ := strconv.Atoi(u.Port())

	var cfg *config.Config
	ts, p := setup(t, func(c *config.Config) {
		c.Discovery = []config.Discovery{{Name: "pods", DNS: "pods.svc", Port: port, Scheme: "http",
			Interval: config.Duration(time.Hour), Backend: config.Backend{Tier: config.TierStandard, MaxConcurrency: 4, Models: []string{"llama"}}}}
		cfg = c
	}, static.URL)
	srv := ts.Config.Handler.(*Server)
	srv.watcher.Resolver = staticResolver{"127.0.0.1"}
	srv.DiscoverNow(context.Background())

	name := "pods-127.0.0.1:" + u.Port()
	if len(p.Snapshot()) != 2 || p.Snapshot()[1].Name != name {
		t.Fatalf("backends = %+v", p.Snapshot())
	}
	if _, ok := p.SetDrained("b0", true); !ok {
		t.Fatal("static backend missing")
	}
	if r := chat(t, ts.URL, `{"model":"llama"}`); r.Header.Get("X-SiliconRouter-Backend") != name || found.hits.Load() != 1 {
		t.Errorf("discovered backend did not serve: %q", r.Header.Get("X-SiliconRouter-Backend"))
	}

	// A reload of the config file keeps what discovery found.
	next := *cfg
	next.Backends = append([]config.Backend(nil), cfg.Backends...)
	next.Backends[0].MaxConcurrency = 2
	if _, err := srv.Apply(&next); err != nil {
		t.Fatal(err)
	}
	if len(p.Snapshot()) != 2 {
		t.Errorf("reload dropped discovered backends: %+v", p.Snapshot())
	}
}
