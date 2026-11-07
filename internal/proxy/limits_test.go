package proxy

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmissionLimitsAndReuse(t *testing.T) {
	s := New(Config{MaxConnections: 3, MaxPerIP: 2})
	if !s.admit("a") || !s.admit("a") || s.admit("a") || !s.admit("b") || s.admit("c") {
		t.Fatal("limits not enforced")
	}
	s.release("a")
	if !s.admit("c") {
		t.Fatal("slot not reusable")
	}
	s.release("a")
	s.release("b")
	s.release("c")
	if len(s.perIP) != 0 {
		t.Fatal("client keys leaked")
	}
}

func TestConcurrentAdmission(t *testing.T) {
	s := New(Config{MaxConnections: 3, MaxPerIP: 2})
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.admit("one-device") {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 2 {
		t.Fatal("concurrent per-IP limit failed")
	}
	s.release("one-device")
	s.release("one-device")
}

func TestStalledReadTimesOut(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &timedConn{Conn: a, timeout: 20 * time.Millisecond}
	_, err := c.Read(make([]byte, 1))
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("got %v", err)
	}
}
