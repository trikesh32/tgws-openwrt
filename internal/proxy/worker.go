package proxy

import (
	"context"
	"net"
	"net/netip"
	"sync"
)

func (s *Server) tryWorker(c net.Conn, consumed []byte, dst netip.AddrPort, label string) bool {
	key := "worker:" + dst.String()
	if s.cfg.DialWorker == nil || s.fails.blocked(key) {
		return false
	}
	w, err := s.cfg.DialWorker(context.Background(), dst)
	if err != nil {
		s.cfg.Stats.WorkerErrors.Add(1)
		s.fails.cooldown(key)
		s.cfg.Debugf("[%s] Worker unavailable: %v", label, err)
		return false
	}
	s.fails.clear(key)
	s.cfg.Stats.Worker.Add(1)
	s.cfg.Debugf("[%s] via Cloudflare Worker", label)
	// Hello/discovery may already have read part of the ORIGINAL TCP stream.
	// Replay it exactly once, before reading more from the client. Never retry
	// another transport after application bytes have been sent upstream.
	if len(consumed) > 0 {
		if err := w.Send(consumed); err != nil {
			_ = w.Close()
			return true
		}
		s.cfg.Stats.BytesUp.Add(int64(len(consumed)))
	}
	s.bridgeWorker(c, w)
	return true
}

// The TCP endpoint may split or coalesce data arbitrarily. No MTProto packet
// splitting, DC guessing or transport re-encryption belongs on this path.
func (s *Server) bridgeWorker(c net.Conn, w WSConn) {
	c = &timedConn{Conn: c, timeout: s.cfg.IOTimeout}
	var once sync.Once
	stop := func() { once.Do(func() { _ = c.Close(); _ = w.Close() }) }
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer stop()
		buf := make([]byte, 64<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if e := w.Send(buf[:n]); e != nil {
					return
				}
				s.cfg.Stats.BytesUp.Add(int64(n))
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer stop()
		for {
			b, err := w.Recv()
			if err != nil {
				return
			}
			n, err := c.Write(b)
			s.cfg.Stats.BytesDown.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	wg.Wait()
}
