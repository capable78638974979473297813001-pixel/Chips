// Package extproc makes the router an endpoint picker for the Kubernetes
// Gateway API Inference Extension: a gateway (Envoy Gateway, Istio,
// kgateway, GKE, ...) asks it, over Envoy's external processing gRPC
// protocol (ext_proc), which model server pod should get each request, and
// then sends the request there itself.
//
// The protocol is the extension's endpoint picker protocol: the picker
// reads the request (headers and body), sets the x-gateway-destination-endpoint
// header and the envoy.lb dynamic metadata to the chosen "ip:port", honors
// a subset hint (envoy.lb.subset_hint, x-gateway-destination-endpoint-subset),
// and answers 503 or 429 with an ImmediateResponse when no endpoint can
// take the request. Bodies are processed in FULL_DUPLEX_STREAMED mode, which
// the extension requires; BUFFERED and NONE request body modes also work
// when Envoy announces them.
//
// gRPC runs on net/http's HTTP/2 server (h2c or TLS), and messages are
// encoded by hand (pb.go), so this adds no dependencies.
package extproc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// The protocol's names.
const (
	DestinationHeader = "x-gateway-destination-endpoint"
	MetadataNamespace = "envoy.lb"
	SubsetNamespace   = "envoy.lb.subset_hint"
	SubsetKey         = "x-gateway-destination-endpoint-subset"

	processPath = "/envoy.service.ext_proc.v3.ExternalProcessor/Process"
	healthPath  = "/grpc.health.v1.Health/Check"
	serviceName = "envoy.service.ext_proc.v3.ExternalProcessor"
)

// bodyChunk is the largest body chunk sent back to Envoy; some Envoy
// builds cap streamed chunks at 64 KiB.
const bodyChunk = 62000

// Request is what the picker sees of an HTTP request.
type Request struct {
	Method, Path, Authority string
	Header                  http.Header // regular headers, canonicalized
	Body                    []byte
	// Subset, when non-nil, is the only endpoints ("ip:port") the gateway
	// allows for this request. Empty but non-nil means none.
	Subset []string
}

// Decision is the picker's answer.
type Decision struct {
	// Endpoint is the chosen "ip:port". Empty with Reject unset is a 503.
	Endpoint string
	// Body, when non-nil, replaces the request body (e.g. a model alias
	// rewritten to its target).
	Body []byte
	// Header holds extra request headers to set.
	Header [][2]string
	// Reject answers the request instead of routing it.
	Reject *Reject
	// Exchange, when set, is told about the response and when the stream
	// ends (to release the slot the picker took).
	Exchange Exchange
}

// Reject is an HTTP answer given by the gateway on the picker's behalf.
type Reject struct {
	Status int
	Header [][2]string
	Body   []byte
}

// Exchange follows one request after its endpoint was picked.
type Exchange interface {
	ResponseHeaders(status int, h http.Header)
	ResponseBody(chunk []byte, end bool)
	// Close is called once, when the response ended or the stream closed.
	Close()
}

// Picker chooses endpoints.
type Picker interface {
	Pick(ctx context.Context, r *Request) Decision
}

// Server serves ext_proc and gRPC health checks.
type Server struct {
	Picker Picker
	Log    *slog.Logger
	// Ready reports readiness for health checks of "readiness" and the
	// ext_proc service ("liveness" is always serving).
	Ready func() bool
	// MaxBody bounds a request body the picker buffers.
	MaxBody int
	// DefaultMode is the request and response body mode assumed when
	// Envoy doesn't announce one (default FULL_DUPLEX_STREAMED).
	DefaultMode int
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		http.Error(w, "gRPC only", http.StatusUnsupportedMediaType)
		return
	}
	if enc := r.Header.Get("Grpc-Encoding"); enc != "" && enc != "identity" {
		grpcStatus(w, 12, "compression is not supported") // UNIMPLEMENTED
		return
	}
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	switch r.URL.Path {
	case processPath:
		s.process(w, r)
	case healthPath:
		s.health(w, r)
	default:
		grpcStatus(w, 12, "unknown method "+r.URL.Path)
	}
}

// grpcStatus ends a call with no messages.
func grpcStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Grpc-Status", fmt.Sprint(code))
	w.Header().Set("Grpc-Message", msg)
	w.WriteHeader(http.StatusOK)
}

func readFrame(r io.Reader, max int) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 0 {
		return nil, errors.New("compressed gRPC messages are not supported")
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if int64(n) > int64(max) {
		return nil, fmt.Errorf("gRPC message of %d bytes exceeds the limit", n)
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return buf, err
}

func writeFrame(w http.ResponseWriter, msg []byte) error {
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	msg, err := readFrame(r.Body, 4<<10)
	if err != nil {
		grpcStatus(w, 3, "bad request") // INVALID_ARGUMENT
		return
	}
	var service string
	fields(msg, func(num, wire int, _ uint64, b []byte) error {
		if num == 1 && wire == wireBytes {
			service = string(b)
		}
		return nil
	})
	var status uint64 = 1 // SERVING
	switch service {
	case "", "liveness":
	case "readiness", serviceName:
		if s.Ready != nil && !s.Ready() {
			status = 2 // NOT_SERVING
		}
	default:
		grpcStatus(w, 5, "unknown service "+service) // NOT_FOUND
		return
	}
	w.WriteHeader(http.StatusOK)
	writeFrame(w, enc(nil).varint(1, status))
	w.Header().Set("Grpc-Status", "0")
}

// stream is one HTTP request's ext_proc conversation.
type stream struct {
	s         *Server
	w         http.ResponseWriter
	ctx       context.Context
	reqMode   int
	respMode  int
	announced bool
	req       *Request
	body      []byte
	deferred  bool // full duplex: the headers response waits for the whole body
	decision  *Decision
	exchange  Exchange
	closed    bool
	maxBody   int
}

func (s *Server) process(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	rc.EnableFullDuplex()
	w.WriteHeader(http.StatusOK)
	rc.Flush() // response headers now: gRPC clients wait for them before streaming
	mode := s.DefaultMode
	if mode == 0 {
		mode = modeFullDuplex
	}
	st := &stream{s: s, w: w, ctx: r.Context(), reqMode: mode, respMode: mode, maxBody: s.MaxBody}
	if st.maxBody <= 0 {
		st.maxBody = 16 << 20
	}
	defer st.close()
	maxMsg := st.maxBody + 1<<20
	for {
		msg, err := readFrame(r.Body, maxMsg)
		if err != nil {
			if !errors.Is(err, io.EOF) && r.Context().Err() == nil {
				s.Log.Warn("ext_proc stream ended with an error", "error", err)
				w.Header().Set("Grpc-Status", "13") // INTERNAL
				w.Header().Set("Grpc-Message", err.Error())
				return
			}
			w.Header().Set("Grpc-Status", "0")
			return
		}
		pr, err := decodeProcessingRequest(msg)
		if err == nil {
			err = st.handle(pr)
		}
		if err != nil {
			s.Log.Warn("ext_proc stream failed", "error", err)
			w.Header().Set("Grpc-Status", "13")
			w.Header().Set("Grpc-Message", err.Error())
			return
		}
	}
}

func (st *stream) close() {
	if st.exchange != nil && !st.closed {
		st.closed = true
		st.exchange.Close()
	}
}

func (st *stream) send(msg []byte) error { return writeFrame(st.w, msg) }

func (st *stream) handle(pr *processingRequest) error {
	if pr.protocol != nil && !st.announced {
		st.announced = true
		st.reqMode, st.respMode = pr.protocol.requestBody, pr.protocol.responseBody
	}
	switch pr.kind {
	case reqHeaders:
		st.req = requestFrom(pr)
		switch {
		case pr.endOfStream || st.reqMode == modeNone || st.reqMode == modeStreamed:
			// No body coming, or it streams after routing: pick on headers alone.
			return st.decide(true)
		case st.reqMode == modeFullDuplex:
			st.deferred = true
			return nil
		default: // buffered: the body arrives after we let the headers through
			return st.send(processingResponse(outReqHeaders, enc(nil).msg(1, nil), nil))
		}

	case reqBody:
		if st.req == nil {
			return errors.New("request body before request headers")
		}
		if st.decision != nil { // streamed mode, already routed: let chunks through
			return st.send(processingResponse(outReqBody, enc(nil), nil))
		}
		if len(st.body)+len(pr.body) > st.maxBody {
			return st.reject(&Reject{Status: http.StatusRequestEntityTooLarge,
				Body: []byte(`{"error":{"message":"request body too large","code":"request_too_large"}}`)})
		}
		st.body = append(st.body, pr.body...)
		if !pr.endOfStream {
			return nil
		}
		st.req.Body = st.body
		return st.decide(false)

	case respHeaders:
		if st.exchange != nil {
			status := 0
			fmt.Sscan(firstOf(pr.headers[":status"]), &status)
			st.exchange.ResponseHeaders(status, pr.headers)
		}
		if err := st.send(processingResponse(outRespHeaders, enc(nil).msg(1, nil), nil)); err != nil {
			return err
		}
		if pr.endOfStream {
			st.close()
		}
		return nil

	case respBody:
		if st.exchange != nil {
			st.exchange.ResponseBody(pr.body, pr.endOfStream)
		}
		if st.respMode == modeFullDuplex {
			if err := st.sendBody(outRespBody, pr.body, pr.endOfStream); err != nil {
				return err
			}
		} else if err := st.send(processingResponse(outRespBody, enc(nil), nil)); err != nil {
			return err
		}
		if pr.endOfStream {
			st.close()
		}
		return nil

	case reqTrailers:
		return st.send(processingResponse(outReqTrailers, nil, nil))
	case respTrailers:
		err := st.send(processingResponse(outRespTrailers, nil, nil))
		st.close()
		return err
	}
	return nil
}

func firstOf(v []string) string {
	if len(v) > 0 {
		return v[0]
	}
	return ""
}

func requestFrom(pr *processingRequest) *Request {
	r := &Request{Header: http.Header{}}
	for k, v := range pr.headers {
		switch k {
		case ":method":
			r.Method = firstOf(v)
		case ":path":
			r.Path = firstOf(v)
		case ":authority":
			r.Authority = firstOf(v)
		default:
			if !strings.HasPrefix(k, ":") {
				r.Header[k] = v
			}
		}
	}
	if md := pr.metadata[SubsetNamespace]; md != nil {
		if list, ok := md[SubsetKey].([]any); ok {
			r.Subset = []string{}
			for _, e := range list {
				if s, ok := e.(string); ok {
					r.Subset = append(r.Subset, s)
				}
			}
		}
	}
	return r
}

// decide asks the picker and sends the answer. onHeaders is true when it
// answers the request headers message (no body is buffered).
func (st *stream) decide(onHeaders bool) error {
	d := st.s.Picker.Pick(st.ctx, st.req)
	st.decision = &d
	st.exchange = d.Exchange
	if d.Reject == nil && d.Endpoint == "" {
		d.Reject = &Reject{Status: http.StatusServiceUnavailable,
			Body: []byte(`{"error":{"message":"no endpoint available","code":"no_backend"}}`)}
	}
	if d.Reject != nil {
		return st.reject(d.Reject)
	}
	set := append([][2]string{{DestinationHeader, d.Endpoint}}, d.Header...)
	md := map[string]map[string]string{MetadataNamespace: {DestinationHeader: d.Endpoint}}
	body := st.body
	var remove []string
	if d.Body != nil {
		body = d.Body
		remove = []string{"content-length"} // the body's length changed; Envoy re-frames it
	}
	switch {
	case onHeaders:
		return st.send(processingResponse(outReqHeaders, enc(nil).msg(1, commonResponse(set, nil, nil, true)), md))
	case st.deferred: // full duplex: headers now, then the body back
		if err := st.send(processingResponse(outReqHeaders, enc(nil).msg(1, commonResponse(set, remove, nil, true)), md)); err != nil {
			return err
		}
		st.deferred = false
		return st.sendBody(outReqBody, body, true)
	default: // buffered: the mutation rides on the body response
		var bm enc
		if d.Body != nil {
			bm = replaceBody(d.Body)
		}
		return st.send(processingResponse(outReqBody, enc(nil).msg(1, commonResponse(set, remove, bm, true)), md))
	}
}

// sendBody streams body back to Envoy in chunks (full duplex mode).
func (st *stream) sendBody(field int, body []byte, end bool) error {
	for {
		n := min(len(body), bodyChunk)
		last := n == len(body)
		resp := processingResponse(field, enc(nil).msg(1, commonResponse(nil, nil, streamedBody(body[:n], end && last), false)), nil)
		if err := st.send(resp); err != nil {
			return err
		}
		body = body[n:]
		if last {
			return nil
		}
	}
}

func (st *stream) reject(r *Reject) error {
	st.close() // nothing will run: let go of whatever the picker held
	h := append([][2]string{{"content-type", "application/json"}}, r.Header...)
	return st.send(processingResponse(outImmediate, immediateResponse(r.Status, h, r.Body, "siliconrouter"), nil))
}
