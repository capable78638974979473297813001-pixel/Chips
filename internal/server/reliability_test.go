package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPanicInHandlerIsContained(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, nil, f.URL)
	srv := ts.Config.Handler.(*Server)
	srv.mux.HandleFunc("GET /test/panic", func(http.ResponseWriter, *http.Request) { panic("bug") })

	resp, err := http.Get(ts.URL + "/test/panic")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "internal_error") {
		t.Errorf("panic answered %d %s", resp.StatusCode, body)
	}
	if r := chat(t, ts.URL, `{"model":"llama"}`); r.StatusCode != 200 {
		t.Errorf("router not serving after a panic: %d", r.StatusCode)
	}
	m, _ := http.Get(ts.URL + "/metrics")
	mb, _ := io.ReadAll(m.Body)
	m.Body.Close()
	if !strings.Contains(string(mb), "siliconrouter_panics_total 1") {
		t.Error("panic not counted")
	}
}

func TestDrainingFailsReadinessOnly(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, nil, f.URL)
	ts.Config.Handler.(*Server).SetDraining(true)
	if code := get(t, ts.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz while draining: %d", code)
	}
	if code := get(t, ts.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz while draining: %d (a liveness failure would restart the pod mid-drain)", code)
	}
	if r := chat(t, ts.URL, `{"model":"llama"}`); r.StatusCode != 200 {
		t.Errorf("requests must still be served while draining: %d", r.StatusCode)
	}
}
