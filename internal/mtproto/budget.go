package mtproto

import "sync"

// Budget bounds live MTProto assembly buffer capacity, not total process RSS.
type Budget struct {
	mu          sync.Mutex
	used, limit int
}

func NewBudget(limit int) *Budget { return &Budget{limit: limit} }
func (b *Budget) acquire(n int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}
func (b *Budget) release(n int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}
