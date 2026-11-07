package proxy

import (
	"bytes"
	"encoding/binary"
	"testing"
	"tgws/internal/mtproto"
)

func TestTransportObserverHandlesFragmentedFirstErrorWithoutChangingBytes(t *testing.T) {
	for _, proto := range []mtproto.Proto{mtproto.ProtoAbridged, mtproto.ProtoIntermediate, mtproto.ProtoPadded} {
		for _, code := range []int32{-404, -429, -444} {
			var packet []byte
			if proto == mtproto.ProtoAbridged {
				packet = []byte{1}
			} else {
				packet = []byte{4, 0, 0, 0}
			}
			packet = binary.LittleEndian.AppendUint32(packet, uint32(code))
			for split := 0; split <= len(packet); split++ {
				st := NewStats()
				o := transportObserver{proto: proto}
				original := append([]byte(nil), packet...)
				a := o.observe(packet[:split], st)
				b := o.observe(packet[split:], st)
				if a+b != code {
					t.Fatalf("proto=%x split=%d codes=%d,%d", proto, split, a, b)
				}
				o.observe(packet, st)
				if st.FirstReply404.Load()+st.FirstReply429.Load()+st.FirstReply444.Load() != 1 {
					t.Fatal("must count once")
				}
				if !bytes.Equal(packet, original) {
					t.Fatal("observer changed traffic")
				}
			}
		}
	}
}

func TestTransportObserverDoesNotClassifyPayloadOrQuickAckAsError(t *testing.T) {
	for _, proto := range []mtproto.Proto{mtproto.ProtoAbridged, mtproto.ProtoIntermediate, mtproto.ProtoPadded} {
		for _, packet := range [][]byte{
			{0x80, 0, 0, 0, 0x6c, 0xfe, 0xff, 0xff},
			{8, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0},
			{32, 0, 0, 0, 0x6c, 0xfe, 0xff, 0xff},
		} {
			o := transportObserver{proto: proto}
			if o.observe(packet, NewStats()) != 0 {
				t.Fatal("ordinary payload/ACK reported as error")
			}
		}
	}
}
