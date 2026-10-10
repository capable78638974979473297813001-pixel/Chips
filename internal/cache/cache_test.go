package cache

import (
	"testing"
	"time"
)

func entry(n int) *Entry { return &Entry{Status: 200, Body: make([]byte, n)} }

func TestGetPut(t *testing.T) {
	c := New(1<<20, time.Minute)
	k := NewKey([]byte("POST"), []byte("/v1/embeddings"), []byte(`{"input":"x"}`))
	if _, ok := c.Get(k); ok {
		t.Fatal("hit on empty cache")
	}
	c.Put(k, entry(10))
	if e, ok := c.Get(k); !ok || len(e.Body) != 10 {
		t.Fatalf("get = %v %v", e, ok)
	}
	if NewKey([]byte("ab"), []byte("c")) == NewKey([]byte("a"), []byte("bc")) {
		t.Error("key parts are not separated")
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New(3*(1000+512), time.Minute)
	a, b, d := NewKey([]byte("a")), NewKey([]byte("b")), NewKey([]byte("d"))
	c.Put(a, entry(1000))
	c.Put(b, entry(1000))
	c.Get(a) // a is now more recent than b
	c.Put(d, entry(1000))
	c.Put(NewKey([]byte("e")), entry(1000))
	if _, ok := c.Get(b); ok {
		t.Error("b should have been evicted first")
	}
	if _, ok := c.Get(a); !ok {
		t.Error("a was used recently and should remain")
	}
	if n, bytes := c.Stats(); n != 3 || bytes > 3*(1000+512) {
		t.Errorf("stats = %d entries, %d bytes", n, bytes)
	}
	c.Put(NewKey([]byte("huge")), entry(1<<20))
	if n, _ := c.Stats(); n != 3 {
		t.Error("an entry larger than the whole cache was stored")
	}
}

func TestExpires(t *testing.T) {
	c := New(1<<20, time.Minute)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	k := NewKey([]byte("k"))
	c.Put(k, entry(1))
	now = now.Add(59 * time.Second)
	if _, ok := c.Get(k); !ok {
		t.Error("expired too early")
	}
	now = now.Add(2 * time.Second)
	if _, ok := c.Get(k); ok {
		t.Error("served an expired entry")
	}
	if n, _ := c.Stats(); n != 0 {
		t.Error("expired entry not removed")
	}
}
