// Package prompt pulls the routing-relevant facts out of an inference
// request: which model, how big the prompt is, and a chain of prefix hashes
// that lets the router recognise a conversation it has seen before.
//
// It understands the request shapes of the common AI serving APIs (OpenAI,
// Anthropic, Ollama, Hugging Face TGI, Google Gemini/Vertex, Triton/KServe,
// Azure OpenAI) and degrades gracefully: a body it cannot read (audio,
// images, protobuf) still gets routed, just without model or prefix hints.
package prompt

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
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
	Parts   json.RawMessage `json:"parts"` // Gemini
}

// Inspect reads whatever routing hints a request carries. It never fails:
// requests it does not understand are still forwarded, routed by load alone.
func Inspect(path, contentType string, body []byte) Info {
	var info Info
	mediaType, params, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "multipart/form-data":
		info.Model = multipartField(body, params["boundary"], "model")
	case mediaType == "application/x-www-form-urlencoded":
		if q, err := url.ParseQuery(string(body)); err == nil {
			info.Model = q.Get("model")
		}
	default:
		if parsed, err := Parse(body); err == nil {
			info = parsed
		}
	}
	if info.Model == "" {
		info.Model = ModelFromPath(path)
	}
	if strings.Contains(path, "stream") { // TGI /generate_stream, Gemini :streamGenerateContent
		info.Stream = true
	}
	return info
}

// ModelFromPath finds a model named in the URL, as in Triton/KServe
// (/v2/models/{name}/infer), Gemini (/v1beta/models/{name}:generateContent)
// and Azure OpenAI (/openai/deployments/{name}/chat/completions).
func ModelFromPath(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "models" || segs[i] == "deployments" {
			name, _, _ := strings.Cut(segs[i+1], ":")
			return name
		}
	}
	return ""
}

func multipartField(body []byte, boundary, field string) string {
	if boundary == "" {
		return ""
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := r.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == field {
			v, _ := io.ReadAll(io.LimitReader(part, 512))
			return strings.TrimSpace(string(v))
		}
	}
}

// Parse reads a JSON request body. Each known field is decoded on its own,
// so an unexpected type in one field does not hide the others.
func Parse(body []byte) (Info, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return Info{}, err
	}
	get := func(key string, v any) bool {
		raw, ok := fields[key]
		return ok && json.Unmarshal(raw, v) == nil
	}

	var info Info
	get("model", &info.Model)
	get("stream", &info.Stream)

	// Completion budget, by API: OpenAI, Anthropic and most engines use
	// max_tokens (newer OpenAI: max_completion_tokens); TGI nests
	// max_new_tokens in parameters; Ollama uses options.num_predict; Gemini
	// uses generationConfig.maxOutputTokens.
	var n int
	var tgi struct {
		MaxNewTokens int `json:"max_new_tokens"`
	}
	var ollama struct {
		NumPredict int `json:"num_predict"`
	}
	var gemini struct {
		MaxOutputTokens int `json:"maxOutputTokens"`
	}
	switch {
	case get("max_completion_tokens", &n), get("max_tokens", &n):
		info.MaxTokens = n
	case get("parameters", &tgi) && tgi.MaxNewTokens > 0:
		info.MaxTokens = tgi.MaxNewTokens
	case get("options", &ollama) && ollama.NumPredict > 0:
		info.MaxTokens = ollama.NumPredict
	case get("generationConfig", &gemini) && gemini.MaxOutputTokens > 0:
		info.MaxTokens = gemini.MaxOutputTokens
	}

	// The system prompt comes first so it anchors the shared prefix:
	// Anthropic and Ollama put it in "system", Gemini in "systemInstruction".
	var sb strings.Builder
	sb.WriteString(contentText(fields["system"]))
	var sysInstr message
	if get("systemInstruction", &sysInstr) {
		sb.WriteString(contentText(sysInstr.Parts))
	}

	var msgs []message
	if !get("messages", &msgs) {
		get("contents", &msgs) // Gemini
	}
	for _, m := range msgs {
		// Record and unit separators keep "ab"+"c" distinct from "a"+"bc".
		sb.WriteByte(0x1e)
		sb.WriteString(m.Role)
		sb.WriteByte(0x1f)
		if len(m.Content) > 0 {
			sb.WriteString(contentText(m.Content))
		} else {
			sb.WriteString(contentText(m.Parts))
		}
	}
	if len(msgs) == 0 {
		// Completions (prompt), embeddings (input), TGI (inputs), and
		// Vertex/TF Serving/KServe v1 (instances).
		for _, key := range []string{"prompt", "input", "inputs", "instances"} {
			if raw := fields[key]; len(raw) > 0 {
				sb.WriteString(contentText(raw))
				break
			}
		}
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
				Type string  `json:"type"`
				Text *string `json:"text"`
			}
			// OpenAI and Anthropic tag text parts with "type"; Gemini parts are bare {"text": ...}.
			if json.Unmarshal(p, &part) == nil && part.Text != nil && (part.Type == "" || part.Type == "text") {
				sb.WriteString(*part.Text)
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
