package proxy

import (
	"net"
	"time"
)

func (s *Server) admit(ip string) bool {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.perIP[ip] >= s.cfg.MaxPerIP {
		return false
	}
	select {
	case s.slots <- struct{}{}:
		s.perIP[ip]++
		return true
	default:
		return false
	}
}
func (s *Server) release(ip string) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	<-s.slots
	s.perIP[ip]--
	if s.perIP[ip] == 0 {
		delete(s.perIP, ip)
	}
}

// Bound every stalled read/write on both WebSocket and direct paths.
type timedConn struct {
	net.Conn
	timeout time.Duration
}

func (c *timedConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}
func (c *timedConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

func (c *timedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
