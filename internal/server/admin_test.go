package server

import (
	"net/http"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func get(t *testing.T, url string, headers ...string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAdminKeys(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.APIKeys = []string{"user-key"}
		c.Auth.AdminKeys = []string{"admin-key"}
	}, f.URL)
	bearer := func(k string) []string { return []string{"Authorization", "Bearer " + k} }

	if code := get(t, ts.URL+"/admin/backends", bearer("user-key")...); code != 401 {
		t.Errorf("user key on /admin: %d, want 401", code)
	}
	if code := get(t, ts.URL+"/admin/backends", bearer("admin-key")...); code != 200 {
		t.Errorf("admin key on /admin: %d, want 200", code)
	}
	if code := get(t, ts.URL+"/admin/backends"); code != 401 {
		t.Errorf("no key on /admin: %d, want 401", code)
	}
	// Admin keys are for administration only, not for inference.
	if resp := chat(t, ts.URL, `{}`, bearer("admin-key")...); resp.StatusCode != 401 {
		t.Errorf("admin key on inference: %d, want 401", resp.StatusCode)
	}
	if resp := chat(t, ts.URL, `{}`, bearer("user-key")...); resp.StatusCode != 200 {
		t.Errorf("user key on inference: %d, want 200", resp.StatusCode)
	}
}

func TestAdminWithoutAdminKeysFallsBackToClientKeys(t *testing.T) {
	f := newFake(t, nil)
	ts, _ := setup(t, func(c *config.Config) { c.Auth.APIKeys = []string{"user-key"} }, f.URL)
	if code := get(t, ts.URL+"/admin/backends", "Authorization", "Bearer user-key"); code != 200 {
		t.Errorf("client key on /admin without admin keys: %d", code)
	}
	if code := get(t, ts.URL+"/admin/backends"); code != 401 {
		t.Errorf("no key: %d", code)
	}
}
