package proxy_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tgws/internal/mtproto"
	"tgws/internal/proxy"
	"tgws/internal/ws/wstest"
)

// fakeDC принимает соединения и сообщает первые want байт каждого.
type fakeDC struct {
	ln  net.Listener
	got chan []byte
}

func newFakeDC(t *testing.T, want int, reply ...[]byte) *fakeDC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDC{ln: ln, got: make(chan []byte, 4)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, want)
				if _, err := io.ReadFull(c, buf); err == nil {
					if len(reply) > 0 {
						_, _ = c.Write(reply[0])
					}
					d.got <- buf
				}
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return d
}

type harness struct {
	addr    string
	dialWS  atomic.Int32
	dialTCP atomic.Int32
	mu      sync.Mutex
	domains []string
	srv     *proxy.Server
	now     atomic.Int64 // секунды Unix: часы читает горутина сервера, двигает тест
}

// newHarness поднимает прокси. wsSrv может быть nil (WebSocket недоступен),
// dc — nil, если откат не ожидается.
func newHarness(t *testing.T, dst netip.AddrPort, wsSrv *wstest.Server, dc *fakeDC, options ...func(*proxy.Config)) *harness {
	t.Helper()
	h := &harness{}
	h.now.Store(1_000_000)
	cfg := proxy.Config{
		Targets: map[int]string{2: "149.154.167.220", 4: "149.154.167.220"},
		OrigDst: func(tc *net.TCPConn) (netip.AddrPort, error) {
			// Нулевой dst — «соединение пришло напрямую, без redirect»:
			// SO_ORIGINAL_DST тогда возвращает собственный адрес сокета.
			if !dst.IsValid() {
				return tc.LocalAddr().(*net.TCPAddr).AddrPort(), nil
			}
			return dst, nil
		},
		DialWS: func(ctx context.Context, target, domain, path string) (proxy.WSConn, error) {
			h.dialWS.Add(1)
			h.mu.Lock()
			h.domains = append(h.domains, domain)
			h.mu.Unlock()
			if wsSrv == nil {
				return nil, errors.New("websocket unavailable")
			}
			c, err := wsSrv.Dialer(path).Dial(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		DialTCP: func(ctx context.Context, addr string) (net.Conn, error) {
			h.dialTCP.Add(1)
			if dc == nil {
				return nil, errors.New("no fallback expected")
			}
			return net.Dial("tcp", dc.ln.Addr().String())
		},
		Now:          func() time.Time { return time.Unix(h.now.Load(), 0) },
		HelloTimeout: 300 * time.Millisecond,
	}
	for _, option := range options {
		option(&cfg)
	}
	h.srv = proxy.New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go h.srv.Serve(ln)
	h.addr = ln.Addr().String()
	return h
}

func (h *harness) firstDomain() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.domains) == 0 {
		return ""
	}
	return h.domains[0]
}

func (h *harness) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func intermediatePacket(payload []byte) []byte {
	p := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(p, uint32(len(payload)))
	copy(p[4:], payload)
	return p
}

func recvBytes(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for bytes")
		return nil
	}
}

func TestObfuscatedClientIsBridgedOverWebSocket(t *testing.T) {
	seen := make(chan []byte, 1)
	srv := wstest.New(t, func(p *wstest.Peer) {
		first, err := p.ReadBinary()
		if err != nil {
			return
		}
		init, err := mtproto.ParseInit(first) // поддельный Telegram — ответчик
		if err != nil || init.DCIdx != 2 || init.Proto != mtproto.ProtoIntermediate {
			t.Errorf("relay init: %+v, %v", init, err)
			return
		}
		pkt, _ := p.ReadBinary()
		init.Pair.Fwd.XORKeyStream(pkt, pkt)
		seen <- pkt
		reply := []byte("pong-from-telegram")
		init.Pair.Rev.XORKeyStream(reply, reply)
		p.WriteFrame(2, true, reply)
		p.ReadBinary() // ждём закрытия
	})
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), srv, nil)
	c := h.dial(t)

	rawInit, cl := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	c.Write(rawInit)
	msg := intermediatePacket([]byte("hello-telegram"))
	enc := append([]byte(nil), msg...)
	cl.Fwd.XORKeyStream(enc, enc)
	c.Write(enc)

	if got := recvBytes(t, seen); string(got) != string(msg) {
		t.Fatalf("telegram saw % x, want % x", got, msg)
	}
	reply := make([]byte, len("pong-from-telegram"))
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	cl.Rev.XORKeyStream(reply, reply)
	if string(reply) != "pong-from-telegram" {
		t.Fatalf("client saw %q", reply)
	}
	if h.srv.Stats().WS.Load() != 1 {
		t.Fatalf("WS counter = %d, want 1", h.srv.Stats().WS.Load())
	}
	if got := h.firstDomain(); got != "kws2.web.telegram.org" {
		t.Fatalf("first domain = %s", got)
	}
}

func TestMediaConnectionTriesTheAlternateDomainFirst(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) { p.ReadBinary(); p.ReadBinary() })
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), srv, nil)
	c := h.dial(t)
	rawInit, _ := mtproto.NewRelayInit(mtproto.ProtoAbridged, -2)
	c.Write(rawInit)
	deadline := time.Now().Add(2 * time.Second)
	for h.dialWS.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.firstDomain(); got != "kws2-1.web.telegram.org" {
		t.Fatalf("first domain = %q, want the alternate one for media", got)
	}
}

func TestPlainAbridgedClientIsBridgedByIP(t *testing.T) {
	seen := make(chan []byte, 1)
	srv := wstest.New(t, func(p *wstest.Peer) {
		first, _ := p.ReadBinary()
		init, err := mtproto.ParseInit(first)
		if err != nil || init.DCIdx != 2 || init.Proto != mtproto.ProtoAbridged {
			t.Errorf("relay init: %+v, %v", init, err)
			return
		}
		pkt, _ := p.ReadBinary()
		init.Pair.Fwd.XORKeyStream(pkt, pkt)
		seen <- pkt
		p.ReadBinary()
	})
	// DC2 известен по адресу: клиент без обфускации DC в потоке не называет.
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), srv, nil)
	c := h.dial(t)
	pkt := append([]byte{2}, 1, 2, 3, 4, 5, 6, 7, 8) // abridged: 2 слова
	c.Write(append([]byte{0xef}, pkt...))
	if got := recvBytes(t, seen); string(got) != string(pkt) {
		t.Fatalf("telegram saw % x, want % x", got, pkt)
	}
}

func TestRedirectFallsBackVerbatimAndBlacklistsTheDC(t *testing.T) {
	redirect := wstest.NewHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.org/")
		w.WriteHeader(http.StatusFound)
	})
	rawInit, _ := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	sent := append(append([]byte(nil), rawInit...), []byte("extra")...)
	dc := newFakeDC(t, len(sent))
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), redirect, dc)

	c := h.dial(t)
	c.Write(sent)
	if got := recvBytes(t, dc.got); string(got) != string(sent) {
		t.Fatal("the fallback must replay the client's bytes verbatim")
	}
	if n := h.dialWS.Load(); n != 2 {
		t.Fatalf("WS attempts = %d, want 2 (both domains)", n)
	}
	if h.srv.Stats().Fallback.Load() != 1 {
		t.Fatalf("fallback counter = %d", h.srv.Stats().Fallback.Load())
	}

	// Все домены ответили редиректом: DC в чёрном списке, второй клиент идёт
	// прямо, не тратя время на заведомо безнадёжные попытки.
	c2 := h.dial(t)
	c2.Write(sent)
	recvBytes(t, dc.got)
	if n := h.dialWS.Load(); n != 2 {
		t.Fatalf("WS attempts after the second client = %d, want still 2", n)
	}
}

func TestTransientFailureCoolsDownWithResponsiveFallbackThenRetries(t *testing.T) {
	rawInit, _ := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	dc := newFakeDC(t, len(rawInit), []byte("reply"))
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), nil, dc) // WS недоступен

	c := h.dial(t)
	c.Write(rawInit)
	recvBytes(t, dc.got)
	if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	first := h.dialWS.Load()
	if first == 0 {
		t.Fatal("the first client must try WebSocket")
	}

	h.dial(t).Write(rawInit)
	recvBytes(t, dc.got)
	if h.dialWS.Load() != first {
		t.Fatal("during the cooldown the DC must not be retried")
	}

	h.now.Add(120) // пауза в минуту истекла
	h.dial(t).Write(rawInit)
	recvBytes(t, dc.got)
	if h.dialWS.Load() <= first {
		t.Fatal("after the cooldown the DC must be tried again — a transient failure is not forever")
	}
}

func TestSilentDirectHandshakeDoesNotSuppressWebSocket(t *testing.T) {
	init, _ := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	dc := newFakeDC(t, len(init)) // Accepts TCP, never sends an application reply.
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), nil, dc)
	h.dial(t).Write(init)
	recvBytes(t, dc.got)
	first := h.dialWS.Load()
	h.dial(t).Write(init)
	recvBytes(t, dc.got)
	if h.dialWS.Load() <= first {
		t.Fatal("silent fallback incorrectly suppressed WS recovery")
	}
}

func TestBridgeCountsFirstTransportErrorAndPreservesClientReply(t *testing.T) {
	reply := []byte{4, 0, 0, 0, 0x6c, 0xfe, 0xff, 0xff}
	w := wstest.New(t, func(p *wstest.Peer) {
		b, err := p.ReadBinary()
		if err != nil {
			return
		}
		init, err := mtproto.ParseInit(b)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err = p.ReadBinary(); err != nil {
			return
		}
		enc := append([]byte(nil), reply...)
		init.Pair.Rev.XORKeyStream(enc, enc)
		p.WriteFrame(2, true, enc[:3])
		p.WriteFrame(2, true, enc[3:])
		p.ReadBinary()
	})
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), w, nil)
	c := h.dial(t)
	c.Write(append([]byte{0xee, 0xee, 0xee, 0xee}, intermediatePacket([]byte("test"))...))
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(reply) {
		t.Fatal("transport error was changed")
	}
	if h.srv.Stats().Snapshot().FirstReply404 != 1 {
		t.Fatal("transport error not counted")
	}
}

func TestCooldownDoesNotSendNewAddressToUnprovenDirectPath(t *testing.T) {
	seen := make(chan []byte, 1)
	w := wstest.New(t, func(p *wstest.Peer) {
		b, err := p.ReadBinary()
		if err == nil {
			seen <- b
		}
		p.ReadBinary()
	})
	var ready atomic.Bool
	var clients atomic.Int32
	pending := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, netip.AddrPort{}, nil, nil, func(cfg *proxy.Config) {
		cfg.OrigDst = func(*net.TCPConn) (netip.AddrPort, error) {
			if clients.Add(1) == 1 {
				return netip.MustParseAddrPort("149.154.167.51:443"), nil
			}
			return netip.MustParseAddrPort("149.154.167.50:443"), nil
		}
		cfg.DialWS = func(ctx context.Context, _, _, _ string) (proxy.WSConn, error) {
			if !ready.Load() {
				return nil, errors.New("temporary outage")
			}
			return w.Dialer("/apiws").Dial(ctx)
		}
		cfg.DialTCP = func(context.Context, string) (net.Conn, error) {
			select {
			case pending <- struct{}{}:
			default:
			}
			<-release
			return nil, errors.New("blocked direct path")
		}
	})
	init, _ := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	h.dial(t).Write(init)
	select {
	case <-pending:
	case <-time.After(time.Second):
		t.Fatal("first fallback not attempted")
	}
	ready.Store(true)
	h.dial(t).Write(init)
	recvBytes(t, seen)
}

// Прямой путь у многих пользователей заблокирован, и каждая попытка стоит
// полного таймаута. Клиент (Android) перебирает адреса по очереди и ждёт
// каждый: на живом роутере это растягивало подключение на минуты. Если
// прямая попытка к адресу только что провалилась, следующие соединения к нему
// закрываются сразу, чтобы клиент переходил к следующему адресу.
func TestFailedDirectPathIsNotRetriedImmediately(t *testing.T) {
	h := newHarness(t, netip.MustParseAddrPort("1.2.3.4:443"), nil, nil) // DC неизвестен, откат недоступен
	hello := append([]byte{0xef}, []byte("12345678")...)

	waitDials := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for h.dialTCP.Load() < want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := h.dialTCP.Load(); got != want {
			t.Fatalf("direct dials = %d, want %d", got, want)
		}
	}

	c1 := h.dial(t)
	c1.Write(hello)
	waitDials(1)

	c2 := h.dial(t)
	c2.Write(hello)
	_ = c2.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the second connection to be closed at once")
	}
	if got := h.dialTCP.Load(); got != 1 {
		t.Fatalf("a recently failed direct path was dialled again: %d dials", got)
	}

	h.now.Add(120) // пауза истекла: путь снова пробуется
	c3 := h.dial(t)
	c3.Write(hello)
	waitDials(2)
}

// Живая проверка: загрузка видео открывает несколько параллельных соединений,
// после неудачного открытия WebSocket DC вставал на паузу, и все остальные
// соединения уходили в прямой путь, который на этой сети заблокирован, —
// загрузка зависала. Пауза нужна, только когда прямой путь работает.
func TestPauseIsIgnoredWhenTheDirectPathIsDown(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) { p.ReadBinary(); p.ReadBinary() })
	var wsUp atomic.Bool // сначала WebSocket недоступен
	var dials atomic.Int32
	s := proxy.New(proxy.Config{
		Targets: map[int]string{2: "149.154.167.220"},
		OrigDst: func(*net.TCPConn) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("149.154.167.51:443"), nil
		},
		DialWS: func(ctx context.Context, target, domain, path string) (proxy.WSConn, error) {
			dials.Add(1)
			if !wsUp.Load() {
				return nil, errors.New("websocket unavailable")
			}
			c, err := srv.Dialer(path).Dial(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		DialTCP:      func(context.Context, string) (net.Conn, error) { return nil, errors.New("direct path is blocked") },
		HelloTimeout: 300 * time.Millisecond,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.Serve(ln)

	rawInit, _ := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	connect := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		c.Write(rawInit)
		return c
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !cond() {
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	// Первое соединение: WebSocket не открылся (пауза DC) и прямой путь тоже.
	connect()
	waitFor("the first connection to fail on both paths", func() bool { return s.Stats().Fallback.Load() == 1 })

	// WebSocket снова доступен, но DC ещё на паузе. Прямой путь заведомо
	// мёртв, поэтому пауза не должна отнимать у второго соединения WebSocket.
	wsUp.Store(true)
	connect()
	waitFor("the second connection to use WebSocket despite the pause", func() bool { return s.Stats().WS.Load() == 1 })
}

// Адрес, которого нет в зашитой таблице, можно привязать к DC вручную: Android
// в прямом соединении индекс DC не пишет, и без привязки его соединения к
// новым адресам уходят в заблокированный прямой путь.
func TestExtraDCMapServesAnUnknownAddress(t *testing.T) {
	seen := make(chan []byte, 1)
	srv := wstest.New(t, func(p *wstest.Peer) {
		first, _ := p.ReadBinary()
		init, err := mtproto.ParseInit(first)
		if err != nil || init.DCIdx != 2 {
			t.Errorf("relay init: %+v, %v", init, err)
			return
		}
		pkt, _ := p.ReadBinary()
		init.Pair.Fwd.XORKeyStream(pkt, pkt)
		seen <- pkt
		p.ReadBinary()
	})
	ip := netip.MustParseAddr("1.2.3.4")
	s := proxy.New(proxy.Config{
		Targets: map[int]string{2: "149.154.167.220"},
		ExtraDC: map[netip.Addr]int{ip: 2},
		OrigDst: func(*net.TCPConn) (netip.AddrPort, error) { return netip.AddrPortFrom(ip, 443), nil },
		DialWS: func(ctx context.Context, target, domain, path string) (proxy.WSConn, error) {
			c, err := srv.Dialer(path).Dial(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		DialTCP:      func(context.Context, string) (net.Conn, error) { return nil, errors.New("no fallback expected") },
		HelloTimeout: 300 * time.Millisecond,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Как у Android: обфусцированный init с ЧУЖИМ индексом DC (здесь 4735).
	rawInit, cl := mtproto.NewRelayInit(mtproto.ProtoAbridged, 4735)
	c.Write(rawInit)
	pkt := append([]byte{2}, 1, 2, 3, 4, 5, 6, 7, 8)
	enc := append([]byte(nil), pkt...)
	cl.Fwd.XORKeyStream(enc, enc)
	c.Write(enc)
	if got := recvBytes(t, seen); string(got) != string(pkt) {
		t.Fatalf("telegram saw % x, want % x", got, pkt)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// Живая проверка на роутере: первое TCP-соединение к IP дата-центра дважды
// подряд оборвалось по таймауту (троттлинг), а повтор на соседний домен того же
// DC прошёл. Пауза на минуту после единственного таймаута отдавала бы такой
// DC прямому TCP, который как раз и заблокирован.
func TestTimeoutOnTheFirstDomainTriesTheNextOne(t *testing.T) {
	srv := wstest.New(t, func(p *wstest.Peer) { p.ReadBinary(); p.ReadBinary() })
	var calls atomic.Int32

	cfgSrv := proxy.New(proxy.Config{
		Targets: map[int]string{2: "149.154.167.220"},
		OrigDst: func(*net.TCPConn) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("149.154.167.51:443"), nil
		},
		DialWS: func(ctx context.Context, target, domain, path string) (proxy.WSConn, error) {
			if calls.Add(1) == 1 {
				return nil, timeoutErr{}
			}
			c, err := srv.Dialer(path).Dial(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		DialTCP:      func(context.Context, string) (net.Conn, error) { return nil, errors.New("no fallback expected") },
		HelloTimeout: 300 * time.Millisecond,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go cfgSrv.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rawInit, _ := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	c.Write(rawInit)
	deadline := time.Now().Add(2 * time.Second)
	for cfgSrv.Stats().WS.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cfgSrv.Stats().WS.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("ws=%d dials=%d, want the second domain to succeed after one timeout",
			cfgSrv.Stats().WS.Load(), calls.Load())
	}
	if cfgSrv.Stats().Fallback.Load() != 0 {
		t.Fatal("a timeout on one domain must not send the client to the direct path")
	}
}

// DC неизвестен, и ни один кандидат его не принял (в этом стенде WebSocket
// недоступен вовсе): байты клиента, включая то, что прочитано при попытке
// определить DC, дословно уходят по назначению, а адрес попадает в счётчик
// неизвестных.
func TestUnknownPlainClientIsReplayedWhenNoDCAccepts(t *testing.T) {
	sent := append([]byte{0xef}, []byte("12345678")...)
	dc := newFakeDC(t, len(sent))
	h := newHarness(t, netip.MustParseAddrPort("1.2.3.4:443"), nil, dc)
	h.dial(t).Write(sent)
	if got := recvBytes(t, dc.got); string(got) != string(sent) {
		t.Fatal("bytes must be replayed verbatim")
	}
	if u := h.srv.Stats().Snapshot().UnknownDC; len(u) != 1 || u[0] != "1.2.3.4" {
		t.Fatalf("unknown_dc = %v", u)
	}
}

func TestNonMTProtoTrafficIsPassedThrough(t *testing.T) {
	req := make([]byte, 0, 80)
	for len(req) < 80 {
		req = append(req, "GET / HTTP/1.1\r\n"...)
	}
	dc := newFakeDC(t, len(req))
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:80"), nil, dc)
	h.dial(t).Write(req)
	if got := recvBytes(t, dc.got); string(got) != string(req) {
		t.Fatal("non-MTProto bytes must reach the original destination intact")
	}
	if h.dialWS.Load() != 0 {
		t.Fatal("HTTP must not be sent to a WebSocket")
	}
}

// requireDropped проверяет, что прокси закрыл соединение, не открыв ни
// WebSocket, ни прямого соединения.
func requireDropped(t *testing.T, h *harness, c net.Conn) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the proxy to close the connection")
	}
	if h.dialTCP.Load() != 0 || h.dialWS.Load() != 0 {
		t.Fatalf("a dropped connection must not dial anything: tcp=%d ws=%d", h.dialTCP.Load(), h.dialWS.Load())
	}
	if h.srv.Stats().Fallback.Load() != 0 {
		t.Fatal("a dropped connection must not count as a fallback")
	}
}

// Прямое подключение к порту демона (сканер, браузер, любопытный сосед по LAN)
// не проходило через redirect, и исходный адрес совпадает с адресом самого
// сокета. Откат дозвонился бы до самого себя, а каждый круг порождал бы новое
// соединение: до исчерпания дескрипторов и остановки Telegram во всей сети.
func TestDirectConnectionToTheListenPortIsDropped(t *testing.T) {
	h := newHarness(t, netip.AddrPort{}, nil, nil)
	c := h.dial(t)
	c.Write(append([]byte{0xef}, make([]byte, 80)...))
	requireDropped(t, h, c)
}

// Правило nft перехватывает только 80, 443 и 5222; всё остальное, что дошло до
// демона, перехвачено не было.
func TestUnexpectedDestinationPortIsDropped(t *testing.T) {
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:9999"), nil, nil)
	c := h.dial(t)
	c.Write(append([]byte{0xef}, make([]byte, 80)...))
	requireDropped(t, h, c)
}

// Клиент прислал часть приветствия и замолчал, ожидая ответа: это короткий
// запрос-ответ неизвестного протокола, а не зависший клиент. Его байты
// дословно уходят по назначению после HelloTimeout, а не теряются.
// (Полностью молчащий клиент, без единого байта, закрывается — см. тест ниже.)
func TestPartialHelloIsReplayedNotHeld(t *testing.T) {
	sent := []byte("ABCDEFGHIJ") // не HTTP, не MTProto, меньше 64 байт
	dc := newFakeDC(t, len(sent))
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), nil, dc)
	h.dial(t).Write(sent)
	if got := recvBytes(t, dc.got); string(got) != string(sent) {
		t.Fatalf("destination saw %q, want %q", got, sent)
	}
	if h.dialWS.Load() != 0 {
		t.Fatal("a partial hello must not open a WebSocket")
	}
}

func TestSilentClientIsReleased(t *testing.T) {
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.51:443"), nil, nil)
	c := h.dial(t)
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	// Клиент молчит: прокси обязан закрыть соединение по HelloTimeout, а не
	// держать его вечно, и не открывать откат ради пустого соединения.
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected the proxy to close the connection")
	}
	deadline := time.Now().Add(time.Second)
	for h.srv.Stats().Active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.srv.Stats().Active.Load() != 0 {
		t.Fatal("the connection goroutine leaked")
	}
	if h.srv.Stats().Fallback.Load() != 0 {
		t.Fatal("an empty connection must not trigger a fallback")
	}
}

func TestOversizedPacketClosesClientWithoutForwarding(t *testing.T) {
	gotBody := make(chan bool, 1)
	srv := wstest.New(t, func(p *wstest.Peer) {
		if _, err := p.ReadBinary(); err != nil {
			gotBody <- false
			return
		}
		_, err := p.ReadBinary()
		gotBody <- err == nil
	})
	h := newHarness(t, netip.MustParseAddrPort("149.154.167.50:443"), srv, nil)
	c := h.dial(t)
	defer c.Close()
	hello, pair := mtproto.NewRelayInit(mtproto.ProtoIntermediate, 2)
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, 1<<30)
	pair.Fwd.XORKeyStream(header, header)
	if _, err := c.Write(append(hello, header...)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("client not closed")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("client hung")
	}
	select {
	case body := <-gotBody:
		if body {
			t.Fatal("invalid packet reached WebSocket")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not closed")
	}
	if h.dialTCP.Load() != 0 {
		t.Fatal("rejected packet retried via direct TCP")
	}
}
