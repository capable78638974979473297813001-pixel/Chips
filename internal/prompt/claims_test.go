package prompt

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestClaims(t *testing.T) {
	for _, tc := range []struct {
		name, path, query, ctype, body string
		models                         []string
		budget                         int
		set, ambiguous                 bool
	}{
		{name: "openai", path: "/v1/chat/completions", body: `{"model":"llama","max_tokens":100,"messages":[]}`,
			models: []string{"llama"}, budget: 100, set: true},
		{name: "largest budget wins", path: "/v1/chat/completions", body: `{"model":"m","max_completion_tokens":10,"max_tokens":5000}`,
			models: []string{"m"}, budget: 5000, set: true},
		{name: "no budget", path: "/v1/chat/completions", body: `{"model":"m"}`, models: []string{"m"}},
		{name: "tgi", path: "/generate", body: `{"inputs":"hi","parameters":{"max_new_tokens":64}}`, budget: 64, set: true},
		{name: "ollama unlimited", path: "/api/generate", body: `{"model":"m","options":{"num_predict":-1}}`,
			models: []string{"m"}, budget: -1, set: true},
		{name: "null budget", body: `{"model":"m","max_tokens":null}`, models: []string{"m"}, budget: -1, set: true},
		{name: "string budget", body: `{"model":"m","max_tokens":"99999"}`, models: []string{"m"}, budget: -1, set: true},
		{name: "gemini snake case", path: "/v1beta/models/gemini:generateContent", body: `{"generation_config":{"max_output_tokens":9000}}`,
			models: []string{"gemini"}, budget: 9000, set: true},
		{name: "path and body model", path: "/openai/deployments/big/chat/completions", body: `{"model":"small"}`,
			models: []string{"big", "small"}},
		{name: "query model", path: "/ws", query: "model=a", models: []string{"a"}},
		{name: "form", ctype: "application/x-www-form-urlencoded", body: "model=w&x=1", models: []string{"w"}},
		{name: "unrelated escaped key is fine", body: `{"model":"m","tools":[{"café":1}],"déjà":2}`, models: []string{"m"}},

		// Spellings that some parser would read differently from the router.
		{name: "escaped model key", body: `{"model":"small","model":"big"}`, models: []string{"small", "big"}, ambiguous: true},
		{name: "duplicate model", body: `{"model":"small","model":"big"}`, models: []string{"small", "big"}, ambiguous: true},
		{name: "case folded model", body: `{"model":"small","MODEL":"big"}`, models: []string{"small", "big"}, ambiguous: true},
		{name: "kelvin sign fold", body: `{"model":"m","max_toKens":99999}`, models: []string{"m"}, budget: 99999, set: true, ambiguous: true},
		{name: "duplicate nested budget", body: `{"options":{"num_predict":1,"num_predict":99999}}`, budget: 99999, set: true, ambiguous: true},
		{name: "model not a string", body: `{"model":["a","b"]}`, ambiguous: true},
		{name: "trailing garbage", body: `{"model":"m"} {"model":"x"}`, ambiguous: true},
		{name: "not json", ctype: "application/octet-stream", body: "\x00\x01", ambiguous: true},
		{name: "two multipart models", ctype: "multipart/form-data; boundary=X",
			body:   "--X\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\na\r\n--X\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nb\r\n--X--\r\n",
			models: []string{"a", "b"}, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ClaimsOf(tc.path, tc.query, tc.ctype, []byte(tc.body))
			if !slices.Equal(c.Models, tc.models) || c.Budget != tc.budget || c.BudgetSet != tc.set || c.Ambiguous != tc.ambiguous {
				t.Errorf("got %+v", c)
			}
		})
	}
}

func TestAddField(t *testing.T) {
	for in, want := range map[string]string{
		`{"model":"m"}`:     `{"max_tokens":5,"model":"m"}`,
		` { "a" : 1 } `:     ` {"max_tokens":5, "a" : 1 } `,
		`{}`:                `{"max_tokens":5}`,
		"{\n}":              "{\"max_tokens\":5\n}",
		`{"x":{"y":[1,2]}}`: `{"max_tokens":5,"x":{"y":[1,2]}}`,
	} {
		got, ok := AddField([]byte(in), "max_tokens", 5)
		if !ok || string(got) != want {
			t.Errorf("AddField(%q) = %q, %v; want %q", in, got, ok, want)
		}
		var v map[string]any
		if err := json.Unmarshal(got, &v); err != nil || v["max_tokens"] != float64(5) {
			t.Errorf("AddField(%q) produced invalid JSON %q: %v", in, got, err)
		}
	}
	if _, ok := AddField([]byte(`[1]`), "k", 1); ok {
		t.Error("AddField accepted an array")
	}
}

// FuzzClaims checks that whatever ClaimsOf accepts as unambiguous, a real
// JSON decoder reads the same way: every key a lenient parser would take
// for model or a budget is spelled exactly, its model is in Models, and no
// budget exceeds Budget.
func FuzzClaims(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","max_tokens":5}`, `{"model":"a","model":"b"}`, `{"MODEL":"x"}`,
		`{"options":{"num_predict":3}}`, `{"parameters":{"max_new_tokens":1e3}}`, `{"model":null}`,
		`{"generation_config":{"max_output_tokens":7}}`, `{"model":"m","max_toKens":9}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		c := ClaimsOf("/v1/chat/completions", "", "application/json", body)
		var top map[string]json.RawMessage
		if c.Ambiguous || json.Unmarshal(body, &top) != nil {
			return
		}
		checkBudget := func(raw json.RawMessage) {
			var n float64
			if json.Unmarshal(raw, &n) != nil || n < 0 || (c.Budget >= 0 && n > float64(c.Budget)) {
				if c.Budget != -1 {
					t.Fatalf("budget %s not covered by %+v in %q", raw, c, body)
				}
			}
		}
		for k, v := range top {
			switch {
			case strings.EqualFold(k, "model"):
				var m string
				if k != "model" || (string(v) != "null" && (json.Unmarshal(v, &m) != nil || !slices.Contains(c.Models, m))) {
					t.Fatalf("model key %q=%s missed: %+v in %q", k, v, c, body)
				}
			case slices.ContainsFunc(topBudget, func(n string) bool { return strings.EqualFold(k, n) }):
				if !slices.Contains(topBudget, k) {
					t.Fatalf("folded budget key %q accepted in %q", k, body)
				}
				checkBudget(v)
			case slices.ContainsFunc(budgetObjects, func(n string) bool { return strings.EqualFold(k, n) }):
				var inner map[string]json.RawMessage
				if string(v) == "null" {
					continue
				}
				if json.Unmarshal(v, &inner) != nil {
					t.Fatalf("budget object %q=%s accepted in %q", k, v, body)
				}
				for ik, iv := range inner {
					if slices.ContainsFunc(innerBudget, func(n string) bool { return strings.EqualFold(ik, n) }) {
						if !slices.Contains(innerBudget, ik) {
							t.Fatalf("folded budget key %q accepted in %q", ik, body)
						}
						checkBudget(iv)
					}
				}
			}
		}
	})
}
