// Package affinity remembers which backends recently served each prompt
// prefix, so follow-up requests can be sent where the KV cache is already warm.
package affinity

import (
	"container/list"
	"sync"
	"time"
)

// MaxHolders is how many backends one prefix remembers. A prefix many
// requests share, like a system prompt, ends up cached on several backends;
// remembering them all lets a new conversation go to the least busy one
// instead of to whichever served the prefix last.
const MaxHolders = 4

// Table is a bounded, TTL-expiring LRU map from prefix hash to the backends
// holding that prefix. It is safe for concurrent use.
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
	holders []holder // most recent first
}

type holder struct {
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

// Lookup returns the backends recorded for the deepest key that is present,
// most recent first. keys must be ordered shallowest to deepest; depth is
// the 1-based index of the matching key, or 0 when nothing matched.
func (t *Table) Lookup(keys []uint64) (backends []string, depth int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for i := len(keys) - 1; i >= 0; i-- {
		el, ok := t.items[keys[i]]
		if !ok {
			continue
		}
		e := el.Value.(*entry)
		live := e.holders[:0]
		for _, h := range e.holders {
			if now.Sub(h.seen) <= t.ttl {
				live = append(live, h)
			}
		}
		e.holders = live
		if len(live) == 0 {
			t.removeElement(el)
			continue
		}
		for _, h := range live {
			backends = append(backends, h.backend)
		}
		return backends, i + 1
	}
	return nil, 0
}

// Record notes that backend holds every key, refreshing recency.
func (t *Table) Record(keys []uint64, backend string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, k := range keys {
		if el, ok := t.items[k]; ok {
			e := el.Value.(*entry)
			e.holders = addHolder(e.holders, holder{backend, now})
			t.ll.MoveToFront(el)
			continue
		}
		t.items[k] = t.ll.PushFront(&entry{key: k, holders: []holder{{backend, now}}})
		for t.ll.Len() > t.capacity {
			t.removeElement(t.ll.Back())
		}
	}
}

// addHolder puts h first, dropping an older record of the same backend and
// the least recent one past MaxHolders.
func addHolder(hs []holder, h holder) []holder {
	if len(hs) > 0 && hs[0].backend == h.backend { // the usual case: no change of order
		hs[0].seen = h.seen
		return hs
	}
	out := append(make([]holder, 0, min(len(hs)+1, MaxHolders)), h)
	for _, o := range hs {
		if o.backend != h.backend && len(out) < MaxHolders {
			out = append(out, o)
		}
	}
	return out
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
