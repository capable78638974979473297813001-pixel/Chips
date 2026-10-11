package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func TestExactTokensNearContextLimit(t *testing.T) {
	var tokenizeCalls atomic.Int64
	var tokenizeBody atomic.Value
	small := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokenize" {
			tokenizeCalls.Add(1)
			b, _ := io.ReadAll(r.Body)
			tokenizeBody.Store(string(b))
			io.WriteString(w, `{"count":1500,"max_model_len":1000}`) // the real count: too big
			return
		}
	})
	big := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].MaxContextTokens = 1000
		c.Backends[0].Tokenize = "vllm"
		c.Backends[1].MaxContextTokens = 100000
	}, small.URL, big.URL)

	// ~3600 characters: an estimate of ~900 tokens says it fits the small backend.
	body := `{"model":"llama","messages":[{"role":"user","content":"` + strings.Repeat("abcd", 900) + `"}]}`
	resp := chat(t, ts.URL, body)
	if got := resp.Header.Get("X-SiliconRouter-Backend"); got != "b1" {
		t.Errorf("went to %s, want b1: the exact count (1500) does not fit b0", got)
	}
	if tokenizeCalls.Load() != 1 {
		t.Fatalf("tokenize calls = %d, want 1", tokenizeCalls.Load())
	}
	var sent struct {
		Model    string
		Messages []map[string]string
	}
	json.Unmarshal([]byte(tokenizeBody.Load().(string)), &sent)
	if sent.Model != "llama" || len(sent.Messages) != 1 {
		t.Errorf("tokenize request = %s", tokenizeBody.Load())
	}

	// Far from every limit: no extra call.
	chat(t, ts.URL, `{"model":"llama","messages":[{"role":"user","content":"hi"}]}`)
	if tokenizeCalls.Load() != 1 {
		t.Errorf("tokenized a small prompt: %d calls", tokenizeCalls.Load())
	}
}

func TestTokenizeFailureFallsBackToEstimate(t *testing.T) {
	small := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokenize" {
			http.Error(w, "no", 500)
		}
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Backends[0].MaxContextTokens = 1000
		c.Backends[0].Tokenize = "vllm"
	}, small.URL)
	body := `{"model":"llama","messages":[{"role":"user","content":"` + strings.Repeat("abcd", 900) + `"}]}`
	if resp := chat(t, ts.URL, body); resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestParseTokenCount(t *testing.T) {
	for _, c := range []struct {
		engine, body string
		want         int
	}{
		{"vllm", `{"count":42,"max_model_len":8192,"tokens":[1,2]}`, 42},
		{"llamacpp", `{"tokens":[1,2,3,4]}`, 4},
		{"tgi", `[{"id":1,"text":"a","start":0,"stop":1},{"id":2,"text":"b","start":1,"stop":2}]`, 2},
	} {
		if got, err := parseTokenCount(c.engine, []byte(c.body)); err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", c.engine, got, err, c.want)
		}
	}
	if _, err := parseTokenCount("vllm", []byte(`{"tokens":[]}`)); err == nil {
		t.Error("vllm answer without count accepted")
	}
}
