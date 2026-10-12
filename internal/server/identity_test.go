package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/audit"
	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/limits"
	"github.com/capable78638974979473297813001-pixel/chips/internal/oidc/oidctest"
)

func oidcSetup(t *testing.T, is *oidctest.Issuer, mutate func(*config.Config)) (string, *Server) {
	t.Helper()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	var srv *Server
	ts, _ := setupServer(t, func(c *config.Config) {
		c.Auth.OIDC = config.OIDC{Issuer: is.URL, Audiences: []string{"siliconrouter"}, UserClaim: "email",
			GroupsClaim: "groups", DefaultTeam: "oidc", Leeway: config.Duration(time.Minute), JWKSRefresh: config.Duration(time.Hour),
			AdminGroups: []string{"platform"},
			Teams:       []config.Team{{Group: "ml", Name: "ml-team", Priority: "high", Models: []string{"llama"}}}}
		c.Auth.Keys = []config.APIKey{{Name: "batch", Key: "batch-key"}}
		if mutate != nil {
			mutate(c)
		}
	}, &srv, f.URL)
	return ts, srv
}

func getAs(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSingleSignOn(t *testing.T) {
	is := oidctest.New(t)
	ts, srv := oidcSetup(t, is, nil)
	ada := is.Token(t, "siliconrouter", map[string]any{"email": "ada@example.com", "groups": []string{"ml"}})
	body := `{"model":"llama","messages":[{"role":"user","content":"hi"}]}`

	if resp := chat(t, ts, body, "Authorization", "Bearer "+ada); resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid token: %d %s", resp.StatusCode, b)
	}
	// API keys still work next to tokens.
	if resp := chat(t, ts, body, "Authorization", "Bearer batch-key"); resp.StatusCode != 200 {
		t.Fatalf("API key: %d", resp.StatusCode)
	}
	// The team's model policy applies.
	if resp := chat(t, ts, `{"model":"other","messages":[]}`, "Authorization", "Bearer "+ada); resp.StatusCode != http.StatusForbidden {
		t.Errorf("model outside the team's list: %d", resp.StatusCode)
	}
	for name, tc := range map[string]struct {
		token string
		code  string
	}{
		"expired":        {is.Token(t, "siliconrouter", map[string]any{"email": "a@x", "exp": time.Now().Add(-time.Hour).Unix()}), "token_expired"},
		"other audience": {is.Token(t, "another-app", map[string]any{"email": "a@x"}), "invalid_token"},
		"no user claim":  {is.Token(t, "siliconrouter", map[string]any{"sub": "x"}), "invalid_token"},
		"forged":         {ada[:len(ada)-8] + "AAAAAAAA", "invalid_token"},
	} {
		resp := chat(t, ts, body, "Authorization", "Bearer "+tc.token)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), tc.code) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}

	// Usage is counted for the team and for the user.
	usage := map[string]limits.Usage{}
	for _, u := range srv.limits.Snapshot() {
		usage[u.Client] = u
	}
	if usage["ml-team"].Requests != 2 || usage["user:ada@example.com"].Requests != 2 || usage["user:ada@example.com"].PromptTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}

	// Admin endpoints: admin group yes, other users no.
	admin := is.Token(t, "siliconrouter", map[string]any{"email": "ops@example.com", "groups": []string{"platform"}})
	if resp := getAs(t, ts+"/admin/usage", admin); resp.StatusCode != 200 {
		t.Errorf("admin group: %d", resp.StatusCode)
	}
	if resp := getAs(t, ts+"/admin/usage", ada); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("non-admin user: %d", resp.StatusCode)
	}
	if resp := getAs(t, ts+"/admin/usage", "batch-key"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("client key on admin: %d", resp.StatusCode)
	}
}

func TestRequireTeamAndUserLimits(t *testing.T) {
	is := oidctest.New(t)
	ts, _ := oidcSetup(t, is, func(c *config.Config) {
		c.Auth.OIDC.RequireTeam = true
		c.Auth.OIDC.UserLimits = config.UserLimits{RequestsPerMinute: 1}
	})
	body := `{"model":"llama","messages":[]}`
	outsider := is.Token(t, "siliconrouter", map[string]any{"email": "eve@example.com", "groups": []string{"sales"}})
	if resp := chat(t, ts, body, "Authorization", "Bearer "+outsider); resp.StatusCode != http.StatusForbidden {
		t.Errorf("user in no team: %d, want 403", resp.StatusCode)
	}
	ada := is.Token(t, "siliconrouter", map[string]any{"email": "ada@example.com", "groups": []string{"ml"}})
	bob := is.Token(t, "siliconrouter", map[string]any{"email": "bob@example.com", "groups": "ml"})
	if resp := chat(t, ts, body, "Authorization", "Bearer "+ada); resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp := chat(t, ts, body, "Authorization", "Bearer "+ada)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), "your account") {
		t.Errorf("second request from the same user: %d %s", resp.StatusCode, b)
	}
	// Per-user: a teammate is not held back by ada's limit.
	if resp := chat(t, ts, body, "Authorization", "Bearer "+bob); resp.StatusCode != 200 {
		t.Errorf("teammate: %d", resp.StatusCode)
	}
}

func TestAuditLog(t *testing.T) {
	is := oidctest.New(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	al, err := audit.Open(audit.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ts, srv := oidcSetup(t, is, func(c *config.Config) {
		c.Backends[0].CostPerMillionPromptTokens = 1e6
	})
	srv.SetAudit(al)
	ada := is.Token(t, "siliconrouter", map[string]any{"email": "ada@example.com", "groups": []string{"ml", "platform"}})
	chat(t, ts, `{"model":"llama","messages":[{"role":"user","content":"secret prompt"}]}`, "Authorization", "Bearer "+ada)
	chat(t, ts, `{"model":"llama"}`, "Authorization", "Bearer wrong")
	req, _ := http.NewRequest("POST", ts+"/admin/backends/b0/drain", nil)
	req.Header.Set("Authorization", "Bearer "+ada)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	al.Close()

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "secret prompt") {
		t.Fatal("prompt content in the audit log")
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e map[string]any
		json.Unmarshal([]byte(line), &e)
		events = append(events, e)
	}
	if len(events) != 3 {
		t.Fatalf("%d events:\n%s", len(events), raw)
	}
	r, fail, adm := events[0], events[1], events[2]
	if r["event"] != "request" || r["client"] != "ml-team" || r["actor"] != "user:ada@example.com" || r["model"] != "llama" ||
		r["status"] != 200.0 || r["prompt_tokens"] != 3.0 || r["cost_usd"] != 3.0 {
		t.Errorf("request entry %v", r)
	}
	if fail["event"] != "auth_failure" || fail["reason"] != "invalid_api_key" {
		t.Errorf("failure entry %v", fail)
	}
	if adm["event"] != "admin" || adm["path"] != "/admin/backends/b0/drain" || adm["actor"] != "user:ada@example.com" || adm["status"] != 200.0 {
		t.Errorf("admin entry %v", adm)
	}
	if n, err := audit.Verify(path); err != nil || n != 3 {
		t.Errorf("verify: %d %v", n, err)
	}
}

func TestCacheIsolatesUsers(t *testing.T) {
	is := oidctest.New(t)
	ts, _ := oidcSetup(t, is, func(c *config.Config) { c.Cache.Enabled = true })
	body := `{"model":"llama","temperature":0,"messages":[{"role":"user","content":"same question"}]}`
	ada := is.Token(t, "siliconrouter", map[string]any{"email": "ada@example.com", "groups": []string{"ml"}})
	bob := is.Token(t, "siliconrouter", map[string]any{"email": "bob@example.com", "groups": []string{"ml"}})
	chat(t, ts, body, "Authorization", "Bearer "+ada)
	if got := chat(t, ts, body, "Authorization", "Bearer "+ada).Header.Get("X-SiliconRouter-Cache"); got != "hit" {
		t.Errorf("same user again: cache %q, want hit", got)
	}
	if got := chat(t, ts, body, "Authorization", "Bearer "+bob).Header.Get("X-SiliconRouter-Cache"); got == "hit" {
		t.Error("another user got the first user's cached answer")
	}
}
