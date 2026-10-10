package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func reloadVia(t *testing.T, url string, headers ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/admin/reload", nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLiveReload(t *testing.T) {
	oldBackend := newFake(t, nil)
	newBackend := newFake(t, nil)
	ts, p := setup(t, func(c *config.Config) {
		c.Auth.AdminKeys = []string{"admin"}
		c.Auth.Keys = []config.APIKey{{Name: "team", Key: "k1"}}
	}, oldBackend.URL)
	srv := ts.Config.Handler.(*Server)

	// The next config, as an operator would write it to the file.
	next := `
backends:
  - {name: b1, url: %q, max_concurrency: 4}
auth:
  admin_keys: [admin]
  keys:
    - {name: team, key: k2, requests_per_minute: 1}
models:
  aliases: {gpt-4o: llama}
listen: ":9999"
`
	var fileErr error
	srv.SetReloader(func() ([]string, error) {
		if fileErr != nil {
			return nil, fileErr
		}
		cfg, err := config.Parse([]byte(fmt.Sprintf(next, newBackend.URL)))
		if err != nil {
			return nil, err
		}
		return srv.Apply(cfg)
	})
	team := func(key string) int {
		return chat(t, ts.URL, `{"model":"gpt-4o"}`, "Authorization", "Bearer "+key).StatusCode
	}
	if code := team("k1"); code != 200 || oldBackend.hits.Load() != 1 {
		t.Fatalf("before reload: %d", code)
	}

	if code, _ := reloadVia(t, ts.URL, "Authorization", "Bearer k1"); code != 401 {
		t.Errorf("client key may reload: %d", code)
	}
	code, out := reloadVia(t, ts.URL, "Authorization", "Bearer admin")
	if code != 200 {
		t.Fatalf("reload: %d %v", code, out)
	}
	changes := fmt.Sprint(out["changes"])
	for _, want := range []string{"added backend b1", "removed backend b0", "updated API keys", "updated model aliases", "listen changed: takes effect after a restart"} {
		if !strings.Contains(changes, want) {
			t.Errorf("changes %s missing %q", changes, want)
		}
	}

	// Rotated key, new limit, new backend, new alias: all live.
	if code := team("k1"); code != 401 {
		t.Errorf("old key after rotation: %d", code)
	}
	if code := team("k2"); code != 200 || newBackend.hits.Load() != 1 || oldBackend.hits.Load() != 1 {
		t.Errorf("new key: %d; hits old=%d new=%d", code, oldBackend.hits.Load(), newBackend.hits.Load())
	}
	if code := team("k2"); code != 429 {
		t.Errorf("new rate limit not enforced: %d", code)
	}
	if st := p.Snapshot(); len(st) != 1 || st[0].Name != "b1" {
		t.Errorf("backends after reload: %+v", st)
	}
	// Usage survived the reload: 1 request before, 1 after (plus one rejected).
	if u := usageOf(t, ts.URL, "team", "Authorization", "Bearer admin"); u.Requests != 2 || u.Rejected != 1 {
		t.Errorf("usage after reload: %+v", u)
	}

	// A broken config is refused and the running one stays.
	fileErr = errors.New("parse config: yaml: line 3: mapping values are not allowed here")
	code, out = reloadVia(t, ts.URL, "Authorization", "Bearer admin")
	if code != 400 || !strings.Contains(fmt.Sprint(out), "previous config still in use") {
		t.Errorf("bad config: %d %v", code, out)
	}
	if code := chat(t, ts.URL, `{}`, "Authorization", "Bearer k2"); code.StatusCode != 429 {
		// still rate limited under the config that was kept
		t.Errorf("config changed by a failed reload: %d", code.StatusCode)
	}
}
