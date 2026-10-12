package affinity

import (
	"slices"
	"testing"
	"time"
)

func first(bs []string) string {
	if len(bs) == 0 {
		return ""
	}
	return bs[0]
}

func TestLookupPrefersDeepestMatch(t *testing.T) {
	tb := New(100, time.Hour)
	tb.Record([]uint64{1, 2}, "a")
	tb.Record([]uint64{1, 2, 3, 4}, "b")
	if got, depth := tb.Lookup([]uint64{1, 2, 3, 4, 5}); !slices.Equal(got, []string{"b"}) || depth != 4 {
		t.Errorf("got %q at depth %d, want b at 4", got, depth)
	}
	if got, _ := tb.Lookup([]uint64{1, 9}); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("shared first block: got %q, want both holders, latest writer first", got)
	}
	if got, depth := tb.Lookup([]uint64{7}); got != nil || depth != 0 {
		t.Errorf("unexpected match %q", got)
	}
}

func TestExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	tb := New(100, time.Minute)
	tb.now = func() time.Time { return now }
	tb.Record([]uint64{1}, "a")
	now = now.Add(2 * time.Minute)
	if got, _ := tb.Lookup([]uint64{1}); got != nil {
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
	if got, _ := tb.Lookup([]uint64{2}); got != nil {
		t.Error("least recently recorded key should have been evicted")
	}
	if got, _ := tb.Lookup([]uint64{1}); first(got) != "a" {
		t.Error("recently refreshed key was evicted")
	}
	if tb.Len() != 2 {
		t.Errorf("len = %d", tb.Len())
	}
}

// A prefix remembers up to MaxHolders backends, most recent first; each
// holder expires on its own.
func TestHolders(t *testing.T) {
	now := time.Unix(0, 0)
	tb := New(100, time.Minute)
	tb.now = func() time.Time { return now }
	for _, b := range []string{"a", "b", "c", "d", "e", "c"} {
		tb.Record([]uint64{1}, b)
		now = now.Add(time.Second)
	}
	if got, _ := tb.Lookup([]uint64{1}); !slices.Equal(got, []string{"c", "e", "d", "b"}) {
		t.Errorf("holders = %q, want c e d b", got)
	}
	now = now.Add(time.Minute - 2*time.Second) // t=64s: b (1s) and d (3s) expire; e (4s) and c (5s) stay
	if got, _ := tb.Lookup([]uint64{1}); !slices.Equal(got, []string{"c", "e"}) {
		t.Errorf("holders = %q, want c e", got)
	}
}
