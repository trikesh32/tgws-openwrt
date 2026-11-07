package mtproto

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

// MaxPayload is a router policy, allowing 1 MiB file chunks plus overhead.
const MaxPayload = 2 << 20
const maxBuffered = MaxPayload + 4 + (64 << 10)

var ErrPacketSize = errors.New("mtproto: invalid or oversized packet")
var ErrBufferBudget = errors.New("mtproto: shared buffer budget exhausted")

// Splitter режет поток, идущий в Telegram, на транспортные пакеты MTProto,
// чтобы каждый уходил в WebSocket отдельным кадром. Границы пакетов видны
// только в открытом тексте, поэтому разбиватель держит собственную копию
// потока шифрования стороны Telegram и расшифровывает ею то же, что уходит
// на провод.
type Splitter struct {
	dec    cipher.Stream
	proto  Proto
	cbuf   []byte // шифртекст, ещё не отданный наружу
	pbuf   []byte // тот же участок в открытом виде
	budget *Budget
	err    error
}

// NewSplitter строит разбиватель по init, отправленному в Telegram.
func NewSplitter(relayInit []byte, proto Proto, budgets ...*Budget) *Splitter {
	p := pairFrom(relayInit)
	// Пропустить 64 байта самого init: данные начинаются после него.
	skip := make([]byte, InitLen)
	p.Fwd.XORKeyStream(skip, skip)
	s := &Splitter{dec: p.Fwd, proto: proto}
	if len(budgets) > 0 {
		s.budget = budgets[0]
	}
	return s
}

// Close releases capacity on every disconnect/error path. It is idempotent.
func (s *Splitter) Close() {
	s.budget.release(cap(s.cbuf) + cap(s.pbuf))
	s.cbuf, s.pbuf = nil, nil
	if s.err == nil {
		s.err = errors.New("mtproto: splitter closed")
	}
}

func (s *Splitter) fail(err error) ([][]byte, error) {
	s.err = err
	s.Close()
	return nil, err
}

// Split returns complete packets, retaining a bounded partial tail. Errors
// are terminal; callers must close the connection and must not fall back.
func (s *Splitter) Split(chunk []byte) ([][]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(chunk) == 0 {
		return nil, nil
	}
	if len(chunk) > maxBuffered-len(s.cbuf) {
		return s.fail(ErrPacketSize)
	}
	need := len(s.cbuf) + len(chunk)
	if need > cap(s.cbuf) {
		// Reserve old AND new allocations during growth. Avoid append's
		// implicit capacity growth bypassing the shared budget.
		capacity := (need + 4095) / 4096 * 4096
		if !s.budget.acquire(2 * capacity) {
			return s.fail(ErrBufferBudget)
		}
		c, p := make([]byte, len(s.cbuf), capacity), make([]byte, len(s.pbuf), capacity)
		copy(c, s.cbuf)
		copy(p, s.pbuf)
		s.budget.release(cap(s.cbuf) + cap(s.pbuf))
		s.cbuf, s.pbuf = c, p
	}
	s.cbuf = append(s.cbuf, chunk...)
	start := len(s.pbuf)
	s.pbuf = s.pbuf[:need]
	s.dec.XORKeyStream(s.pbuf[start:], chunk)

	var parts [][]byte
	off := 0
	for off < len(s.cbuf) {
		n, more := s.nextLen(off)
		if more {
			break
		}
		if n <= 0 {
			return s.fail(ErrPacketSize)
		}
		parts = append(parts, append([]byte(nil), s.cbuf[off:off+n]...))
		off += n
	}
	// Остаток копируется вниз, чтобы буфер не рос вместе с потоком.
	s.cbuf = append(s.cbuf[:0], s.cbuf[off:]...)
	s.pbuf = append(s.pbuf[:0], s.pbuf[off:]...)
	return parts, nil
}

// Flush отдаёт накопленный хвост, когда клиент закрыл соединение.
func (s *Splitter) Flush() []byte {
	if len(s.cbuf) == 0 {
		return nil
	}
	tail := append([]byte(nil), s.cbuf...)
	s.cbuf = s.cbuf[:0]
	s.pbuf = s.pbuf[:0]
	return tail
}

// nextLen возвращает длину пакета, начинающегося в off. more=true — данных
// пока мало. n<=0 при more=false — длину прочитать нельзя.
func (s *Splitter) nextLen(off int) (n int, more bool) {
	avail := len(s.cbuf) - off
	switch s.proto {
	case ProtoAbridged:
		first := s.pbuf[off]
		var payload, header int
		if first == 0x7f || first == 0xff {
			if avail < 4 {
				return 0, true
			}
			payload = int(uint32(s.pbuf[off+1])|uint32(s.pbuf[off+2])<<8|uint32(s.pbuf[off+3])<<16) * 4
			header = 4
		} else {
			payload = int(first&0x7f) * 4
			header = 1
		}
		if payload <= 0 || payload > MaxPayload {
			return 0, false
		}
		if avail < header+payload {
			return 0, true
		}
		return header + payload, false
	case ProtoIntermediate, ProtoPadded:
		if avail < 4 {
			return 0, true
		}
		payload := int(binary.LittleEndian.Uint32(s.pbuf[off:]) & 0x7fffffff)
		if payload <= 0 || payload > MaxPayload {
			return 0, false
		}
		if avail < 4+payload {
			return 0, true
		}
		return 4 + payload, false
	}
	return 0, false
}
