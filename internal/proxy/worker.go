package proxy

import (
	"net"
	"net/netip"
	"sync"
)

func (s *Server) directWorkerBlocked(dst netip.AddrPort) bool {
	return s.fails.blocked("worker:" + dst.String())
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
