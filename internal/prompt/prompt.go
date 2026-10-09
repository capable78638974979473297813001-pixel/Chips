// Package prompt pulls the routing-relevant facts out of an OpenAI-style
// request body: which model, how big the prompt is, and a chain of prefix
// hashes that lets the router recognise a conversation it has seen before.
package prompt

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"strings"
)

// Info describes one inference request.
type Info struct {
	Model  string
	Stream bool
	// MaxTokens is the requested completion budget, 0 if unspecified.
	MaxTokens int
	// Text is a canonical rendering of the prompt used for prefix matching.
	// Two requests whose prompts start the same way have Texts that start the same way.
	Text string
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type request struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Messages            []message       `json:"messages"`
	Prompt              json.RawMessage `json:"prompt"`
	Input               json.RawMessage `json:"input"`
}

// Parse reads a chat-completions, completions or embeddings request body.
func Parse(body []byte) (Info, error) {
	var r request
	if err := json.Unmarshal(body, &r); err != nil {
		return Info{}, err
	}
	info := Info{Model: r.Model, Stream: r.Stream}
	switch {
	case r.MaxCompletionTokens != nil:
		info.MaxTokens = *r.MaxCompletionTokens
	case r.MaxTokens != nil:
		info.MaxTokens = *r.MaxTokens
	}

	var sb strings.Builder
	for _, m := range r.Messages {
		// Record and unit separators keep "ab"+"c" distinct from "a"+"bc".
		sb.WriteByte(0x1e)
		sb.WriteString(m.Role)
		sb.WriteByte(0x1f)
		sb.WriteString(contentText(m.Content))
	}
	if len(r.Messages) == 0 {
		raw := r.Prompt
		if len(raw) == 0 {
			raw = r.Input
		}
		sb.WriteString(contentText(raw))
	}
	info.Text = sb.String()
	return info, nil
}

// contentText flattens a message content field. Strings are used as-is; for
// multi-part content the text parts are concatenated and any other part
// (images, audio) contributes its raw JSON so it still affects the prefix.
func contentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			var part struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(p, &part) == nil && part.Type == "text" {
				sb.WriteString(part.Text)
				continue
			}
			var str string
			if json.Unmarshal(p, &str) == nil {
				sb.WriteString(str)
				continue
			}
			sb.Write(p)
		}
		return sb.String()
	}
	return string(raw)
}

// EstimateTokens approximates a token count from character length. It is
// deliberately tokenizer-agnostic: routing only needs the order of magnitude.
func EstimateTokens(text string, charsPerToken float64) int {
	if text == "" {
		return 0
	}
	return int(float64(len(text))/charsPerToken) + 1
}

// BlockHashes returns one hash per full block of blockChars bytes, each
// covering everything from the start of the text to the end of that block.
// Hash i of two texts is equal exactly when their first (i+1)*blockChars
// bytes are equal (barring collisions), which is how inference engines key
// their prefix caches too. At most maxBlocks hashes are returned.
func BlockHashes(text string, blockChars, maxBlocks int) []uint64 {
	n := len(text) / blockChars
	if n > maxBlocks {
		n = maxBlocks
	}
	if n == 0 {
		return nil
	}
	out := make([]uint64, n)
	h := fnv.New64a()
	for i := range n {
		h.Write([]byte(text[i*blockChars : (i+1)*blockChars]))
		out[i] = h.Sum64()
	}
	return out
}

// SessionKey hashes an explicit client session ID into the same key space as
// block hashes so it can be looked up alongside them.
func SessionKey(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte("session\x00"))
	h.Write([]byte(id))
	return h.Sum64()
}
