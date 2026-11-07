package ws_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"tgws/internal/ws"
	"tgws/internal/ws/wstest"
)

func TestWorkerAuthHeaderAndNoRedirectFollowing(t *testing.T) {
	seen := make(chan *http.Request, 1)
	srv := wstest.NewHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		w.Header().Set("Location", "https://elsewhere.invalid/")
		w.WriteHeader(302)
	})
	d := srv.Dialer("/apiws?dst=149.154.167.51&port=443")
	d.BearerToken = strings.Repeat("a", 64)
	_, err := d.Dial(context.Background())
	var he *ws.HandshakeError
	if !errors.As(err, &he) || he.Status != 302 {
		t.Fatalf("redirect not rejected: %v", err)
	}
	r := <-seen
	if r.Header.Get("Authorization") != "Bearer "+d.BearerToken {
		t.Fatal("auth missing")
	}
	if strings.Contains(r.URL.String(), d.BearerToken) {
		t.Fatal("token in URL")
	}
	if r.URL.Query().Get("dst") != "149.154.167.51" {
		t.Fatal("destination lost")
	}
}

func TestHeaderInjectionRejectedBeforeDial(t *testing.T) {
	d := ws.Dialer{Addr: "must-not-dial.invalid:443", ServerName: "relay.example", Path: "/apiws", BearerToken: "x\r\nInjected: true"}
	if _, err := d.Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid request field") {
		t.Fatal(err)
	}
}

func TestMissingAcceptIsRejected(t *testing.T) {
	srv := wstest.NewHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	if c, err := srv.Dialer("/apiws").Dial(context.Background()); err == nil {
		c.Close()
		t.Fatal("accepted upgrade without accept header")
	}
}

func TestOversizedHeaderIsRejected(t *testing.T) {
	srv := wstest.NewHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("x", 8192))
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	if c, err := srv.Dialer("/apiws").Dial(context.Background()); err == nil {
		c.Close()
		t.Fatal("accepted oversized header")
	}
}

func dial(t *testing.T, s *wstest.Server) *ws.Conn {
	t.Helper()
	c, err := s.Dialer("/apiws").Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestEchoAndSeparateFrames(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) {
		for {
			data, err := p.ReadBinary()
			if err != nil {
				return
			}
			p.WriteFrame(2, true, data)
		}
	})
	c := dial(t, srv)
	if err := c.Send([]byte("one"), []byte("two")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two"} {
		got, err := c.Recv()
		if err != nil || string(got) != want {
			t.Fatalf("Recv = %q, %v; want %q", got, err, want)
		}
	}
}

func TestLargeFrameUsesExtendedLength(t *testing.T) {
	big := bytes.Repeat([]byte{0xab}, 70000)
	srv := wstest.New(t, func(p *wstest.Peer) {
		data, err := p.ReadBinary()
		if err != nil {
			return
		}
		p.WriteFrame(2, true, data)
	})
	c := dial(t, srv)
	if err := c.Send(big); err != nil {
		t.Fatal(err)
	}
	got, err := c.Recv()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("large frame round trip failed: len=%d err=%v", len(got), err)
	}
}

func TestPingIsAnsweredAndSkipped(t *testing.T) {
	pong := make(chan []byte, 1)
	srv := wstest.New(t, func(p *wstest.Peer) {
		p.WriteFrame(9, true, []byte("x"))
		p.WriteFrame(2, true, []byte("data"))
		op, payload, err := p.ReadFrame()
		if err == nil && op == 0xA {
			pong <- payload
		}
	})
	c := dial(t, srv)
	got, err := c.Recv()
	if err != nil || string(got) != "data" {
		t.Fatalf("Recv = %q, %v", got, err)
	}
	if p := <-pong; string(p) != "x" {
		t.Fatalf("pong payload = %q, want x", p)
	}
}

func TestFragmentedMessageIsAssembled(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) {
		p.WriteFrame(2, false, []byte("ab"))
		p.WriteFrame(0, true, []byte("cd"))
		p.ReadBinary()
	})
	c := dial(t, srv)
	got, err := c.Recv()
	if err != nil || string(got) != "abcd" {
		t.Fatalf("Recv = %q, %v; want abcd", got, err)
	}
}

func TestCloseFrameEndsWithEOF(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) {
		p.WriteFrame(8, true, []byte{0x03, 0xe8})
		p.ReadBinary()
	})
	c := dial(t, srv)
	if _, err := c.Recv(); err != io.EOF {
		t.Fatalf("Recv err = %v, want io.EOF", err)
	}
}

func TestRedirectIsReportedWithLocation(t *testing.T) {
	srv := wstest.NewHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.org/")
		w.WriteHeader(http.StatusFound)
	})
	_, err := srv.Dialer("/apiws").Dial(context.Background())
	var he *ws.HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *HandshakeError", err)
	}
	if !he.Redirect() || he.Status != 302 || he.Location != "https://example.org/" {
		t.Fatalf("unexpected handshake error: %+v", he)
	}
}

// Send, упёршийся в переполненный буфер, держит замок записи, а Close пишет
// кадр закрытия под тем же замком: без срока записи закрытие ждало бы
// таймаута TCP (около четверти часа), и мост не мог бы разобрать соединение.
func TestCloseIsNotBlockedByAStuckSend(t *testing.T) {
	release := make(chan struct{})
	srv := wstest.New(t, func(p *wstest.Peer) { <-release }) // сервер ничего не читает
	t.Cleanup(func() { close(release) })
	c := dial(t, srv)

	go func() {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 256; i++ { // 256 МиБ — больше любых буферов сокета
			if c.Send(chunk) != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // Send упирается в полный буфер

	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung behind a blocked Send")
	}
}

func TestOversizedFrameIsRejected(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) {
		// Заявленная длина 1 ТиБ при крошечном теле: клиент не должен пытаться
		// выделить память под неё.
		p.Conn.Write([]byte{0x82, 127, 0, 0, 1, 0, 0, 0, 0, 0})
		p.ReadBinary()
	})
	c := dial(t, srv)
	if _, err := c.Recv(); err == nil {
		t.Fatal("an oversized frame must be an error")
	}
}
