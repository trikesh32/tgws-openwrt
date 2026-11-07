package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"sync/atomic"
	"testing"
	"tgws/internal/proxy"
	"tgws/internal/ws/wstest"
)

func TestWorkerPreservesRawStreamUnknownDC(t *testing.T) {
	// This is deliberately not MTProto; Worker must not parse/re-encrypt it.
	payload := bytes.Repeat([]byte("GET raw opaque TCP\x00\xff"), 20000)
	seen := make(chan []byte, 1)
	w := wstest.New(t, func(p *wstest.Peer) {
		_ = p.WriteFrame(2, true, []byte("ready")) // before any client hello
		var all []byte
		for len(all) < len(payload) {
			b, err := p.ReadBinary()
			if err != nil {
				return
			}
			all = append(all, b...)
		}
		seen <- all
		_ = p.WriteFrame(2, true, []byte("reply-"))
		_ = p.WriteFrame(2, true, []byte("tail"))
		_, _, _ = p.ReadFrame() // stay alive until client closes
	})
	dst := netip.MustParseAddrPort("149.154.175.211:5222")
	h := newHarness(t, dst, nil, nil, func(cfg *proxy.Config) {
		cfg.DialWorker = func(ctx context.Context, got netip.AddrPort) (proxy.WSConn, error) {
			if got != dst {
				return nil, errors.New("lost original destination")
			}
			return w.Dialer("/apiws").Dial(ctx)
		}
	})
	c := h.dial(t)
	ready := make([]byte, 5)
	if _, err := io.ReadFull(c, ready); err != nil || string(ready) != "ready" {
		t.Fatalf("banner %q: %v", ready, err)
	}
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	if got := recvBytes(t, seen); !bytes.Equal(got, payload) {
		t.Fatal("raw stream changed")
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil || string(reply) != "reply-tail" {
		t.Fatalf("reply %q: %v", reply, err)
	}
	if h.dialTCP.Load() != 0 || h.dialWS.Load() != 0 {
		t.Fatal("old path used despite Worker success")
	}
	if h.srv.Stats().Snapshot().Worker != 1 {
		t.Fatal("missing Worker counter")
	}
}

func TestWorkerHandshakeFailureFallsBackWithoutLosingBytesAndCoolsDown(t *testing.T) {
	payload := []byte("GET / HTTP/1.1\r\nHost: telegram.org\r\n\r\n")
	dc := newFakeDC(t, len(payload))
	var calls atomic.Int32
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:80"), nil, dc, func(cfg *proxy.Config) {
		cfg.DialWorker = func(context.Context, netip.AddrPort) (proxy.WSConn, error) {
			calls.Add(1)
			return nil, errors.New("HTTP 502")
		}
	})
	for i := 0; i < 2; i++ {
		c := h.dial(t)
		if _, err := c.Write(payload); err != nil {
			t.Fatal(err)
		}
		if got := recvBytes(t, dc.got); !bytes.Equal(got, payload) {
			t.Fatal("fallback lost bytes")
		}
		c.Close()
	}
	if calls.Load() != 1 {
		t.Fatal("failed Worker retried during cooldown")
	}
	if h.srv.Stats().Snapshot().WorkerErrors != 1 {
		t.Fatal("missing error counter")
	}
}
