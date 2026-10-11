package limits

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTracker(l Limits) (*Tracker, *clock) {
	c := &clock{time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)}
	t := New()
	t.now = c.now
	t.SetLimits(map[string]Limits{"team": l})
	return t, c
}

func TestConcurrencyLimit(t *testing.T) {
	tr, _ := newTracker(Limits{MaxConcurrent: 2})
	r1, _ := tr.Admit("team")
	r2, _ := tr.Admit("team")
	if _, rej := tr.Admit("team"); rej == nil || rej.Reason != "concurrency_limit" {
		t.Fatalf("third request admitted: %+v", rej)
	}
	r1()
	r1() // releasing twice is harmless
	if r3, rej := tr.Admit("team"); rej != nil {
		t.Fatalf("slot not freed: %+v", rej)
	} else {
		r3()
	}
	r2()
	if u := tr.Snapshot()[0]; u.Requests != 3 || u.Rejected != 1 || u.InFlight != 0 {
		t.Errorf("usage = %+v", u)
	}
}

func TestRequestsPerMinute(t *testing.T) {
	tr, clk := newTracker(Limits{RequestsPerMinute: 60}) // one per second, burst 60
	for i := range 60 {
		r, rej := tr.Admit("team")
		if rej != nil {
			t.Fatalf("burst request %d rejected", i)
		}
		r()
	}
	_, rej := tr.Admit("team")
	if rej == nil || rej.Reason != "rate_limited" || rej.RetryAfter <= 0 || rej.RetryAfter > time.Second {
		t.Fatalf("61st request: %+v", rej)
	}
	clk.t = clk.t.Add(time.Second)
	if r, rej := tr.Admit("team"); rej != nil {
		t.Fatalf("not refilled after a second: %+v", rej)
	} else {
		r()
	}
}

func TestDailyTokenQuota(t *testing.T) {
	tr, clk := newTracker(Limits{TokensPerDay: 1000})
	r, _ := tr.Admit("team")
	r()
	tr.Record("team", 600, 500, false)
	_, rej := tr.Admit("team")
	if rej == nil || rej.Reason != "quota_exceeded" || rej.RetryAfter != time.Hour {
		t.Fatalf("over quota: %+v (retry after should be the hour until UTC midnight)", rej)
	}
	clk.t = clk.t.Add(time.Hour) // a new UTC day
	if r, rej := tr.Admit("team"); rej != nil {
		t.Fatalf("quota did not reset at midnight: %+v", rej)
	} else {
		r()
	}
	u := tr.Snapshot()[0]
	if u.TokensToday != 0 || u.PromptTokens != 600 || u.CompletionTokens != 500 || u.Day != "2026-10-11" {
		t.Errorf("usage = %+v", u)
	}
}

func TestUnlimitedAndReload(t *testing.T) {
	tr, _ := newTracker(Limits{MaxConcurrent: 1})
	for range 100 {
		if _, rej := tr.Admit("someone-else"); rej != nil {
			t.Fatal("client without limits was limited")
		}
	}
	r, _ := tr.Admit("team")
	tr.Record("team", 10, 5, true)
	// A reload that lifts the limit keeps the counters.
	tr.SetLimits(map[string]Limits{})
	if r2, rej := tr.Admit("team"); rej != nil {
		t.Fatal("limit still enforced after removal")
	} else {
		r2()
	}
	r()
	for _, u := range tr.Snapshot() {
		if u.Client == "team" && (u.PromptTokens != 10 || u.EstimatedTokens != 15 || u.Requests != 2) {
			t.Errorf("counters lost on reload: %+v", u)
		}
	}
}
