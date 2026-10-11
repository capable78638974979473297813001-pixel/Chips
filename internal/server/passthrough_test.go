package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// wsEcho answers a WebSocket upgrade and then echoes every byte, standing
// in for a realtime API.
func wsEcho(t *testing.T, seen chan<- string) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"gpt-realtime"}]}`)
			return
		}
		seen <- r.URL.RequestURI() + " " + r.Header.Get("Authorization")
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		io.Copy(conn, rw)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestWebSocketPassthrough(t *testing.T) {
	seen := make(chan string, 1)
	backend := wsEcho(t, seen)
	ts, p := setup(t, func(c *config.Config) {
		c.Auth.APIKeys = []string{"router-key"}
		c.Backends[0].APIKey = "upstream-key"
		c.Models.Aliases = map[string]string{"realtime": "gpt-realtime"}
	}, backend.URL)

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /v1/realtime?model=realtime HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer router-key\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 || resp.Header.Get("X-SiliconRouter-Backend") != "b0" {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}
	if got := <-seen; got != "/v1/realtime?model=gpt-realtime Bearer upstream-key" {
		t.Errorf("backend saw %q (alias and backend key should apply)", got)
	}
	// The session holds a slot while open.
	if st := p.Snapshot()[0]; st.Inflight != 1 {
		t.Errorf("in flight during session = %d, want 1", st.Inflight)
	}
	fmt.Fprint(conn, "frame-bytes")
	buf := make([]byte, 11)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "frame-bytes" {
		t.Errorf("echo = %q, %v", buf, err)
	}
	conn.Close()
	for i := 0; p.Snapshot()[0].Inflight != 0; i++ {
		if i > 200 {
			t.Fatal("slot not released after the session closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func h2cClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p}}
}

func TestGRPCPassthrough(t *testing.T) {
	// A gRPC server is HTTP/2 with trailers carrying the status.
	grpcBackend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Header.Get("Content-Type") != "application/grpc" {
			http.Error(w, "not grpc over http/2: "+r.Proto, 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(200)
		w.Write(append([]byte("echo:"), body...))
		w.Header().Set("Grpc-Status", "0")
		w.Header().Set("Grpc-Message", r.URL.Path)
	}))
	var bp http.Protocols
	bp.SetUnencryptedHTTP2(true)
	grpcBackend.Config.Protocols = &bp
	grpcBackend.Start()
	defer grpcBackend.Close()

	plain := newFake(t, nil) // an HTTP-only backend: must not get gRPC
	router := httptest.NewUnstartedServer(nil)
	var rp http.Protocols
	rp.SetHTTP1(true)
	rp.SetUnencryptedHTTP2(true)
	router.Config.Protocols = &rp
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[1].GRPCURL = grpcBackend.URL
	}, plain.URL, plain.URL)
	router.Config.Handler = ts.Config.Handler
	router.Start()
	defer router.Close()

	req, _ := http.NewRequest(http.MethodPost, router.URL+"/inference.GRPCInferenceService/ModelInfer", strings.NewReader("\x00\x00\x00\x00\x03abc"))
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 || string(body) != "echo:\x00\x00\x00\x00\x03abc" {
		t.Fatalf("proto %s status %d body %q", resp.Proto, resp.StatusCode, body)
	}
	if resp.Trailer.Get("Grpc-Status") != "0" || resp.Trailer.Get("Grpc-Message") != "/inference.GRPCInferenceService/ModelInfer" {
		t.Errorf("trailers lost: %v", resp.Trailer)
	}
	if resp.Header.Get("X-SiliconRouter-Backend") != "b1" {
		t.Errorf("went to %s, want b1 (the only backend with grpc_url)", resp.Header.Get("X-SiliconRouter-Backend"))
	}
}

func TestGRPCWithoutGRPCBackend(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, nil, f.URL)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/svc/Method", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 501 || f.hits.Load() != 0 {
		t.Errorf("status %d, backend hits %d", resp.StatusCode, f.hits.Load())
	}
}
