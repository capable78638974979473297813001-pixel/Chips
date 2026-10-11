package server

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// chaosBackend misbehaves in the way each request's X-Chaos header asks
// for (the router forwards it), so a test can mix faults deterministically.
func chaosBackend(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	hijack := func() (close func(), write func(string)) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			panic(err)
		}
		return func() { conn.Close() }, func(s string) { conn.Write([]byte(s)) }
	}
	switch r.Header.Get("X-Chaos") {
	case "ok":
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	case "stream-ok":
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 5 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	case "reset-mid-stream": // headers and a token, then the connection dies
		closeConn, write := hijack()
		write("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		write("1d\r\ndata: {\"choices\":[{}]}\n\n\r\n")
		closeConn()
	case "garbage": // not HTTP at all
		closeConn, write := hijack()
		write("\x00\x01SSH-2.0-nonsense\r\n\r\n")
		closeConn()
	case "close-no-response":
		closeConn, _ := hijack()
		closeConn()
	case "hang-after-headers": // until the client gives up
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case "500-big":
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, strings.Repeat("x", 1<<20))
	case "503":
		http.Error(w, "busy", http.StatusServiceUnavailable)
	case "slow-drip": // one byte at a time
		for i := 0; i < 200 && r.Context().Err() == nil; i++ {
			io.WriteString(w, " ")
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}
}

var chaosKinds = []string{"ok", "stream-ok", "reset-mid-stream", "garbage", "close-no-response",
	"hang-after-headers", "500-big", "503", "slow-drip"}

// TestChaos runs a few hundred concurrent requests against backends that
// fail in every way above, with clients that hang up at random, then
// checks that nothing leaked: backend slots, queue places, per-client
// in-flight counts and goroutines all return to where they started, and
// the router still serves.
func TestChaos(t *testing.T) {
	a, b := newFake(t, chaosBackend), newFake(t, chaosBackend)
	ts, p := setup(t, func(c *config.Config) {
		c.Backends[0].MaxConcurrency, c.Backends[1].MaxConcurrency = 3, 3
		c.Queue.MaxWaiting = 20
		c.Queue.MaxWait = config.Duration(2 * time.Second)
		c.Retry.FirstTokenTimeout = config.Duration(200 * time.Millisecond)
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k", MaxConcurrent: 50}}
	}, a.URL, b.URL)
	srv := ts.Config.Handler.(*Server)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	codes := sync.Map{}
	for i := range 300 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rnd := rand.New(rand.NewPCG(uint64(i), 7))
			kind := chaosKinds[rnd.IntN(len(chaosKinds))]
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(50+rnd.IntN(1500))*time.Millisecond)
			defer cancel()
			stream := strings.HasPrefix(kind, "stream") || kind == "reset-mid-stream" || kind == "hang-after-headers"
			body := fmt.Sprintf(`{"model":"llama","stream":%v,"messages":[{"role":"user","content":"chaos %d"}]}`, stream, i)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer k")
			req.Header.Set("X-Chaos", kind)
			resp, err := client.Do(req)
			if err != nil {
				codes.Store(kind+" client error", true)
				return
			}
			if rnd.IntN(3) == 0 { // hang up partway through the body
				io.CopyN(io.Discard, resp.Body, int64(rnd.IntN(64)))
			} else {
				io.Copy(io.Discard, resp.Body)
			}
			resp.Body.Close()
			codes.Store(fmt.Sprintf("%s %d", kind, resp.StatusCode), true)
		}()
	}
	wg.Wait()

	// Everything the router held must be given back.
	waitUntil(t, func() bool {
		for _, st := range p.Snapshot() {
			if st.Inflight != 0 {
				return false
			}
		}
		return p.QueueDepth() == 0
	})
	for _, u := range srv.limits.Snapshot() {
		if u.InFlight != 0 {
			t.Errorf("client %s still has %d requests in flight", u.Client, u.InFlight)
		}
	}
	// Passive health took the misbehaving backends out; the active checks
	// that run in production bring them back.
	checker := &pool.Checker{Pool: p, Client: client, Path: "/v1/models", Interval: time.Hour, Timeout: time.Second}
	for range 3 {
		checker.CheckAll(context.Background())
	}
	// And the router still works.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama"}`))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("X-Chaos", "ok")
	if resp, err := client.Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("router broken after chaos: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	client.CloseIdleConnections()
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+20 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+20 {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
	if testing.Verbose() {
		codes.Range(func(k, _ any) bool { t.Log(k); return true })
	}
}
