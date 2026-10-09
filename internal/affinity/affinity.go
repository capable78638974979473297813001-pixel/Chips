// Package affinity remembers which backend last served each prompt prefix,
// so follow-up requests can be sent where the KV cache is already warm.
package affinity

import (
	"container/list"
	"sync"
	"time"
)

// Table is a bounded, TTL-expiring LRU map from prefix hash to backend name.
// It is safe for concurrent use.
type Table struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	ll       *list.List
	items    map[uint64]*list.Element
	now      func() time.Time
}

type entry struct {
	key     uint64
	backend string
	seen    time.Time
}

func New(capacity int, ttl time.Duration) *Table {
	return &Table{
		capacity: capacity,
		ttl:      ttl,
		ll:       list.New(),
		items:    make(map[uint64]*list.Element, capacity),
		now:      time.Now,
	}
}

// Lookup returns the backend recorded for the deepest key that is present.
// keys must be ordered shallowest to deepest; depth is the 1-based index of
// the matching key, or 0 when nothing matched.
func (t *Table) Lookup(keys []uint64) (backend string, depth int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for i := len(keys) - 1; i >= 0; i-- {
		el, ok := t.items[keys[i]]
		if !ok {
			continue
		}
		e := el.Value.(*entry)
		if now.Sub(e.seen) > t.ttl {
			t.removeElement(el)
			continue
		}
		return e.backend, i + 1
	}
	return "", 0
}

// Record maps every key to backend, refreshing recency.
func (t *Table) Record(keys []uint64, backend string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, k := range keys {
		if el, ok := t.items[k]; ok {
			e := el.Value.(*entry)
			e.backend, e.seen = backend, now
			t.ll.MoveToFront(el)
			continue
		}
		t.items[k] = t.ll.PushFront(&entry{key: k, backend: backend, seen: now})
		for t.ll.Len() > t.capacity {
			t.removeElement(t.ll.Back())
		}
	}
}

// Len reports the number of stored prefixes.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ll.Len()
}

func (t *Table) removeElement(el *list.Element) {
	t.ll.Remove(el)
	delete(t.items, el.Value.(*entry).key)
}
