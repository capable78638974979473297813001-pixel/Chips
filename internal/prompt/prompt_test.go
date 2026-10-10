package prompt

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseChat(t *testing.T) {
	info, err := Parse([]byte(`{
		"model": "llama", "stream": true, "max_tokens": 10, "max_completion_tokens": 20,
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": [{"type": "text", "text": "what is "}, {"type": "text", "text": "this?"},
				{"type": "image_url", "image_url": {"url": "data:x"}}]}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "llama" || !info.Stream || info.MaxTokens != 20 {
		t.Errorf("info = %+v", info)
	}
	for _, want := range []string{"be brief", "what is ", "this?", "data:x", "system", "user"} {
		if !strings.Contains(info.Text, want) {
			t.Errorf("text %q missing %q", info.Text, want)
		}
	}
}

func TestParseCompletionAndEmbeddings(t *testing.T) {
	info, err := Parse([]byte(`{"model": "m", "prompt": "hello", "max_tokens": 5}`))
	if err != nil || info.Text != "hello" || info.MaxTokens != 5 {
		t.Errorf("completion: %+v, %v", info, err)
	}
	info, err = Parse([]byte(`{"model": "m", "input": ["a", "b"]}`))
	if err != nil || info.Text != `["a", "b"]` {
		t.Errorf("embeddings: %+v, %v", info, err)
	}
}

func TestParseRejectsNonJSON(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("expected error")
	}
}

func TestConversationTurnsShareRolePrefix(t *testing.T) {
	turn1, _ := Parse([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	turn2, _ := Parse([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`))
	if !strings.HasPrefix(turn2.Text, turn1.Text) {
		t.Errorf("turn 2 %q does not extend turn 1 %q", turn2.Text, turn1.Text)
	}
}

func TestBlockHashesMatchOnSharedPrefix(t *testing.T) {
	base := strings.Repeat("a", 100)
	a := BlockHashes(base+"XXXXXXXXXX", 32, 100)
	b := BlockHashes(base+"YYYYYYYYYY", 32, 100)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("got %d and %d blocks, want 3", len(a), len(b))
	}
	for i := range 3 {
		if a[i] != b[i] {
			t.Errorf("block %d differs although the first 96 bytes are equal", i)
		}
	}
	c := BlockHashes("b"+base[1:], 32, 100)
	if c[2] == a[2] {
		t.Error("hash chain ignored a difference in the first block")
	}
	if got := BlockHashes(base, 32, 2); len(got) != 2 {
		t.Errorf("maxBlocks not applied: %d", len(got))
	}
	if BlockHashes("short", 32, 10) != nil {
		t.Error("partial block should produce no hashes")
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("", 4) != 0 || EstimateTokens(strings.Repeat("x", 400), 4) != 101 {
		t.Error("unexpected estimate")
	}
}

func TestInspectAPIStyles(t *testing.T) {
	cases := []struct {
		name, path, ctype, body string
		model                   string
		maxTokens               int
		text                    []string
	}{
		{"anthropic messages", "/v1/messages", "application/json",
			`{"model":"claude-like","max_tokens":64,"system":[{"type":"text","text":"be terse"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			"claude-like", 64, []string{"be terse", "hello"}},
		{"ollama chat", "/api/chat", "application/json",
			`{"model":"llama3","options":{"num_predict":32},"messages":[{"role":"user","content":"hi"}]}`,
			"llama3", 32, []string{"hi"}},
		{"ollama generate", "/api/generate", "application/json",
			`{"model":"llama3","system":"sys","prompt":"why"}`, "llama3", 0, []string{"sys", "why"}},
		{"tgi", "/generate_stream", "application/json",
			`{"inputs":"once upon","parameters":{"max_new_tokens":20}}`, "", 20, []string{"once upon"}},
		{"gemini", "/v1beta/models/gemma-2:generateContent", "application/json",
			`{"systemInstruction":{"parts":[{"text":"rules"}]},"contents":[{"role":"user","parts":[{"text":"q"}]}],"generationConfig":{"maxOutputTokens":9}}`,
			"gemma-2", 9, []string{"rules", "q"}},
		{"triton", "/v2/models/resnet50/infer", "application/json",
			`{"inputs":[{"name":"x","shape":[1],"datatype":"FP32","data":[1]}]}`, "resnet50", 0, nil},
		{"kserve v1", "/v1/models/sklearn-iris:predict", "application/json",
			`{"instances":[[6.8,2.8,4.8,1.4]]}`, "sklearn-iris", 0, []string{"6.8"}},
		{"azure openai", "/openai/deployments/gpt-prod/chat/completions", "application/json",
			`{"messages":[{"role":"user","content":"x"}]}`, "gpt-prod", 0, []string{"x"}},
		{"body model wins over path", "/v2/models/a/generate", "application/json",
			`{"model":"b"}`, "b", 0, nil},
		{"form", "/v1/thing", "application/x-www-form-urlencoded", "model=whisper&x=1", "whisper", 0, nil},
		{"binary", "/v2/models/yolo/infer", "application/octet-stream", "\x00\x01\x02", "yolo", 0, nil},
		{"wrong field type", "/v1/chat/completions", "application/json",
			`{"model":"m","max_tokens":"lots","messages":"not a list"}`, "m", 0, nil},
	}
	for _, c := range cases {
		info := Inspect(c.path, c.ctype, []byte(c.body))
		if info.Model != c.model || info.MaxTokens != c.maxTokens {
			t.Errorf("%s: model=%q max=%d, want %q %d", c.name, info.Model, info.MaxTokens, c.model, c.maxTokens)
		}
		for _, want := range c.text {
			if !strings.Contains(info.Text, want) {
				t.Errorf("%s: text %q missing %q", c.name, info.Text, want)
			}
		}
	}
	if !Inspect("/generate_stream", "application/json", []byte(`{}`)).Stream {
		t.Error("TGI streaming path not detected")
	}
}

func TestInspectMultipartAudio(t *testing.T) {
	body := "--XYZ\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\nContent-Type: audio/wav\r\n\r\nRIFF....\r\n" +
		"--XYZ\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nwhisper-large-v3\r\n--XYZ--\r\n"
	if got := Inspect("/v1/audio/transcriptions", "multipart/form-data; boundary=XYZ", []byte(body)).Model; got != "whisper-large-v3" {
		t.Errorf("model = %q", got)
	}
}

func TestModelFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/models":                       "",
		"/v1/chat/completions":             "",
		"/v2/models/bert/versions/1/infer": "bert",
		"/v1/models/m:predict":             "m",
	} {
		if got := ModelFromPath(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

func TestDeterministic(t *testing.T) {
	for body, want := range map[string]bool{
		`{"temperature":0}`:                             true,
		`{"temperature":0.0}`:                           true,
		`{"temperature":0.7}`:                           false,
		`{}`:                                            false, // API defaults sample
		`{"options":{"temperature":0}}`:                 true,  // Ollama
		`{"parameters":{"temperature":0}}`:              true,  // TGI
		`{"generationConfig":{"temperature":0}}`:        true,  // Gemini
		`{"temperature":1,"options":{"temperature":0}}`: false, // top level wins
	} {
		info, err := Parse([]byte(body))
		if err != nil || info.Deterministic != want {
			t.Errorf("%s: deterministic=%v err=%v, want %v", body, info.Deterministic, err, want)
		}
	}
}

func TestScannerEdgeCases(t *testing.T) {
	ok := []string{
		`{}`, ` { } `, `{"model":"a\"b"}`, `{"messages":[]}`, `{"x":{"y":[1,{"z":"]}"}]},"model":"m"}`,
		`{"model":"m","messages":[{"role":"user","content":"a, \"b\" ]}"}]}`, `{"model":"\u00e9t\u00e9"}`,
	}
	for _, b := range ok {
		if _, err := Parse([]byte(b)); err != nil {
			t.Errorf("%s: %v", b, err)
		}
	}
	if info, _ := Parse([]byte(`{"model":"a\"b"}`)); info.Model != `a"b` {
		t.Errorf("escaped model = %q", info.Model)
	}
	if info, _ := Parse([]byte(`{"model":"\u00e9t\u00e9"}`)); info.Model != "été" {
		t.Errorf("unicode model = %q", info.Model)
	}
	bad := []string{``, `[]`, `"x"`, `{`, `{"a"}`, `{"a":}`, `{"a":"b"`, `{"a":"b" "c":1}`, `{"a":[1,2}`, `{"a":"unterminated}`}
	for _, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("%q: expected an error", b)
		}
	}
}

// The scanner must never panic, and on any valid JSON object it must find
// the same model and stream flag as encoding/json.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`{"model":"llama","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"a\u0062c","x":[{"y":"}"}],"temperature":0}`,
		`{"inputs":"x","parameters":{"max_new_tokens":5}}`,
		`{"a":`, `[1,2]`, `{"model":1}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		info, err := Parse(body)
		// A map, not a struct: struct decoding matches keys case-insensitively,
		// but API field names are exact.
		var ref map[string]any
		if json.Unmarshal(body, &ref) != nil || !json.Valid(body) {
			return
		}
		if body := bytes.TrimSpace(body); len(body) == 0 || body[0] != '{' {
			return
		}
		if err != nil {
			t.Fatalf("valid object rejected: %v", err)
		}
		if m, ok := ref["model"].(string); ok && m != info.Model {
			t.Fatalf("model %q, encoding/json says %q", info.Model, m)
		}
		if st, ok := ref["stream"].(bool); ok && st != info.Stream {
			t.Fatalf("stream %v, encoding/json says %v", info.Stream, st)
		}
	})
}

func TestReplaceModel(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"\"model\":\"x\""}], "model" : "gpt-4o", "stream":true}`)
	out, ok := ReplaceModel(body, "llama-3.1-70b")
	if !ok || string(out) != `{"messages":[{"role":"user","content":"\"model\":\"x\""}], "model" : "llama-3.1-70b", "stream":true}` {
		t.Errorf("got %s", out)
	}
	if _, ok := ReplaceModel([]byte(`{"prompt":"x"}`), "m"); ok {
		t.Error("replaced a missing field")
	}
	if out, _ := ReplaceModel([]byte(`{"model":"a"}`), `we"ird`); string(out) != `{"model":"we\"ird"}` {
		t.Errorf("not escaped: %s", out)
	}
}

func TestReplacePathModel(t *testing.T) {
	for in, want := range map[string]string{
		"/v2/models/old/infer":                     "/v2/models/new/infer",
		"/v1beta/models/old:generateContent":       "/v1beta/models/new:generateContent",
		"/openai/deployments/old/chat/completions": "/openai/deployments/new/chat/completions",
		"/v1/chat/completions":                     "/v1/chat/completions",
	} {
		if got := ReplacePathModel(in, "new"); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
