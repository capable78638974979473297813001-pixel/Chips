package prompt

import (
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
	for _, want := range []string{"be brief", "what is this?", "data:x", "system", "user"} {
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
	if err != nil || info.Text != "ab" {
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
