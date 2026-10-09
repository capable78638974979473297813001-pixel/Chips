package affinity

import (
	"testing"
	"time"
)

func TestLookupPrefersDeepestMatch(t *testing.T) {
	tb := New(100, time.Hour)
	tb.Record([]uint64{1, 2}, "a")
	tb.Record([]uint64{1, 2, 3, 4}, "b")
	if got, depth := tb.Lookup([]uint64{1, 2, 3, 4, 5}); got != "b" || depth != 4 {
		t.Errorf("got %q at depth %d, want b at 4", got, depth)
	}
	if got, _ := tb.Lookup([]uint64{1, 9}); got != "b" {
		t.Errorf("shared first block should follow latest writer, got %q", got)
	}
	if got, depth := tb.Lookup([]uint64{7}); got != "" || depth != 0 {
		t.Errorf("unexpected match %q", got)
	}
}

func TestExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	tb := New(100, time.Minute)
	tb.now = func() time.Time { return now }
	tb.Record([]uint64{1}, "a")
	now = now.Add(2 * time.Minute)
	if got, _ := tb.Lookup([]uint64{1}); got != "" {
		t.Errorf("expired entry returned %q", got)
	}
	if tb.Len() != 0 {
		t.Error("expired entry not removed")
	}
}

func TestCapacityEvictsLeastRecent(t *testing.T) {
	tb := New(2, time.Hour)
	tb.Record([]uint64{1}, "a")
	tb.Record([]uint64{2}, "b")
	tb.Lookup([]uint64{1}) // lookups do not refresh; records do
	tb.Record([]uint64{1}, "a")
	tb.Record([]uint64{3}, "c")
	if got, _ := tb.Lookup([]uint64{2}); got != "" {
		t.Error("least recently recorded key should have been evicted")
	}
	if got, _ := tb.Lookup([]uint64{1}); got != "a" {
		t.Error("recently refreshed key was evicted")
	}
	if tb.Len() != 2 {
		t.Errorf("len = %d", tb.Len())
	}
}
