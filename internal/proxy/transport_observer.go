package proxy

import (
	"encoding/binary"
	"tgws/internal/mtproto"
)

// Inspect only the first transport header. Never retain or log message bodies.
// This is observational: even error bytes are forwarded unchanged to the client.
type transportObserver struct {
	proto  mtproto.Proto
	prefix [8]byte
	n      int
	done   bool
}

func (o *transportObserver) observe(b []byte, st *Stats) int32 {
	if o.done {
		return 0
	}
	need, offset := 8, 4
	if o.proto == mtproto.ProtoAbridged {
		need, offset = 5, 1
	}
	o.n += copy(o.prefix[o.n:need], b)
	if o.n < need {
		return 0
	}
	o.done = true
	if o.proto == mtproto.ProtoAbridged {
		if o.prefix[0] != 1 {
			return 0
		}
	} else {
		n := binary.LittleEndian.Uint32(o.prefix[:4])
		if o.proto == mtproto.ProtoPadded {
			if n < 4 || n > 19 {
				return 0
			}
		} else if o.proto != mtproto.ProtoIntermediate || n != 4 {
			return 0
		}
	}
	code := int32(binary.LittleEndian.Uint32(o.prefix[offset:need]))
	switch code {
	case -404:
		st.FirstReply404.Add(1)
	case -429:
		st.FirstReply429.Add(1)
	case -444:
		st.FirstReply444.Add(1)
	default:
		return 0
	}
	return code
}
