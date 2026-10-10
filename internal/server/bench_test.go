package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// benchBody is a realistic multi-turn chat request: a system prompt and a
// few turns, about 8 KB.
var benchBody = func() string {
	var msgs []string
	msgs = append(msgs, fmt.Sprintf(`{"role":"system","content":%q}`, strings.Repeat("You are a helpful assistant. ", 60)))
	for i := range 6 {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":%q}`, strings.Repeat(fmt.Sprintf("question %d ", i), 80)))
		msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":%q}`, strings.Repeat("answer ", 60)))
	}
	return `{"model":"llama","temperature":0,"max_tokens":64,"messages":[` + strings.Join(msgs, ",") + `]}`
}()

func benchPost(b *testing.B, client *http.Client, url string) {
	b.Helper()
	b.SetBytes(int64(len(benchBody)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Post(url, "application/json", strings.NewReader(benchBody))
			if err != nil {
				b.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// BenchmarkDirect is the floor: the client talking to the backend itself.
func BenchmarkDirect(b *testing.B) {
	f := newFake(b, nil)
	benchPost(b, benchClient(), f.URL+"/v1/chat/completions")
}

// BenchmarkRouter is the same traffic through SiliconRouter. The difference
// from BenchmarkDirect is the router's overhead per request.
func BenchmarkRouter(b *testing.B) {
	f := newFake(b, nil)
	ts, _ := setup(b, func(c *config.Config) { c.Backends[0].MaxConcurrency = 1024 }, f.URL)
	benchPost(b, benchClient(), ts.URL+"/v1/chat/completions")
}

func benchClient() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
}

// BenchmarkRouterCacheHit is a repeated temperature-0 request answered from
// the response cache: no backend involved.
func BenchmarkRouterCacheHit(b *testing.B) {
	f := newFake(b, nil)
	ts, _ := setup(b, func(c *config.Config) {
		c.Backends[0].MaxConcurrency = 1024
		c.Cache.Enabled = true
	}, f.URL)
	benchPost(b, benchClient(), ts.URL+"/v1/chat/completions")
}
