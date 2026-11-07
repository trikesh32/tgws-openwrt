package mtproto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// newSplitter возвращает разбиватель и функцию, которой «прокси» шифрует
// данные в сторону Telegram (тот же Fwd, что вернул NewRelayInit).
func newSplitter(t *testing.T, proto Proto) (*Splitter, func([]byte) []byte) {
	t.Helper()
	raw, relay := NewRelayInit(proto, 2)
	enc := func(plain []byte) []byte {
		out := append([]byte(nil), plain...)
		relay.Fwd.XORKeyStream(out, out)
		return out
	}
	return NewSplitter(raw, proto), enc
}

func abridged(words int) []byte {
	p := make([]byte, 1+words*4)
	p[0] = byte(words)
	for i := 1; i < len(p); i++ {
		p[i] = byte(i)
	}
	return p
}

func intermediate(payload int) []byte {
	p := make([]byte, 4+payload)
	binary.LittleEndian.PutUint32(p, uint32(payload))
	return p
}

func TestSplitsTwoAbridgedPackets(t *testing.T) {
	sp, enc := newSplitter(t, ProtoAbridged)
	p1, p2 := abridged(2), abridged(3)
	wire := enc(append(append([]byte(nil), p1...), p2...))

	parts := mustSplit(t, sp, wire)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if !bytes.Equal(parts[0], wire[:len(p1)]) || !bytes.Equal(parts[1], wire[len(p1):]) {
		t.Fatal("parts do not match the ciphertext boundaries")
	}
}

func TestHoldsPartialPacketUntilComplete(t *testing.T) {
	sp, enc := newSplitter(t, ProtoIntermediate)
	wire := enc(intermediate(20))

	if parts := mustSplit(t, sp, wire[:10]); len(parts) != 0 {
		t.Fatalf("partial packet leaked: %d parts", len(parts))
	}
	parts := mustSplit(t, sp, wire[10:])
	if len(parts) != 1 || !bytes.Equal(parts[0], wire) {
		t.Fatalf("complete packet not released intact: %d parts", len(parts))
	}
}

func TestExtendedAbridgedLength(t *testing.T) {
	sp, enc := newSplitter(t, ProtoAbridged)
	words := 200 // не помещается в 7 бит: длина идёт тремя байтами после 0x7f
	p := make([]byte, 4+words*4)
	p[0] = 0x7f
	p[1], p[2], p[3] = byte(words), byte(words>>8), byte(words>>16)
	wire := enc(p)
	parts := mustSplit(t, sp, wire)
	if len(parts) != 1 || !bytes.Equal(parts[0], wire) {
		t.Fatalf("extended-length packet: %d parts", len(parts))
	}
}

func TestRejectsZeroLength(t *testing.T) {
	sp, enc := newSplitter(t, ProtoAbridged)
	if _, err := sp.Split(enc([]byte{0, 1, 2, 3})); err != ErrPacketSize {
		t.Fatalf("got %v", err)
	}
	if _, err := sp.Split(enc([]byte{9, 9})); err != ErrPacketSize {
		t.Fatal("failure must be terminal")
	}
}

func mustSplit(t *testing.T, sp *Splitter, b []byte) [][]byte {
	t.Helper()
	parts, err := sp.Split(b)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func TestFlushReturnsTail(t *testing.T) {
	sp, enc := newSplitter(t, ProtoIntermediate)
	wire := enc(intermediate(20))
	mustSplit(t, sp, wire[:7])
	tail := sp.Flush()
	if !bytes.Equal(tail, wire[:7]) {
		t.Fatalf("flush = % x, want % x", tail, wire[:7])
	}
	if sp.Flush() != nil {
		t.Fatal("second flush must be empty")
	}
}
