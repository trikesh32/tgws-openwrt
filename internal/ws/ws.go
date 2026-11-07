// Package ws — минимальный клиент WebSocket (RFC 6455) поверх crypto/tls.
//
// net/http сюда намеренно не подключается: он добавляет около мегабайта к
// бинарнику, а на роутерах с 8–16 МБ флеша это заметно. Ответ на апгрейд
// разбирается вручную.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgws/internal/sockmark"
)

const (
	guid       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxMessage = 16 << 20

	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA
)

var errTooLarge = errors.New("ws: message too large")

// HandshakeError — сервер не согласился на апгрейд.
type HandshakeError struct {
	Status   int
	Line     string
	Location string
}

func (e *HandshakeError) Error() string { return "websocket handshake: " + e.Line }

// Redirect сообщает, что сервер ответил перенаправлением. Для kws-доменов это
// признак того, что WebSocket для данного DC закрыт.
func (e *HandshakeError) Redirect() bool {
	switch e.Status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// Dialer описывает одно подключение.
type Dialer struct {
	Addr       string      // host:port, куда идёт TCP (IP дата-центра)
	ServerName string      // SNI и заголовок Host (kws2.web.telegram.org)
	Path       string      // /apiws
	TLS        *tls.Config // nil — настройки по умолчанию; в тестах свой пул
	Mark       int         // SO_MARK, 0 — без метки
	Timeout    time.Duration
}

// Dial устанавливает TCP, TLS и выполняет апгрейд.
func (d Dialer) Dial(ctx context.Context) (*Conn, error) {
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	nd := net.Dialer{Control: sockmark.Control(d.Mark)}
	raw, err := nd.DialContext(ctx, "tcp", d.Addr)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{}
	if d.TLS != nil {
		cfg = d.TLS.Clone()
	}
	cfg.ServerName = d.ServerName
	tc := tls.Client(raw, cfg)
	if dl, ok := ctx.Deadline(); ok {
		// Срок действует на весь апгрейд: сервер, принявший TLS и замолчавший,
		// не должен держать горутину бесконечно.
		_ = tc.SetDeadline(dl)
	}
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		raw.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	req := "GET " + d.Path + " HTTP/1.1\r\n" +
		"Host: " + d.ServerName + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: binary\r\n\r\n"
	if _, err := io.WriteString(tc, req); err != nil {
		raw.Close()
		return nil, err
	}

	br := bufio.NewReader(tc)
	line, err := readHeaderLine(br)
	if err != nil {
		raw.Close()
		return nil, err
	}
	line = strings.TrimSpace(line)
	status := 0
	if f := strings.SplitN(line, " ", 3); len(f) >= 2 {
		status, _ = strconv.Atoi(f[1])
	}
	hdr := map[string]string{}
	for i := 0; ; i++ {
		l, err := readHeaderLine(br)
		if err != nil {
			raw.Close()
			return nil, err
		}
		l = strings.TrimSpace(l)
		if l == "" {
			break
		}
		if i > 100 {
			raw.Close()
			return nil, errors.New("ws: too many response headers")
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			hdr[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if status != 101 {
		raw.Close()
		return nil, &HandshakeError{Status: status, Line: line, Location: hdr["location"]}
	}
	sum := sha1.Sum([]byte(key + guid))
	if hdr["sec-websocket-accept"] != base64.StdEncoding.EncodeToString(sum[:]) {
		raw.Close()
		return nil, errors.New("ws: missing or bad Sec-WebSocket-Accept")
	}

	_ = tc.SetDeadline(time.Time{})
	return &Conn{c: tc, br: br}, nil
}

// Bound each header line before allocation (bufio's default buffer is 4 KiB).
func readHeaderLine(br *bufio.Reader) (string, error) {
	b, err := br.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Conn — установленное соединение. Send можно звать одновременно с Recv.
type Conn struct {
	c    net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
	frag []byte
	once sync.Once
}

func appendFrame(dst []byte, op byte, payload []byte) []byte {
	n := len(payload)
	dst = append(dst, 0x80|op)
	switch {
	case n < 126:
		dst = append(dst, 0x80|byte(n))
	case n < 65536:
		dst = append(dst, 0x80|126, byte(n>>8), byte(n))
	default:
		dst = append(dst, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	dst = append(dst, mask[:]...)
	start := len(dst)
	dst = append(dst, payload...)
	for i := range payload {
		dst[start+i] ^= mask[i&3]
	}
	return dst
}

func (c *Conn) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.c.Write(b)
	return err
}

// Send отправляет каждую часть отдельным бинарным кадром одним вызовом Write.
func (c *Conn) Send(parts ...[]byte) error {
	var buf []byte
	for _, p := range parts {
		buf = appendFrame(buf, opBinary, p)
	}
	return c.write(buf)
}

// Recv возвращает следующее сообщение. io.EOF — сервер закрыл соединение.
func (c *Conn) Recv() ([]byte, error) {
	for {
		op, payload, fin, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opClose:
			reply := payload
			if len(reply) > 2 {
				reply = reply[:2]
			}
			_ = c.write(appendFrame(nil, opClose, reply))
			return nil, io.EOF
		case opPing:
			_ = c.write(appendFrame(nil, opPong, payload))
		case opPong:
		case opCont, opText, opBinary:
			if fin && len(c.frag) == 0 {
				return payload, nil
			}
			c.frag = append(c.frag, payload...)
			if len(c.frag) > maxMessage {
				return nil, errTooLarge
			}
			if fin {
				msg := c.frag
				c.frag = nil
				return msg, nil
			}
		}
	}
}

// Close отправляет кадр закрытия и закрывает сокет; повторные вызовы безвредны.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		// Close the socket directly: never wait behind a blocked Send mutex.
		if nc, ok := c.c.(interface{ NetConn() net.Conn }); ok {
			err = nc.NetConn().Close()
		} else {
			err = c.c.Close()
		}
	})
	return err
}

func (c *Conn) readFrame() (op byte, payload []byte, fin bool, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0f
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(e[:])
	}
	if n > maxMessage || ((op == opCont || op == opText || op == opBinary) && n > uint64(maxMessage-len(c.frag))) {
		err = errTooLarge
		return
	}
	var mask [4]byte
	masked := h[1]&0x80 != 0
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return
}
