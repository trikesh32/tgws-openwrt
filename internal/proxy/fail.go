package proxy

import (
	"sync"
	"time"
)

// failTracker помнит, каким DC пока не стоит предлагать WebSocket.
//
// Два разных состояния. Пауза (cooldown) — для сбоев, которые могут пройти:
// сеть роутера ещё не поднялась, таймаут. Через cool DC пробуется снова.
// Чёрный список (blacklist) — для случая, когда ВСЕ домены DC ответили
// перенаправлением: это решение сервера, и оно не меняется до перезапуска.
type failTracker struct {
	mu    sync.Mutex
	now   func() time.Time
	cool  time.Duration
	until map[string]time.Time
	black map[string]bool
}

func newFailTracker(now func() time.Time, cool time.Duration) *failTracker {
	return &failTracker{now: now, cool: cool, until: map[string]time.Time{}, black: map[string]bool{}}
}

func (f *failTracker) blocked(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.black[key] {
		return true
	}
	t, ok := f.until[key]
	return ok && f.now().Before(t)
}

func (f *failTracker) cooldown(key string) {
	f.mu.Lock()
	if len(f.until) >= 1024 {
		clear(f.until)
	}
	f.until[key] = f.now().Add(f.cool)
	f.mu.Unlock()
}

func (f *failTracker) blacklist(key string) {
	f.mu.Lock()
	f.black[key] = true
	f.mu.Unlock()
}

// clear снимает паузу после успешного соединения; чёрный список не трогает.
func (f *failTracker) clear(key string) {
	f.mu.Lock()
	delete(f.until, key)
	f.mu.Unlock()
}
