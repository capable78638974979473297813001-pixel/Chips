package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

const clusterSecret = "a-long-shared-cluster-secret"

// twoRouters starts routers A and B, peers of each other, in front of the
// same backends.
func twoRouters(t *testing.T, mutate func(*config.Config), backends ...string) (a, b *httptest.Server) {
	t.Helper()
	ta := httptest.NewUnstartedServer(nil)
	tb := httptest.NewUnstartedServer(nil)
	for _, n := range []struct {
		ts   *httptest.Server
		name string
		peer *httptest.Server
	}{{ta, "router-a", tb}, {tb, "router-b", ta}} {
		var sb strings.Builder
		sb.WriteString("backends:\n")
		for i, u := range backends {
			fmt.Fprintf(&sb, "  - {name: gpu%d, url: %q, max_concurrency: 8}\n", i, u)
		}
		fmt.Fprintf(&sb, "cluster: {node_name: %s, secret: %s, peers: [%q, %q]}\n",
			n.name, clusterSecret, "http://"+ta.Listener.Addr().String(), "http://"+tb.Listener.Addr().String())
		cfg, err := config.Parse([]byte(sb.String()))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Routing.BlockChars = 16
		if mutate != nil {
			mutate(cfg)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := pool.New(cfg.Backends, pool.OptionsFromConfig(cfg), log)
		if err != nil {
			t.Fatal(err)
		}
		n.ts.Config.Handler = New(cfg, p, &http.Client{}, log)
		n.ts.Start()
		t.Cleanup(n.ts.Close)
	}
	return ta, tb
}

func syncNow(ts *httptest.Server) { ts.Config.Handler.(*Server).cluster.SyncNow(context.Background()) }

func TestConversationFollowsAcrossRouters(t *testing.T) {
	release := make(chan struct{})
	g0 := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), "hold") {
			<-release
		}
	})
	g1 := newFake(t, nil)
	a, b := twoRouters(t, nil, g0.URL, g1.URL)

	// Keep gpu0 busy on router A only, so A places the conversation on gpu1
	// while B, idle, would pick gpu0 on its own.
	done := make(chan struct{})
	go func() { chat(t, a.URL, `{"messages":[{"role":"user","content":"hold"}]}`); close(done) }()
	for g0.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := chat(t, a.URL, conversation(3)).Header.Get("X-SiliconRouter-Backend"); got != "gpu1" {
		t.Fatalf("router A put turn 1 on %s, want gpu1", got)
	}
	close(release)
	<-done

	// Control: without a sync, router B sends the next turn elsewhere.
	if got := chat(t, b.URL, conversation(4)).Header.Get("X-SiliconRouter-Backend"); got != "gpu0" {
		t.Fatalf("control: unsynced router B chose %s, want gpu0", got)
	}
	// Router B just learned gpu0 for turn 2's prefix; reset by syncing A's view.
	syncNow(a)
	for i := range 5 {
		resp := chat(t, b.URL, conversation(3+i))
		got := resp.Header.Get("X-SiliconRouter-Backend")
		if i == 0 && got != "gpu1" {
			t.Fatalf("after sync, turn 1's prefix via router B went to %s, want gpu1", got)
		}
		if resp.Header.Get("X-SiliconRouter-Prefix-Cache") != "hit" {
			t.Errorf("request %d via router B was not a prefix-cache hit", i)
		}
	}
}

func TestQuotaHoldsAcrossRouters(t *testing.T) {
	g := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":40,"completion_tokens":20}}`)
	})
	a, b := twoRouters(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k", TokensPerDay: 100}}
	}, g.URL)
	auth := []string{"Authorization", "Bearer k"}
	if code := chat(t, a.URL, `{}`, auth...).StatusCode; code != 200 { // 60 tokens on A
		t.Fatal(code)
	}
	if code := chat(t, b.URL, `{}`, auth...).StatusCode; code != 200 { // 60 on B: each alone is under 100
		t.Fatal(code)
	}
	// Usage is recorded after the response is relayed, so wait for it
	// before pushing it to the peer.
	for _, r := range []*httptest.Server{a, b} {
		waitUntil(t, func() bool {
			u := r.Config.Handler.(*Server).limits.Snapshot()
			return len(u) == 1 && u[0].TokensToday == 60
		})
	}
	syncNow(a)
	syncNow(b)
	for _, r := range []*httptest.Server{a, b} {
		if code := chat(t, r.URL, `{}`, auth...).StatusCode; code != 429 {
			t.Errorf("120 of 100 tokens used across the cluster, router still admits: %d", code)
		}
	}
}

func TestClusterRejectsWrongSecret(t *testing.T) {
	g := newFake(t, nil)
	a, _ := twoRouters(t, nil, g.URL)
	req, _ := http.NewRequest(http.MethodPost, a.URL+"/cluster/sync", strings.NewReader(`{"from":"evil","affinity":[{"k":1,"b":"gpu0"}]}`))
	req.Header.Set("Authorization", "Bearer guess")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong secret: %d", resp.StatusCode)
	}
}

func TestConcurrencyLimitIsClusterWide(t *testing.T) {
	release := make(chan struct{})
	g := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	a, b := twoRouters(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k", MaxConcurrent: 1}}
	}, g.URL)
	defer close(release)
	key := []string{"Authorization", "Bearer k"}

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, a.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set(key[0], key[1])
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	waitUntil(t, func() bool { return g.hits.Load() == 1 })
	syncNow(a) // B learns that team has a request in flight on A
	if resp := chat(t, b.URL, `{}`, key...); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("router B admitted a second concurrent request: %d", resp.StatusCode)
	}
}
