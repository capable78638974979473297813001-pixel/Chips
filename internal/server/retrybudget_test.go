package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

func TestRetryBudget(t *testing.T) {
	clock := time.Unix(1000, 0)
	b := newRetryBudget(0.2, 0.5) // floor: 5 retries per 10s window
	b.now = func() time.Time { return clock }

	spent := 0
	for range 100 {
		b.request()
	}
	for b.spend() {
		spent++
	}
	if spent != 25 { // 0.2 × 100 + 0.5 × 10
		t.Fatalf("spent %d retries, want 25", spent)
	}
	if b.allows() {
		t.Error("budget allows more after it was spent")
	}

	// The window slides: ten seconds later the old traffic no longer counts,
	// and only the floor is left.
	clock = clock.Add(10 * time.Second)
	spent = 0
	for b.spend() {
		spent++
	}
	if spent != 5 {
		t.Errorf("floor allowed %d retries, want 5", spent)
	}

	b.set(-1, 0) // no budget
	for range 1000 {
		if !b.spend() {
			t.Fatal("unlimited budget refused a retry")
		}
	}
}

func TestRetryBudgetStopsRetries(t *testing.T) {
	failing := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}
	for _, tc := range []struct {
		name         string
		ratio, floor float64
		wantStatus   int
		wantHits     int64
	}{
		{"budget available: retried on the other backend", 0.2, 10, http.StatusBadGateway, 2}, // every backend failed
		{"budget spent: the backend's 503 goes to the client", 0.0001, 0.0001, http.StatusServiceUnavailable, 1},
	} {
		a, b := newFake(t, failing), newFake(t, failing)
		ts, _ := setup(t, func(c *config.Config) {
			c.Retry.BudgetRatio, c.Retry.BudgetMinPerSecond = tc.ratio, tc.floor
		}, a.URL, b.URL)
		resp := chat(t, ts.URL, `{"model":"llama"}`)
		if hits := a.hits.Load() + b.hits.Load(); resp.StatusCode != tc.wantStatus || hits != tc.wantHits {
			t.Errorf("%s: status %d after %d upstream attempts, want %d after %d", tc.name, resp.StatusCode, hits, tc.wantStatus, tc.wantHits)
		}
	}
}
