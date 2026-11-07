package proxy

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sort"
	"time"

	"tgws/internal/mtproto"
)

// Определение DC перебором.
//
// Android в прямом соединении не пишет в init настоящий индекс DC, а адреса,
// которые он берёт из списка, присланного Telegram, зашитая таблица знать не
// может. Тогда демон отправляет первый пакет клиента на DC-кандидата и смотрит
// на ответ: транспортная ошибка -404 («ключ не найден») означает неверный DC,
// и пробуется следующий. Найденный DC запоминается для адреса.

// isWrongDCError сообщает, что ответ DC — транспортная ошибка, говорящая о
// неверном DC: -404 (ключ клиента неизвестен) или -444 (неверный DC). Ответ
// передаётся расшифрованным, в формате транспорта клиента. Другие ошибки
// (например, -429, слишком много запросов) о DC ничего не говорят, и перебор по
// ним продолжаться не должен.
func isWrongDCError(proto mtproto.Proto, plain []byte) bool {
	var code []byte
	switch proto {
	case mtproto.ProtoAbridged:
		// Один пакет из четырёх байт: длина в словах (1) и сам код.
		if len(plain) != 5 || plain[0] != 0x01 {
			return false
		}
		code = plain[1:]
	case mtproto.ProtoIntermediate, mtproto.ProtoPadded:
		// Длина 4 и код; в padded после них может идти до трёх байт добавки.
		if len(plain) < 8 || len(plain) > 11 || binary.LittleEndian.Uint32(plain) != 4 {
			return false
		}
		code = plain[4:8]
	default:
		return false
	}
	switch int32(binary.LittleEndian.Uint32(code)) {
	case -404, -444:
		return true
	}
	return false
}

// discovery — найденное соединение, готовое к мосту.
type discovery struct {
	dc    int
	conn  WSConn
	relay mtproto.Pair
	sp    *mtproto.Splitter
	// reply — первый ответ DC, уже расшифрованный потоком Telegram; клиенту его
	// ещё предстоит зашифровать своим потоком.
	reply []byte
}

func (s *Server) learnedDC(ip netip.Addr) (int, bool) {
	s.learnedMu.Lock()
	defer s.learnedMu.Unlock()
	dc, ok := s.learned[ip]
	return dc, ok
}

func (s *Server) learn(ip netip.Addr, dc int) {
	s.learnedMu.Lock()
	if len(s.learned) >= 1024 {
		clear(s.learned)
	}
	s.learned[ip] = dc
	s.learnedMu.Unlock()
}

// candidates — DC, на которые есть цель WebSocket и которые сейчас не на
// паузе (или на паузе, но прямой путь мёртв и откатываться некуда).
func (s *Server) candidates(dst netip.AddrPort) []int {
	var out []int
	for dc := range s.cfg.Targets {
		key := failKey(dc, false)
		if s.fails.blocked(key) && (s.fails.permanent(key) || s.direct.healthy(dst.String())) {
			continue
		}
		out = append(out, dc)
	}
	sort.Ints(out)
	return out
}

// firstReply ждёт первое сообщение DC не дольше ReplyWait. По таймауту
// закрывает соединение, чтобы не оставить висеть читающую горутину.
func (s *Server) firstReply(conn WSConn) ([]byte, bool) {
	type result struct {
		msg []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		m, err := conn.Recv()
		ch <- result{m, err}
	}()
	select {
	case r := <-ch:
		return r.msg, r.err == nil
	case <-time.After(s.cfg.ReplyWait):
		_ = conn.Close()
		return nil, false
	}
}

// discover пытается определить DC соединения, чей DC неизвестен. Вторым
// значением всегда возвращает байты, которые прочитал у клиента сверх
// приветствия: при неудаче они нужны для дословного повтора на прямом пути.
func (s *Server) discover(c net.Conn, h hello, dst netip.AddrPort, label string) (*discovery, []byte, error) {
	cands := s.candidates(dst)
	if len(cands) == 0 {
		return nil, nil, nil
	}

	// Без первого пакета клиента нечем проверять DC.
	_ = c.SetReadDeadline(time.Now().Add(s.cfg.DiscoverWait))
	buf := make([]byte, 64<<10)
	n, _ := c.Read(buf)
	_ = c.SetReadDeadline(time.Time{})
	raw := append([]byte(nil), buf[:n]...)
	if n == 0 {
		return nil, raw, nil
	}
	plain := append([]byte(nil), raw...)
	h.pair.Fwd.XORKeyStream(plain, plain)

	for _, dc := range cands {
		conn := s.connectWS(dc, false, s.cfg.Targets[dc], failKey(dc, false), label)
		if conn == nil {
			continue
		}
		relayInit, relay := mtproto.NewRelayInit(h.proto, int16(dc))
		sp := mtproto.NewSplitter(relayInit, h.proto, s.budget)
		enc := append([]byte(nil), plain...)
		relay.Fwd.XORKeyStream(enc, enc)
		parts, splitErr := sp.Split(enc)
		if splitErr != nil {
			sp.Close()
			_ = conn.Close()
			return nil, nil, splitErr
		}
		if len(parts) == 0 {
			// Первый пакет пришёл не целиком: DC ничего не ответит, и
			// проверить им нечем.
			sp.Close()
			_ = conn.Close()
			return nil, raw, nil
		}
		if conn.Send(relayInit) != nil || conn.Send(parts...) != nil {
			sp.Close()
			_ = conn.Close()
			continue
		}
		reply, ok := s.firstReply(conn)
		if !ok {
			s.cfg.Debugf("[%s] DC%d gave no answer to the first packet", label, dc)
			sp.Close()
			_ = conn.Close()
			continue
		}
		relay.Rev.XORKeyStream(reply, reply)
		if isWrongDCError(h.proto, reply) {
			s.cfg.Debugf("[%s] DC%d rejected the client (wrong DC), trying the next", label, dc)
			sp.Close()
			_ = conn.Close()
			continue
		}
		s.learn(dst.Addr(), dc)
		s.cfg.Logf("learned: %s belongs to DC%d", dst.Addr(), dc)
		return &discovery{dc: dc, conn: conn, relay: relay, sp: sp, reply: reply}, raw, nil
	}
	return nil, raw, nil
}
