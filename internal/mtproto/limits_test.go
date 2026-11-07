package mtproto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestOversizedLengthRejectedBeforeBody(t *testing.T) {
	for _, proto := range []Proto{ProtoIntermediate, ProtoPadded, ProtoAbridged} {
		s, enc := newSplitter(t, proto)
		header := make([]byte, 4)
		if proto == ProtoAbridged {
			copy(header, []byte{0x7f, 0xff, 0xff, 0xff})
		} else {
			binary.LittleEndian.PutUint32(header, 1<<30)
		}
		wire := enc(header)
		// Fragmented headers must not evade validation.
		for i := 0; i < 3; i++ {
			if _, err := s.Split(wire[i : i+1]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Split(wire[3:]); err != ErrPacketSize {
			t.Fatalf("%x: got %v", proto, err)
		}
		if cap(s.cbuf)+cap(s.pbuf) != 0 {
			t.Fatal("failed parser retained memory")
		}
		if len(s.Flush()) != 0 {
			t.Fatal("invalid data forwarded on EOF")
		}
	}
}

func TestSharedBudgetAndRelease(t *testing.T) {
	b := NewBudget(8192)
	newS := func() *Splitter { return &Splitter{dec: identity{}, proto: ProtoIntermediate, budget: b} }
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, 1024)
	a, c := newS(), newS()
	if _, err := a.Split(header); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Split(header); err != ErrBufferBudget {
		t.Fatalf("got %v", err)
	}
	a.Close()
	a.Close()
	c.Close()
	if b.used != 0 {
		t.Fatalf("leaked %d bytes", b.used)
	}
	d := newS()
	defer d.Close()
	if _, err := d.Split(header); err != nil {
		t.Fatal("released capacity not reusable", err)
	}
}

func TestMaximumPacketAndFollowingPacket(t *testing.T) {
	s, enc := newSplitter(t, ProtoIntermediate)
	defer s.Close()
	plain := append(intermediate(MaxPayload), intermediate(16)...)
	wire := enc(plain)
	var parts [][]byte
	for off := 0; off < len(wire); {
		end := min(off+65536, len(wire))
		p, err := s.Split(wire[off:end])
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p...)
		off = end
	}
	if len(parts) != 2 || !bytes.Equal(bytes.Join(parts, nil), wire) {
		t.Fatal("valid boundary packet corrupted")
	}
}

func FuzzSplitterBounded(f *testing.F) {
	f.Add([]byte{0xff, 0xff, 0xff, 0x7f})
	f.Add([]byte{4, 0, 0, 0, 1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, data []byte) {
		b := NewBudget(64 << 10)
		s := &Splitter{dec: identity{}, proto: ProtoIntermediate, budget: b}
		for len(data) > 0 {
			n := min(len(data), 1024)
			_, err := s.Split(data[:n])
			data = data[n:]
			if err != nil {
				break
			}
		}
		s.Close()
		if b.used != 0 {
			t.Fatal("budget leaked")
		}
	})
}
