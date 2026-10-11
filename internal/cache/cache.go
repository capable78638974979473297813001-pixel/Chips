// Package cache keeps recent inference responses in memory so an identical
// request can be answered without touching a GPU.
package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"io"
	"net/http"
	"sync"
	"time"
)

// Key identifies a request. It is a hash, so the cache never holds prompts.
type Key [sha256.Size]byte

// NewKey hashes the parts of a request that determine its answer.
func NewKey(parts ...[]byte) Key {
	h := sha256.New()
	var sep [1]byte
	for _, p := range parts {
		h.Write(p)
		h.Write(sep[:]) // so ("ab","c") and ("a","bc") differ
	}
	var k Key
	h.Sum(k[:0])
	return k
}

// Entry is a complete stored response.
type Entry struct {
	Status  int
	Header  http.Header
	Body    []byte
	Backend string // which backend produced it
	stored  time.Time
}

func (e *Entry) size() int64 { return int64(len(e.Body)) + 512 } // rough header overhead

type item struct {
	key   Key
	entry *Entry
}

// Cache is an LRU bounded by total bytes, with a time-to-live per entry.
type Cache struct {
	mu       sync.Mutex
	ll       *list.List
	items    map[Key]*list.Element
	flights  map[Key]chan struct{} // requests currently being computed
	bytes    int64
	maxBytes int64
	ttl      time.Duration
	now      func() time.Time
}

func New(maxBytes int64, ttl time.Duration) *Cache {
	return &Cache{ll: list.New(), items: map[Key]*list.Element{}, flights: map[Key]chan struct{}{},
		maxBytes: maxBytes, ttl: ttl, now: time.Now}
}

// Lookup returns a cached entry for k. On a miss it either makes the caller
// the leader for k (leader=true: compute the answer, Put it if it can be
// cached, then call done) or, if another request is already computing k,
// waits for that request and returns its answer (coalesced=true). If the
// leader's answer could not be cached, a waiter gets no entry and computes
// its own (leader=true). Identical requests that arrive together therefore
// cost one backend call.
func (c *Cache) Lookup(ctx context.Context, k Key) (e *Entry, coalesced, leader bool, done func()) {
	if e, ok := c.Get(k); ok {
		return e, false, false, func() {}
	}
	c.mu.Lock()
	if ch, busy := c.flights[k]; busy {
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, false, false, func() {}
		}
		if e, ok := c.Get(k); ok {
			return e, true, false, func() {}
		}
		return nil, false, true, func() {} // the leader's answer was not cacheable
	}
	ch := make(chan struct{})
	c.flights[k] = ch
	c.mu.Unlock()
	return nil, false, true, func() {
		c.mu.Lock()
		delete(c.flights, k)
		c.mu.Unlock()
		close(ch)
	}
}

// Get returns a fresh entry for k. Entries are read-only once stored.
func (c *Cache) Get(k Key) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	it := el.Value.(*item)
	if c.now().Sub(it.entry.stored) > c.ttl {
		c.removeLocked(el)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return it.entry, true
}

// Put stores e under k, evicting the least recently used entries to stay
// within the byte budget. Entries larger than the whole budget are dropped.
func (c *Cache) Put(k Key, e *Entry) { c.put(k, e, c.now()) }

func (c *Cache) put(k Key, e *Entry, stored time.Time) {
	if e.size() > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e.stored = stored
	if el, ok := c.items[k]; ok {
		c.removeLocked(el)
	}
	c.items[k] = c.ll.PushFront(&item{key: k, entry: e})
	c.bytes += e.size()
	for c.bytes > c.maxBytes {
		c.removeLocked(c.ll.Back())
	}
}

func (c *Cache) removeLocked(el *list.Element) {
	it := c.ll.Remove(el).(*item)
	delete(c.items, it.key)
	c.bytes -= it.entry.size()
}

// persisted is the on-disk form of one entry.
type persisted struct {
	Key     Key
	Status  int
	Header  http.Header
	Body    []byte
	Backend string
	Stored  time.Time
}

// Save writes every unexpired entry to w, oldest first.
func (c *Cache) Save(w io.Writer) error {
	c.mu.Lock()
	var out []persisted
	now := c.now()
	for el := c.ll.Back(); el != nil; el = el.Prev() {
		it := el.Value.(*item)
		if now.Sub(it.entry.stored) <= c.ttl {
			e := it.entry
			out = append(out, persisted{it.key, e.Status, e.Header, e.Body, e.Backend, e.stored})
		}
	}
	c.mu.Unlock()
	return gob.NewEncoder(w).Encode(out)
}

// Load adds entries saved by Save, skipping any that have expired since.
// It returns how many were restored.
func (c *Cache) Load(r io.Reader) (int, error) {
	var in []persisted
	if err := gob.NewDecoder(r).Decode(&in); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range in {
		if c.now().Sub(p.Stored) > c.ttl {
			continue
		}
		c.put(p.Key, &Entry{Status: p.Status, Header: p.Header, Body: p.Body, Backend: p.Backend}, p.Stored)
		n++
	}
	return n, nil
}

// Stats reports entry count and bytes held.
func (c *Cache) Stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.bytes
}
