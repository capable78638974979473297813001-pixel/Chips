// Package cache keeps recent inference responses in memory so an identical
// request can be answered without touching a GPU.
package cache

import (
	"container/list"
	"crypto/sha256"
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
	bytes    int64
	maxBytes int64
	ttl      time.Duration
	now      func() time.Time
}

func New(maxBytes int64, ttl time.Duration) *Cache {
	return &Cache{ll: list.New(), items: map[Key]*list.Element{}, maxBytes: maxBytes, ttl: ttl, now: time.Now}
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
func (c *Cache) Put(k Key, e *Entry) {
	if e.size() > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e.stored = c.now()
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

// Stats reports entry count and bytes held.
func (c *Cache) Stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.bytes
}
