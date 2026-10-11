package cache

import (
	"bytes"
	"context"
	"net/http"
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

func TestLookupCoalesces(t *testing.T) {
	c := New(1<<20, time.Minute)
	k := NewKey([]byte("same request"))
	_, _, leader, done := c.Lookup(context.Background(), k)
	if !leader {
		t.Fatal("first request is not the leader")
	}
	results := make(chan bool, 5)
	for range 5 {
		go func() {
			e, coalesced, leader, d := c.Lookup(context.Background(), k)
			d()
			results <- e != nil && coalesced && !leader
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the followers start waiting
	c.Put(k, entry(3))
	done()
	for range 5 {
		if !<-results {
			t.Error("a follower did not get the leader's answer")
		}
	}
}

func TestLookupAfterUncacheableAnswer(t *testing.T) {
	c := New(1<<20, time.Minute)
	k := NewKey([]byte("k"))
	_, _, _, done := c.Lookup(context.Background(), k)
	got := make(chan bool)
	go func() {
		e, _, leader, d := c.Lookup(context.Background(), k)
		d()
		got <- e == nil && leader
	}()
	time.Sleep(10 * time.Millisecond)
	done() // the leader's answer was an error: nothing stored
	if !<-got {
		t.Error("waiter did not fall back to computing its own answer")
	}
	// A waiter gives up with its context.
	_, _, _, done2 := c.Lookup(context.Background(), k)
	defer done2()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if e, _, leader, _ := c.Lookup(ctx, k); e != nil || leader {
		t.Error("cancelled waiter should get nothing")
	}
}

func TestSaveLoad(t *testing.T) {
	c := New(1<<20, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	fresh, stale := NewKey([]byte("fresh")), NewKey([]byte("stale"))
	c.Put(stale, &Entry{Status: 200, Body: []byte("old")})
	now = now.Add(50 * time.Second)
	c.Put(fresh, &Entry{Status: 200, Body: []byte("new"), Header: http.Header{"Content-Type": {"application/json"}}, Backend: "gpu1"})
	var buf bytes.Buffer
	if err := c.Save(&buf); err != nil {
		t.Fatal(err)
	}
	d := New(1<<20, time.Minute)
	d.now = func() time.Time { return now.Add(20 * time.Second) } // "stale" is now 70s old
	if n, err := d.Load(&buf); err != nil || n != 1 {
		t.Fatalf("restored %d, %v; want 1", n, err)
	}
	e, ok := d.Get(fresh)
	if !ok || string(e.Body) != "new" || e.Backend != "gpu1" || e.Header.Get("Content-Type") != "application/json" {
		t.Errorf("restored entry = %+v", e)
	}
	if _, ok := d.Get(stale); ok {
		t.Error("expired entry restored")
	}
}
