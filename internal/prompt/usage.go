package prompt

import "bytes"

// Usage is the token count a backend reported for one response. A field is
// -1 when the response did not report it.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// ParseUsage finds the token counts in the end of a response body. Every
// API puts them near the end: in the body of a JSON response, or in the
// last events of a stream. It understands:
//
//	OpenAI and compatible engines:  "usage": {"prompt_tokens", "completion_tokens"}
//	Anthropic:                      "usage": {"input_tokens", "output_tokens"}
//	Gemini:                         "usageMetadata": {"promptTokenCount", "candidatesTokenCount"}
//	Ollama native:                  "prompt_eval_count", "eval_count"
func ParseUsage(tail []byte) Usage {
	u := Usage{-1, -1}
	if v := lastValue(tail, `"usage"`); len(v) > 0 && v[0] == '{' {
		u.PromptTokens = firstInt(v, "prompt_tokens", "input_tokens")
		u.CompletionTokens = firstInt(v, "completion_tokens", "output_tokens")
	}
	if v := lastValue(tail, `"usageMetadata"`); len(v) > 0 && v[0] == '{' && u.CompletionTokens < 0 {
		u.PromptTokens = firstInt(v, "promptTokenCount")
		u.CompletionTokens = firstInt(v, "candidatesTokenCount")
	}
	if u.CompletionTokens < 0 {
		if n, ok := numberValue(lastValue(tail, `"eval_count"`)); ok {
			u.CompletionTokens = int64(n)
			if p, ok := numberValue(lastValue(tail, `"prompt_eval_count"`)); ok {
				u.PromptTokens = int64(p)
			}
		}
	}
	return u
}

// lastValue returns the JSON value after the last occurrence of key (a
// quoted name) followed by a colon, or nil.
func lastValue(b []byte, key string) []byte {
	for end := len(b); end > 0; {
		i := bytes.LastIndex(b[:end], []byte(key))
		if i < 0 {
			return nil
		}
		j := skipSpace(b, i+len(key))
		if j < len(b) && b[j] == ':' {
			start := skipSpace(b, j+1)
			if stop, err := skipValue(b, start); err == nil {
				return b[start:stop]
			}
			return nil
		}
		end = i // the key text appeared as a value, keep looking
	}
	return nil
}

func firstInt(obj []byte, keys ...string) int64 {
	for _, k := range keys {
		if n, ok := numberValue(field(obj, k)); ok {
			return int64(n)
		}
	}
	return -1
}
