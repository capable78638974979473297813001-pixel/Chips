package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
)

// exactTokens asks b's engine how many tokens the request's prompt is,
// using its tokenize API. It returns ok=false on any failure, and the
// caller keeps its estimate.
func (s *Server) exactTokens(ctx context.Context, b *pool.Backend, model string, body []byte, info prompt.Info) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var payload any
	switch b.Tokenize {
	case "vllm":
		// vLLM tokenizes chat requests with the model's chat template, so
		// the count includes the template's own tokens.
		if msgs := prompt.Field(body, "messages"); msgs != nil {
			payload = map[string]any{"model": model, "messages": json.RawMessage(msgs)}
		} else {
			payload = map[string]any{"model": model, "prompt": promptString(body, info)}
		}
	case "llamacpp":
		payload = map[string]any{"content": promptString(body, info)}
	case "tgi":
		payload = map[string]any{"inputs": promptString(body, info)}
	default:
		return 0, false
	}
	raw, _ := json.Marshal(payload)
	u := *b.URL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/tokenize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	b.SetAuth(req.Header)
	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Debug("tokenize failed", "backend", b.Name, "error", err)
		return 0, false
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		s.log.Debug("tokenize failed", "backend", b.Name, "status", resp.Status)
		return 0, false
	}
	n, err := parseTokenCount(b.Tokenize, out)
	if err != nil {
		s.log.Debug("tokenize answer not understood", "backend", b.Name, "error", err)
		return 0, false
	}
	return n, true
}

// promptString is the request's prompt as plain text for engines that
// tokenize a string: the prompt field if there is one, else the router's
// rendering of the messages.
func promptString(body []byte, info prompt.Info) string {
	for _, key := range []string{"prompt", "inputs", "input"} {
		var s string
		if v := prompt.Field(body, key); v != nil && json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return info.Text
}

func parseTokenCount(engine string, body []byte) (int, error) {
	switch engine {
	case "vllm": // {"count": 123, "max_model_len": ..., "tokens": [...]}
		var v struct {
			Count *int `json:"count"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Count == nil {
			return 0, fmt.Errorf("no count in %.100s", body)
		}
		return *v.Count, nil
	case "llamacpp": // {"tokens": [1, 2, 3]}
		var v struct {
			Tokens []json.RawMessage `json:"tokens"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Tokens == nil {
			return 0, fmt.Errorf("no tokens in %.100s", body)
		}
		return len(v.Tokens), nil
	case "tgi": // [{"id": 1, "text": "..."}, ...]
		var v []json.RawMessage
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, err
		}
		return len(v), nil
	}
	return 0, fmt.Errorf("unknown engine %q", engine)
}
