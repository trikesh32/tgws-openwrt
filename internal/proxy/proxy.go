// Package proxy — ядро демона: принимает перехваченные соединения, определяет
// дата-центр, ведёт трафик через WebSocket или, если это не вышло, прямым TCP.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"tgws/internal/dcmap"
	"tgws/internal/mtproto"
	"tgws/internal/ws"
)

const wsPath = "/apiws"

// WSConn — то, что ядру нужно от WebSocket; *ws.Conn ему удовлетворяет.
type WSConn interface {
	Send(parts ...[]byte) error
	Recv() ([]byte, error)
	Close() error
}

// Config собирает зависимости; всё внешнее (сеть, адреса, часы) подменяемо.
type Config struct {
	Targets        map[int]string     // DC → IP для WebSocket
	ExtraDC        map[netip.Addr]int // ручная привязка адресов к DC поверх зашитой таблицы
	OrigDst        func(*net.TCPConn) (netip.AddrPort, error)
	DialWS         func(ctx context.Context, target, domain, path string) (WSConn, error)
	DialTCP        func(ctx context.Context, addr string) (net.Conn, error)
	DialWorker     func(ctx context.Context, dst netip.AddrPort) (WSConn, error)
	WorkerFirst    bool // false: keep Telegram WebSocket first, Worker before direct fallback
	Stats          *Stats
	Logf           func(format string, args ...any) // события, о которых стоит знать
	Debugf         func(format string, args ...any) // ход каждого соединения
	Now            func() time.Time
	HelloTimeout   time.Duration
	Cooldown       time.Duration
	MaxConnections int
	MaxPerIP       int
	IOTimeout      time.Duration
	// DiscoverWait — сколько ждать первый пакет клиента при определении DC
	// перебором; ReplyWait — сколько ждать ответ DC-кандидата.
	DiscoverWait time.Duration
	ReplyWait    time.Duration
}

// Server обслуживает перехваченные соединения.
type Server struct {
	cfg         Config
	slots       chan struct{}
	admissionMu sync.Mutex
	perIP       map[string]int
	budget      *mtproto.Budget
	fails       *failTracker
	// direct помнит адреса, до которых прямая попытка только что не дошла.
	direct *failTracker
	// learned — адреса, чей DC определён перебором (см. discover.go).
	learnedMu sync.Mutex
	learned   map[netip.Addr]int
}

// New подставляет значения по умолчанию.
func New(cfg Config) *Server {
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 64
	}
	if cfg.MaxPerIP <= 0 {
		cfg.MaxPerIP = 16
	}
	if cfg.IOTimeout <= 0 {
		cfg.IOTimeout = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HelloTimeout == 0 {
		cfg.HelloTimeout = 10 * time.Second
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = time.Minute
	}
	if cfg.DiscoverWait == 0 {
		cfg.DiscoverWait = 2 * time.Second
	}
	if cfg.ReplyWait == 0 {
		cfg.ReplyWait = 3 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Debugf == nil {
		cfg.Debugf = func(string, ...any) {}
	}
	if cfg.Stats == nil {
		cfg.Stats = NewStats()
	}
	return &Server{
		cfg:     cfg,
		slots:   make(chan struct{}, cfg.MaxConnections),
		perIP:   map[string]int{},
		budget:  mtproto.NewBudget(16 << 20),
		fails:   newFailTracker(cfg.Now, cfg.Cooldown),
		direct:  newFailTracker(cfg.Now, cfg.Cooldown),
		learned: map[netip.Addr]int{},
	}
}

// Stats отдаёт счётчики.
func (s *Server) Stats() *Stats { return s.cfg.Stats }

// Serve принимает соединения, пока слушатель не закрыт.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Нехватка дескрипторов и подобное: не падаем, а ждём.
			s.cfg.Logf("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		key, _, err := net.SplitHostPort(c.RemoteAddr().String())
		if err != nil || !s.admit(key) {
			c.Close()
			continue
		}
		go func() { defer s.release(key); s.handle(c) }()
	}
}

func failKey(dc int, media bool) string {
	if media {
		return fmt.Sprintf("%dm", dc)
	}
	return fmt.Sprintf("%d", dc)
}

// pickDC предпочитает DC из init клиента; открытому клиенту DC подсказывает
// только адрес назначения.
func (s *Server) pickDC(h hello, ip netip.Addr) (dc int, media, ok bool) {
	// Ручная привязка сильнее всего остального: так её и задают, когда
	// остальное не дало ответа (Android пишет в init мусорный индекс).
	if d, found := s.cfg.ExtraDC[ip]; found {
		return d, false, true
	}
	if h.hasDC {
		dc = int(h.dcIdx)
		if dc < 0 {
			dc, media = -dc, true
		}
		if dcmap.Valid(dc) {
			return dc, media, true
		}
		// Тестовые DC (10000+) и прочее нестандартное — не наше дело.
	}
	if dc, ok = dcmap.ByIP(ip); ok {
		return dc, false, true
	}
	// Выученное перебором — последним: индекс из init клиента и зашитая
	// таблица надёжнее, чем вывод по ответам DC.
	dc, ok = s.learnedDC(ip)
	return dc, false, ok
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	st := s.cfg.Stats
	st.Active.Add(1)
	st.Total.Add(1)
	defer st.Active.Add(-1)

	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	dst, err := s.cfg.OrigDst(tc)
	if err != nil {
		s.cfg.Logf("original destination: %v", err)
		return
	}
	label := c.RemoteAddr().String() + " -> " + dst.String()

	// Соединение, которое не проходило через redirect, отвечает на
	// SO_ORIGINAL_DST собственным адресом сокета. Откат дозвонился бы тогда до
	// самого демона, тот принял бы это соединение и снова откатился на себя:
	// цепочка до исчерпания дескрипторов, а вместе с ней перестаёт работать
	// Telegram во всей сети. Достаточно одного любопытного клиента на порту.
	if local, ok := c.LocalAddr().(*net.TCPAddr); ok {
		lp := local.AddrPort()
		if dst.Addr().Unmap() == lp.Addr().Unmap() && dst.Port() == lp.Port() {
			s.cfg.Debugf("[%s] not redirected, dropping", label)
			return
		}
	}
	// Правило nft перенаправляет только эти порты; остальное дошло до демона
	// мимо перехвата, и обслуживать это не наше дело.
	switch dst.Port() {
	case 80, 443, 5222:
	default:
		s.cfg.Debugf("[%s] unexpected destination port, dropping", label)
		return
	}
	// Worker carries the original TCP stream, including unknown transports
	// and DCs. Fall back only before any client bytes have been consumed.
	if s.cfg.WorkerFirst && s.tryWorker(c, nil, dst, label) {
		return
	}

	_ = c.SetReadDeadline(time.Now().Add(s.cfg.HelloTimeout))
	h := readHello(c)
	_ = c.SetReadDeadline(time.Time{})
	if h.err != nil {
		if len(h.consumed) == 0 {
			// Клиент ничего не сказал: откатываться не из-за чего.
			return
		}
		// Первые байты нужны, чтобы понять, что за клиент пришёл: без них
		// «не MTProto» не отличить от TLS, обфускации с secret или нового
		// транспорта.
		head := h.consumed
		if len(head) > 24 {
			head = head[:24]
		}
		s.fallback(c, h.consumed, dst, label, fmt.Sprintf("not MTProto (%v), first bytes % x", h.err, head))
		return
	}

	dc, media, ok := s.pickDC(h, dst.Addr())
	if !ok {
		// DC неизвестен: пробуем определить перебором по ответам DC. Всё, что
		// клиент успел прислать сверх приветствия, нужно сохранить для отката.
		d, extra, discoveryErr := s.discover(c, h, dst, label)
		if discoveryErr != nil {
			s.cfg.Debugf("[%s] rejected: %v", label, discoveryErr)
			return
		}
		if d != nil {
			st.WS.Add(1)
			st.BytesUp.Add(int64(len(extra)))
			s.cfg.Debugf("[%s] DC%d (discovered) via WebSocket", label, d.dc)
			started := time.Now()
			end := bridge(&timedConn{Conn: c, timeout: s.cfg.IOTimeout}, d.conn, h.pair, d.relay, d.sp, st, d.reply)
			s.cfg.Debugf("[%s] DC%d session closed by %s (%v): up %d B, down %d B, %.1fs",
				label, d.dc, end.by, end.err, end.up, end.down, time.Since(started).Seconds())
			return
		}
		h.consumed = append(append([]byte(nil), h.consumed...), extra...)
		if st.NoteUnknown(dst.Addr().String()) {
			s.cfg.Logf("no DC known for %s (transport %#x, obfuscated=%v, dc index in init=%d, %d bytes read): extend dcmap if clients keep using it",
				dst.Addr(), uint32(h.proto), h.hasDC, h.dcIdx, len(h.consumed))
		}
		s.fallback(c, h.consumed, dst, label, "unknown DC")
		return
	}
	target, has := s.cfg.Targets[dc]
	key := failKey(dc, media)
	if !has {
		s.fallback(c, h.consumed, dst, label, fmt.Sprintf("no WebSocket target for DC%d", dc))
		return
	}
	if s.fails.blocked(key) {
		// Пауза отдаёт DC прямому пути, поэтому имеет смысл, только пока он
		// работает. Там, где он заблокирован, пауза лишь ломала соединения:
		// загрузка видео открывает несколько параллельных соединений, и все
		// они, пока длилась пауза, уходили в заведомо мёртвый путь.
		if !s.direct.blocked(dst.Addr().String()) {
			s.fallback(c, h.consumed, dst, label, fmt.Sprintf("DC%d WebSocket is paused", dc))
			return
		}
		s.cfg.Debugf("[%s] DC%d is paused, but the direct path is down: trying WebSocket anyway", label, dc)
	}
	conn := s.connectWS(dc, media, target, key, label)
	if conn == nil {
		s.fallback(c, h.consumed, dst, label, fmt.Sprintf("DC%d WebSocket failed", dc))
		return
	}

	idx := int16(dc)
	if media {
		idx = -idx
	}
	relayInit, relay := mtproto.NewRelayInit(h.proto, idx)
	if err := conn.Send(relayInit); err != nil {
		_ = conn.Close()
		s.fallback(c, h.consumed, dst, label, fmt.Sprintf("DC%d WebSocket write: %v", dc, err))
		return
	}
	st.WS.Add(1)
	s.cfg.Debugf("[%s] DC%d media=%v via WebSocket", label, dc, media)
	started := time.Now()
	end := bridge(&timedConn{Conn: c, timeout: s.cfg.IOTimeout}, conn, h.pair, relay, mtproto.NewSplitter(relayInit, h.proto, s.budget), st, nil)
	// Конец сессии нужен, чтобы отличить зависание от обрыва: без него в
	// журнале видно только начало.
	s.cfg.Debugf("[%s] DC%d session closed by %s (%v): up %d B, down %d B, %.1fs",
		label, dc, end.by, end.err, end.up, end.down, time.Since(started).Seconds())
}

// connectWS перебирает домены DC. Решение об отказе принимает здесь же:
// все домены ответили перенаправлением — DC закрыт для WebSocket насовсем,
// иначе (таймаут, ошибка) — пауза.
func (s *Server) connectWS(dc int, media bool, target, key, label string) WSConn {
	redirects, attempts := 0, 0
	for _, domain := range dcmap.Domains(dc, media) {
		attempts++
		conn, err := s.cfg.DialWS(context.Background(), target, domain, wsPath)
		if err == nil {
			s.fails.clear(key)
			return conn
		}
		s.cfg.Stats.WSErrors.Add(1)
		var he *ws.HandshakeError
		switch {
		case errors.As(err, &he) && he.Redirect():
			redirects++
			s.cfg.Debugf("[%s] %s answered %d -> %s", label, domain, he.Status, he.Location)
		case isTimeout(err):
			// Таймаут не прекращает перебор: на живом роутере первое
			// TCP-соединение к IP дата-центра обрывалось (троттлинг), а
			// повтор на соседний домен того же DC проходил. Пауза нужна,
			// только если не удался ни один домен.
			s.cfg.Debugf("[%s] %s timed out", label, domain)
		default:
			s.cfg.Debugf("[%s] %s: %v", label, domain, err)
		}
	}
	if redirects == attempts {
		s.fails.blacklist(key)
		s.cfg.Logf("DC%d: every WebSocket domain redirects, using direct TCP until restart", dc)
	} else {
		s.fails.cooldown(key)
	}
	return nil
}

// fallback ведёт соединение прямым TCP на исходный адрес. Прочитанные у
// клиента байты повторяются дословно, поэтому перешифровка не нужна.
func (s *Server) fallback(c net.Conn, consumed []byte, dst netip.AddrPort, label, why string) {
	if !s.cfg.WorkerFirst && s.tryWorker(c, consumed, dst, label) {
		return
	}
	c = &timedConn{Conn: c, timeout: s.cfg.IOTimeout}
	s.cfg.Stats.Fallback.Add(1)
	s.cfg.Debugf("[%s] direct: %s", label, why)

	// Если прямая попытка к адресу только что не дошла, повторять её на каждом
	// соединении значит заставлять клиента ждать полный таймаут каждый раз:
	// Android перебирает адреса по очереди, и подключение растягивалось на
	// минуты. Клиент получает отказ сразу и идёт к следующему адресу.
	key := dst.Addr().String()
	if s.direct.blocked(key) {
		s.cfg.Debugf("[%s] direct path failed recently, dropping at once", label)
		return
	}
	up, err := s.cfg.DialTCP(context.Background(), dst.String())
	if err != nil {
		s.direct.cooldown(key)
		s.cfg.Debugf("[%s] direct dial: %v", label, err)
		return
	}
	s.direct.clear(key)
	defer up.Close()
	up = &timedConn{Conn: up, timeout: s.cfg.IOTimeout}
	if len(consumed) > 0 {
		if _, err := up.Write(consumed); err != nil {
			return
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(to, from net.Conn) {
		defer wg.Done()
		_, err := io.Copy(to, from)
		if err != nil {
			_ = to.Close()
			_ = from.Close()
			return
		}
		// Preserve TCP half-close so the other direction can finish its
		// response. Its I/O deadline still bounds peers that never close.
		if cw, ok := to.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go pipe(up, c)
	go pipe(c, up)
	wg.Wait()
}

// bridge связывает клиента и WebSocket, перешифровывая поток в обе стороны:
// клиентский шифр снимается, шифр стороны Telegram накладывается.
//
// initial — первый ответ DC, уже прочитанный при определении DC перебором и
// расшифрованный потоком Telegram; клиенту он уходит первым.
func bridge(c net.Conn, w WSConn, client, relay mtproto.Pair, sp *mtproto.Splitter, st *Stats, initial []byte) sessionEnd {
	defer sp.Close()
	var up, down atomic.Int64
	var first sync.Once
	var end sessionEnd
	// Кто завершил сессию первым и по какой причине: вторая сторона уже
	// рвётся из-за закрытия и сама по себе ничего не говорит.
	finish := func(by string, err error) {
		first.Do(func() { end.by, end.err = by, err })
	}
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			// Сначала клиентский сокет: его закрытие не блокируется, а
			// закрытие WebSocket может ждать записи.
			_ = c.Close()
			_ = w.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // клиент → Telegram
		defer wg.Done()
		defer shutdown()
		buf := make([]byte, 64<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				b := buf[:n]
				client.Fwd.XORKeyStream(b, b)
				relay.Fwd.XORKeyStream(b, b)
				st.BytesUp.Add(int64(n))
				up.Add(int64(n))
				parts, splitErr := sp.Split(b)
				if splitErr != nil {
					finish("invalid client packet", splitErr)
					return
				}
				if len(parts) > 0 {
					if serr := w.Send(parts...); serr != nil {
						finish("upstream write", serr)
						return
					}
				}
			}
			if err != nil {
				finish("client", err)
				// Incomplete packets are discarded on disconnect.
				return
			}
		}
	}()

	go func() { // Telegram → клиент
		defer wg.Done()
		defer shutdown()
		if len(initial) > 0 {
			client.Rev.XORKeyStream(initial, initial)
			st.BytesDown.Add(int64(len(initial)))
			down.Add(int64(len(initial)))
			if _, err := c.Write(initial); err != nil {
				finish("client write", err)
				return
			}
		}
		for {
			msg, err := w.Recv()
			if err != nil {
				finish("upstream", err)
				return
			}
			relay.Rev.XORKeyStream(msg, msg)
			client.Rev.XORKeyStream(msg, msg)
			st.BytesDown.Add(int64(len(msg)))
			down.Add(int64(len(msg)))
			if _, err := c.Write(msg); err != nil {
				finish("client write", err)
				return
			}
		}
	}()
	wg.Wait()
	end.up, end.down = up.Load(), down.Load()
	return end
}

// sessionEnd описывает, как закончилась сессия через WebSocket.
type sessionEnd struct {
	by       string // кто завершил первым
	err      error
	up, down int64
}
