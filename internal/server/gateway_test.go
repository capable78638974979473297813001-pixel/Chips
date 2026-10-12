package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/extproc"
)

func gatewayReq(body string, h ...string) *extproc.Request {
	r := &extproc.Request{Method: "POST", Path: "/v1/chat/completions", Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
	for i := 0; i+1 < len(h); i += 2 {
		r.Header.Set(h[i], h[i+1])
	}
	return r
}

func endpointOf(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return hostPort(u)
}

func TestGatewayPicksAndFollowsResponse(t *testing.T) {
	a, b := newFake(t, nil), newFake(t, nil)
	var srv *Server
	_, p := setupServer(t, func(c *config.Config) {
		c.Models.Aliases = map[string]string{"gpt-4o": "llama"}
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k"}}
	}, &srv, a.URL, b.URL)
	pick := gatewayPicker{srv}

	d := pick.Pick(context.Background(), gatewayReq(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello there"}]}`, "Authorization", "Bearer k"))
	if d.Reject != nil {
		t.Fatalf("rejected: %d %s", d.Reject.Status, d.Reject.Body)
	}
	if d.Endpoint != endpointOf(t, a.URL) && d.Endpoint != endpointOf(t, b.URL) {
		t.Fatalf("endpoint %q is not a backend", d.Endpoint)
	}
	if !strings.Contains(string(d.Body), `"model":"llama"`) {
		t.Errorf("alias not rewritten in body: %s", d.Body)
	}
	if got := p.Snapshot()[0].Inflight + p.Snapshot()[1].Inflight; got != 1 {
		t.Errorf("in flight while the gateway runs it: %d", got)
	}
	d.Exchange.ResponseHeaders(200, http.Header{"Content-Type": {"application/json"}})
	d.Exchange.ResponseBody([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`), true)
	d.Exchange.Close()
	d.Exchange.Close() // once only
	if got := p.Snapshot()[0].Inflight + p.Snapshot()[1].Inflight; got != 0 {
		t.Errorf("slot not released: %d", got)
	}
	for _, u := range srv.limits.Snapshot() {
		if u.Client == "team" && (u.PromptTokens != 7 || u.CompletionTokens != 3 || u.InFlight != 0) {
			t.Errorf("usage %+v", u)
		}
	}
	// The next turn of the conversation goes to the same pod.
	next := pick.Pick(context.Background(), gatewayReq(`{"model":"llama","messages":[{"role":"user","content":"hello there"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`, "Authorization", "Bearer k"))
	if next.Endpoint != d.Endpoint {
		t.Errorf("follow-up went to %s, first turn to %s", next.Endpoint, d.Endpoint)
	}
	next.Exchange.Close()
}

func TestGatewayRejectsAndSubset(t *testing.T) {
	a, b := newFake(t, nil), newFake(t, nil)
	var srv *Server
	setupServer(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k", MaxConcurrent: 1}}
	}, &srv, a.URL, b.URL)
	pick := gatewayPicker{srv}
	body := `{"model":"llama","messages":[]}`

	if d := pick.Pick(context.Background(), gatewayReq(body)); d.Reject == nil || d.Reject.Status != 401 {
		t.Fatalf("no key: %+v", d)
	}
	sub := gatewayReq(body, "Authorization", "Bearer k")
	sub.Subset = []string{endpointOf(t, b.URL)}
	d := pick.Pick(context.Background(), sub)
	if d.Endpoint != endpointOf(t, b.URL) {
		t.Fatalf("subset ignored: %+v", d)
	}
	// The key's concurrency limit holds while the gateway runs the request.
	if d2 := pick.Pick(context.Background(), gatewayReq(body, "Authorization", "Bearer k")); d2.Reject == nil || d2.Reject.Status != 429 || d2.Reject.Header[0][0] != "retry-after" {
		t.Errorf("over the limit: %+v", d2)
	}
	d.Exchange.Close()

	none := gatewayReq(body, "Authorization", "Bearer k")
	none.Subset = []string{"10.9.9.9:8000"}
	if d := pick.Pick(context.Background(), none); d.Reject == nil || d.Reject.Status != 503 {
		t.Errorf("subset with no known endpoint: %+v", d)
	}
}
