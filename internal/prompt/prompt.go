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
	"hash/fnv"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Info describes one inference request.
type Info struct {
	Model  string
	Stream bool
	// MaxTokens is the requested completion budget, 0 if unspecified.
	MaxTokens int
	// Text is a canonical rendering of the prompt used for prefix matching:
	// message contents as they appear in the body (JSON-escaped), so two
	// requests whose prompts start the same way have Texts that start the same way.
	Text string
	// Deterministic is true when the request asks for greedy decoding
	// (temperature 0), so an identical request gets an identical answer.
	Deterministic bool
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

// Field returns the raw JSON value of a top-level field of a JSON object
// body, or nil.
func Field(body []byte, key string) []byte { return field(body, key) }

// ReplacePathModel swaps the model named in a URL (see ModelFromPath).
func ReplacePathModel(path, model string) string {
	segs := strings.Split(path, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "models" || segs[i] == "deployments" {
			_, action, hasAction := strings.Cut(segs[i+1], ":")
			segs[i+1] = model
			if hasAction {
				segs[i+1] += ":" + action
			}
			return strings.Join(segs, "/")
		}
	}
	return path
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

// Parse reads a JSON request body in one pass without decoding it: it picks
// out the few fields routing needs and slices the prompt text straight out of
// the body bytes.
func Parse(body []byte) (Info, error) {
	var info Info
	var maxCompletion, maxTokens, params, options, genCfg, temperature []byte
	var system, systemInstruction, messages, contents []byte
	var inputs [4][]byte // prompt, input, inputs, instances: first present wins, in this order
	err := eachField(body, func(k, v []byte) bool {
		switch string(k) {
		case "model":
			info.Model, _ = stringValue(v)
		case "stream":
			info.Stream = string(v) == "true"
		case "max_completion_tokens":
			maxCompletion = v
		case "max_tokens":
			maxTokens = v
		case "parameters": // TGI
			params = v
		case "options": // Ollama
			options = v
		case "generationConfig": // Gemini
			genCfg = v
		case "temperature":
			temperature = v
		case "system": // Anthropic, Ollama
			system = v
		case "systemInstruction": // Gemini
			systemInstruction = v
		case "messages":
			messages = v
		case "contents": // Gemini
			contents = v
		case "prompt":
			inputs[0] = v
		case "input":
			inputs[1] = v
		case "inputs": // TGI
			inputs[2] = v
		case "instances": // Vertex, TF Serving, KServe v1
			inputs[3] = v
		}
		return true
	})
	if err != nil {
		return Info{}, err
	}

	// Completion budget, by API: OpenAI, Anthropic and most engines use
	// max_tokens (newer OpenAI: max_completion_tokens); TGI nests
	// max_new_tokens in parameters; Ollama uses options.num_predict; Gemini
	// uses generationConfig.maxOutputTokens.
	for _, v := range [][]byte{maxCompletion, maxTokens, field(params, "max_new_tokens"),
		field(options, "num_predict"), field(genCfg, "maxOutputTokens")} {
		if n, ok := numberValue(v); ok && n >= 0 && n == float64(int(n)) {
			info.MaxTokens = int(n)
			break
		}
	}
	// Sampling temperature, wherever the API puts it. Zero means greedy
	// decoding: the same request yields the same answer.
	for _, v := range [][]byte{temperature, field(options, "temperature"), field(params, "temperature"),
		field(genCfg, "temperature")} {
		if t, ok := numberValue(v); ok {
			info.Deterministic = t == 0
			break
		}
	}

	var sb strings.Builder
	sb.Grow(len(body))
	// The system prompt comes first so it anchors the shared prefix.
	sb.Write(textOf(system))
	sb.Write(textOf(field(systemInstruction, "parts")))
	if messages == nil {
		messages = contents
	}
	n := 0
	eachElement(messages, func(m []byte) {
		n++
		var role, content, parts []byte
		eachField(m, func(k, v []byte) bool {
			switch string(k) {
			case "role":
				role = v
			case "content":
				content = v
			case "parts": // Gemini
				parts = v
			}
			return true
		})
		// Record and unit separators keep "ab"+"c" distinct from "a"+"bc".
		sb.WriteByte(0x1e)
		sb.Write(textOf(role))
		sb.WriteByte(0x1f)
		if content != nil {
			sb.Write(textOf(content))
		} else {
			sb.Write(textOf(parts))
		}
	})
	if n == 0 {
		for _, v := range inputs {
			if v != nil {
				sb.Write(textOf(v))
				break
			}
		}
	}
	info.Text = sb.String()
	return info, nil
}

// EstimateTokens approximates a token count without a tokenizer. ASCII
// text (English, code) runs at about charsPerToken characters per token;
// Chinese, Japanese and Korean characters are each about one token in
// current tokenizers; other scripts (Cyrillic, Greek, Arabic, ...) about
// two and a half characters per token. Routing only needs the right order
// of magnitude; see Server's exact counting for prompts near a limit.
func EstimateTokens(text string, charsPerToken float64) int {
	if text == "" {
		return 0
	}
	var ascii, cjk, other int
	for i := 0; i < len(text); {
		c := text[i]
		if c < utf8.RuneSelf {
			ascii++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			cjk++
		} else {
			other++
		}
	}
	return int(float64(ascii)/charsPerToken+float64(cjk)+float64(other)/2.5) + 1
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
