package server

import (
	"sync"
	"time"
)

// retryBudget caps retries at a share of recent requests, so a fleet that
// is failing because it is overloaded is not hit with retry storms on top
// (each failure turning into two or three more requests). Within the
// window, retries may reach ratio × requests plus minPerSecond × window
// seconds; the floor lets a quiet router still retry its odd failure.
type retryBudget struct {
	ratio        float64 // < 0: no budget, retries are unlimited
	minPerSecond float64

	mu      sync.Mutex
	buckets [10]budgetBucket // one per second, reused round-robin
	now     func() time.Time
}

type budgetBucket struct {
	second            int64
	requests, retries float64
}

func newRetryBudget(ratio, minPerSecond float64) *retryBudget {
	return &retryBudget{ratio: ratio, minPerSecond: minPerSecond, now: time.Now}
}

func (b *retryBudget) set(ratio, minPerSecond float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ratio, b.minPerSecond = ratio, minPerSecond
}

// bucketLocked returns this second's bucket, clearing it if it last held
// an older second.
func (b *retryBudget) bucketLocked() *budgetBucket {
	sec := b.now().Unix()
	bk := &b.buckets[sec%int64(len(b.buckets))]
	if bk.second != sec {
		bk.second, bk.requests, bk.retries = sec, 0, 0
	}
	return bk
}

// request records a new client request.
func (b *retryBudget) request() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bucketLocked().requests++
}

// allowsLocked reports whether one more retry fits in the budget.
func (b *retryBudget) allowsLocked() bool {
	if b.ratio < 0 {
		return true
	}
	b.bucketLocked()
	oldest := b.now().Unix() - int64(len(b.buckets)) + 1
	var requests, retries float64
	for _, bk := range b.buckets {
		if bk.second >= oldest {
			requests += bk.requests
			retries += bk.retries
		}
	}
	return retries+1 <= b.ratio*requests+b.minPerSecond*float64(len(b.buckets))
}

// allows reports whether a retry would fit, without spending it.
func (b *retryBudget) allows() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allowsLocked()
}

// spend records a retry if it fits in the budget and reports whether it did.
func (b *retryBudget) spend() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.allowsLocked() {
		return false
	}
	b.bucketLocked().retries++
	return true
}
