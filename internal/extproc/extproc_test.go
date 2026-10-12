package extproc

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a fake Envoy ---

type envoy struct {
	t    *testing.T
	pw   *io.PipeWriter
	resp *http.Response
}

func dial(t *testing.T, url string) *envoy {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, url+processPath, pr)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	client := h2cClient()
	type result struct {
		resp *http.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		ch <- result{resp, err}
	}()
	// The server answers headers at once; the first frame write unblocks Do.
	e := &envoy{t: t, pw: pw}
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		e.resp = r.resp
	case <-time.After(2 * time.Second):
		t.Fatal("no response headers")
	}
	t.Cleanup(func() { pw.Close(); e.resp.Body.Close() })
	return e
}

func h2cClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p}}
}

func (e *envoy) send(msg enc) {
	e.t.Helper()
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
	if _, err := e.pw.Write(append(hdr[:], msg...)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *envoy) recv() *response {
	e.t.Helper()
	done := make(chan []byte, 1)
	go func() {
		b, err := readFrame(e.resp.Body, 32<<20)
		if err != nil {
			done <- nil
			return
		}
		done <- b
	}()
	select {
	case b := <-done:
		if b == nil {
			e.t.Fatalf("stream ended; grpc-status %q %q", e.resp.Trailer.Get("Grpc-Status"), e.resp.Trailer.Get("Grpc-Message"))
		}
		return decodeResponse(e.t, b)
	case <-time.After(2 * time.Second):
		e.t.Fatal("no response message")
		return nil
	}
}

func headersMsg(field int, h [][2]string, eos bool) enc {
	var hm enc
	for _, kv := range h {
		hm = hm.msg(1, enc(nil).str(1, kv[0]).bytes(3, []byte(kv[1])))
	}
	return enc(nil).msg(field, enc(nil).msg(1, hm).boolean(3, eos))
}

func bodyMsg(field int, b []byte, eos bool) enc {
	return enc(nil).msg(field, enc(nil).bytes(1, b).boolean(2, eos))
}

func withProtocol(m enc, reqMode, respMode int) enc {
	return m.msg(11, enc(nil).varint(1, uint64(reqMode)).varint(2, uint64(respMode)))
}

func withSubset(m enc, endpoints ...string) enc {
	var list enc
	for _, ep := range endpoints {
		list = list.msg(1, enc(nil).str(3, ep))
	}
	value := enc(nil).msg(6, list)
	st := enc(nil).msg(1, enc(nil).str(1, SubsetKey).msg(2, value))
	return m.msg(8, enc(nil).msg(1, enc(nil).str(1, SubsetNamespace).msg(2, st)))
}

// response is a decoded ProcessingResponse.
type response struct {
	field       int
	set         map[string]string
	removed     []string
	body        []byte // streamed or replaced body
	bodyEOS     bool
	clearRoute  bool
	status      int // immediate response
	immBody     string
	metadataEPs map[string]string
}

func decodeResponse(t *testing.T, b []byte) *response {
	r := &response{set: map[string]string{}, metadataEPs: map[string]string{}}
	common := func(c []byte) {
		fields(c, func(n, w int, v uint64, s []byte) error {
			switch n {
			case 2:
				decodeMutation(s, r)
			case 3:
				fields(s, func(n, w int, v uint64, s []byte) error {
					switch n {
					case 1:
						r.body = s
					case 3:
						fields(s, func(n, w int, v uint64, s []byte) error {
							if n == 1 {
								r.body = s
							}
							if n == 2 {
								r.bodyEOS = v != 0
							}
							return nil
						})
					}
					return nil
				})
			case 5:
				r.clearRoute = v != 0
			}
			return nil
		})
	}
	err := fields(b, func(num, wire int, v uint64, s []byte) error {
		switch {
		case num >= 1 && num <= 4:
			r.field = num
			fields(s, func(n, w int, v uint64, c []byte) error {
				if n == 1 {
					common(c)
				}
				return nil
			})
		case num == 5 || num == 6:
			r.field = num
		case num == outImmediate:
			r.field = num
			fields(s, func(n, w int, v uint64, x []byte) error {
				switch n {
				case 1:
					fields(x, func(n, w int, v uint64, _ []byte) error { r.status = int(v); return nil })
				case 2:
					decodeMutation(x, r)
				case 3:
					r.immBody = string(x)
				}
				return nil
			})
		case num == 8:
			md, _ := decodeStruct(s)
			if lb, ok := md[MetadataNamespace].(map[string]any); ok {
				for k, v := range lb {
					r.metadataEPs[k], _ = v.(string)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func decodeMutation(b []byte, r *response) {
	fields(b, func(n, w int, v uint64, s []byte) error {
		switch n {
		case 1:
			var key, val string
			var action uint64
			fields(s, func(n, w int, v uint64, hv []byte) error {
				if n == 1 {
					fields(hv, func(n, w int, v uint64, x []byte) error {
						if n == 1 {
							key = string(x)
						} else {
							val = string(x)
						}
						return nil
					})
				}
				if n == 3 {
					action = v
				}
				return nil
			})
			if action != overwrite {
				r.set["!append:"+key] = val
			}
			r.set[key] = val
		case 2:
			r.removed = append(r.removed, string(s))
		}
		return nil
	})
}

// --- a fake picker ---

type picker struct {
	mu       sync.Mutex
	seen     []*Request
	decision func(*Request) Decision
}

func (p *picker) Pick(_ context.Context, r *Request) Decision {
	p.mu.Lock()
	p.seen = append(p.seen, r)
	p.mu.Unlock()
	return p.decision(r)
}

type exchange struct {
	mu     sync.Mutex
	status int
	body   bytes.Buffer
	closed int
}

func (x *exchange) ResponseHeaders(status int, _ http.Header) {
	x.mu.Lock()
	x.status = status
	x.mu.Unlock()
}
func (x *exchange) ResponseBody(b []byte, _ bool) { x.mu.Lock(); x.body.Write(b); x.mu.Unlock() }
func (x *exchange) Close()                        { x.mu.Lock(); x.closed++; x.mu.Unlock() }

func newServer(t *testing.T, p Picker, ready func() bool) string {
	s := &Server{Picker: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Ready: ready, MaxBody: 1 << 20}
	ts := httptest.NewUnstartedServer(s)
	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	ts.Config.Protocols = &protos
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL
}

var reqHdrs = [][2]string{{":method", "POST"}, {":path", "/v1/chat/completions"}, {":authority", "gw"}, {"content-type", "application/json"}, {"x-session-id", "s1"}}

// --- tests ---

func TestFullDuplexPicksAfterBody(t *testing.T) {
	x := &exchange{}
	p := &picker{decision: func(r *Request) Decision {
		return Decision{Endpoint: "10.0.0.7:8000", Body: []byte(strings.Replace(string(r.Body), "gpt-4o", "llama", 1)), Exchange: x}
	}}
	e := dial(t, newServer(t, p, nil))

	e.send(headersMsg(reqHeaders, reqHdrs, false))
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"` + strings.Repeat("x", 100000) + `"}]}`)
	e.send(bodyMsg(reqBody, body[:50000], false))
	e.send(bodyMsg(reqBody, body[50000:], true))

	h := e.recv()
	if h.field != outReqHeaders || h.set[DestinationHeader] != "10.0.0.7:8000" || !h.clearRoute || h.metadataEPs[DestinationHeader] != "10.0.0.7:8000" {
		t.Fatalf("headers response %+v", h)
	}
	if _, appended := h.set["!append:"+DestinationHeader]; appended {
		t.Error("destination header must overwrite one the client sent")
	}
	if len(h.removed) != 1 || h.removed[0] != "content-length" {
		t.Errorf("removed %v: a rewritten body needs its length dropped", h.removed)
	}
	var got []byte
	for {
		b := e.recv()
		if b.field != outReqBody {
			t.Fatalf("expected body chunks, got field %d", b.field)
		}
		if len(b.body) > bodyChunk {
			t.Fatalf("chunk of %d bytes", len(b.body))
		}
		got = append(got, b.body...)
		if b.bodyEOS {
			break
		}
	}
	if !bytes.Contains(got, []byte(`"model":"llama"`)) || len(got) != len(body)-1 {
		t.Errorf("body sent back: %d bytes, %q...", len(got), got[:40])
	}
	r := p.seen[0]
	if r.Method != "POST" || r.Path != "/v1/chat/completions" || r.Header.Get("X-Session-Id") != "s1" || r.Subset != nil {
		t.Errorf("request seen by picker: %+v", r)
	}

	// The response passes through and is reported to the exchange.
	e.send(headersMsg(respHeaders, [][2]string{{":status", "200"}}, false))
	if rh := e.recv(); rh.field != outRespHeaders {
		t.Fatalf("response headers answer: %+v", rh)
	}
	e.send(bodyMsg(respBody, []byte(`{"usage":{"prompt_tokens":5}}`), true))
	if rb := e.recv(); rb.field != outRespBody || string(rb.body) != `{"usage":{"prompt_tokens":5}}` || !rb.bodyEOS {
		t.Fatalf("response body answer: %+v", rb)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.status != 200 || !strings.Contains(x.body.String(), "prompt_tokens") || x.closed != 1 {
		t.Errorf("exchange saw status %d body %q closed %d", x.status, x.body.String(), x.closed)
	}
}

func TestRejectAndSubset(t *testing.T) {
	x := &exchange{}
	p := &picker{decision: func(r *Request) Decision {
		return Decision{Reject: &Reject{Status: 429, Header: [][2]string{{"retry-after", "1"}}, Body: []byte(`{"error":"busy"}`)}, Exchange: x}
	}}
	e := dial(t, newServer(t, p, nil))
	e.send(withSubset(headersMsg(reqHeaders, reqHdrs, false), "10.0.0.1:8000", "10.0.0.2:8000"))
	e.send(bodyMsg(reqBody, []byte(`{}`), true))
	im := e.recv()
	if im.field != outImmediate || im.status != 429 || im.set["retry-after"] != "1" || im.immBody != `{"error":"busy"}` {
		t.Fatalf("immediate response %+v", im)
	}
	if got := p.seen[0].Subset; len(got) != 2 || got[1] != "10.0.0.2:8000" {
		t.Errorf("subset %v", got)
	}
	if x.closed != 1 {
		t.Error("a rejected request's exchange must be closed")
	}

	// No endpoint and no rejection is a 503.
	p.decision = func(*Request) Decision { return Decision{} }
	e2 := dial(t, newServer(t, p, nil))
	e2.send(headersMsg(reqHeaders, [][2]string{{":method", "GET"}, {":path", "/v1/models"}}, true))
	if im := e2.recv(); im.status != 503 {
		t.Errorf("empty decision: %+v", im)
	}
}

func TestBufferedMode(t *testing.T) {
	p := &picker{decision: func(r *Request) Decision {
		return Decision{Endpoint: "10.0.0.9:8000", Body: []byte(`{"model":"real"}`)}
	}}
	e := dial(t, newServer(t, p, nil))
	e.send(withProtocol(headersMsg(reqHeaders, reqHdrs, false), modeBuffered, modeNone))
	if h := e.recv(); h.field != outReqHeaders || len(h.set) != 0 {
		t.Fatalf("buffered: headers should pass unchanged first, got %+v", h)
	}
	e.send(bodyMsg(reqBody, []byte(`{"model":"alias"}`), true))
	b := e.recv()
	if b.field != outReqBody || b.set[DestinationHeader] != "10.0.0.9:8000" || string(b.body) != `{"model":"real"}` {
		t.Fatalf("buffered body response %+v", b)
	}
}

func TestHealth(t *testing.T) {
	ready := false
	url := newServer(t, &picker{}, func() bool { return ready })
	check := func(service string) (int, string) {
		msg := enc(nil).str(1, service)
		var hdr [5]byte
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
		req, _ := http.NewRequest(http.MethodPost, url+healthPath, bytes.NewReader(append(hdr[:], msg...)))
		req.Header.Set("Content-Type", "application/grpc")
		resp, err := h2cClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := readFrame(resp.Body, 1024)
		io.Copy(io.Discard, resp.Body)
		status := 0
		fields(b, func(n, w int, v uint64, _ []byte) error { status = int(v); return nil })
		code := resp.Trailer.Get("Grpc-Status")
		if code == "" {
			code = resp.Header.Get("Grpc-Status")
		}
		return status, code
	}
	if s, c := check("liveness"); s != 1 || c != "0" {
		t.Errorf("liveness %d %s", s, c)
	}
	if s, _ := check(serviceName); s != 2 {
		t.Errorf("not ready: %d", s)
	}
	ready = true
	if s, _ := check("readiness"); s != 1 {
		t.Errorf("ready: %d", s)
	}
	if _, c := check("bogus"); c != "5" {
		t.Errorf("unknown service: grpc-status %s", c)
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	for _, b := range [][]byte{{0x12}, {0x12, 0x05, 1}, {0x08}} {
		if _, err := decodeProcessingRequest(b); err == nil {
			t.Errorf("%x decoded", b)
		}
	}
}
