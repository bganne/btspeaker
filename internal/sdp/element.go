package sdp

import (
	"encoding/binary"
	"errors"
)

// Data element types (SDP spec 3.2).
const (
	tNil  = 0
	tUint = 1
	tInt  = 2
	tUUID = 3
	tStr  = 4
	tBool = 5
	tSeq  = 6
	tAlt  = 7
	tURL  = 8
)

// elem is an SDP data element. For tUint/tInt/tUUID(16/32 bit) the value is u and
// n is the width in bytes; 128-bit UUIDs and wide ints keep their bytes in b.
type elem struct {
	t    byte
	n    int
	u    uint64
	b    []byte
	kids []elem
}

type uuid [16]byte

var baseUUID = uuid{0, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0x80, 0x5F, 0x9B, 0x34, 0xFB}

func uintE(n int, v uint64) elem { return elem{t: tUint, n: n, u: v} }
func uuidE(v uint16) elem        { return elem{t: tUUID, n: 2, u: uint64(v)} }
func strE(s string) elem         { return elem{t: tStr, b: []byte(s)} }
func seqE(k ...elem) elem        { return elem{t: tSeq, kids: k} }

func (e elem) uuid() uuid {
	if e.n == 16 {
		var u uuid
		copy(u[:], e.b)
		return u
	}
	u := baseUUID
	binary.BigEndian.PutUint32(u[:4], uint32(e.u))
	return u
}

func (e elem) containsUUID(u uuid) bool {
	if e.t == tUUID && e.uuid() == u {
		return true
	}
	for _, k := range e.kids {
		if k.containsUUID(u) {
			return true
		}
	}
	return false
}

func sizeIdx(n int) (idx byte, hdr []byte) {
	switch {
	case n < 256:
		return 5, []byte{byte(n)}
	case n < 65536:
		return 6, binary.BigEndian.AppendUint16(nil, uint16(n))
	}
	return 7, binary.BigEndian.AppendUint32(nil, uint32(n))
}

func (e elem) encode() []byte {
	switch e.t {
	case tNil:
		return []byte{0}
	case tUint, tInt, tUUID:
		var idx byte
		switch e.n {
		case 1:
			idx = 0
		case 2:
			idx = 1
		case 4:
			idx = 2
		case 8:
			idx = 3
		default:
			idx = 4
		}
		out := []byte{e.t<<3 | idx}
		if e.n > 8 || e.n == 16 {
			return append(out, e.b...)
		}
		for i := e.n - 1; i >= 0; i-- {
			out = append(out, byte(e.u>>(8*i)))
		}
		return out
	case tBool:
		return []byte{e.t<<3 | 0, byte(e.u)}
	case tStr, tURL:
		idx, h := sizeIdx(len(e.b))
		return append(append([]byte{e.t<<3 | idx}, h...), e.b...)
	}
	// sequence / alternative
	var body []byte
	for _, k := range e.kids {
		body = append(body, k.encode()...)
	}
	idx, h := sizeIdx(len(body))
	return append(append([]byte{e.t<<3 | idx}, h...), body...)
}

var errElem = errors.New("sdp: bad data element")

// maxKids caps the elements in one sequence: valid requests need far fewer,
// and a pre-auth peer must not make us allocate one elem per payload byte.
const maxKids = 32

// decode parses one data element, returning it and the bytes consumed.
func decode(b []byte) (elem, int, error) {
	return decodeDepth(b, 0)
}

func decodeDepth(b []byte, depth int) (elem, int, error) {
	if len(b) < 1 || depth > 8 {
		return elem{}, 0, errElem
	}
	t, idx := b[0]>>3, b[0]&7
	pos := 1
	var size int
	switch {
	case t == tNil:
		if idx != 0 {
			return elem{}, 0, errElem // NIL has size index 0
		}
		return elem{t: tNil}, 1, nil
	case idx <= 4:
		if t == tStr || t == tURL || t == tSeq || t == tAlt {
			return elem{}, 0, errElem // variable-size types need a size index >= 5
		}
		size = []int{1, 2, 4, 8, 16}[idx]
		if t == tBool {
			if idx != 0 {
				return elem{}, 0, errElem // Bool has size index 0
			}
			size = 1
		}
	default:
		if t == tUint || t == tInt || t == tUUID {
			return elem{}, 0, errElem // fixed-size types only use size indexes 0-4
		}
		w := []int{1, 2, 4}[idx-5]
		if len(b) < pos+w {
			return elem{}, 0, errElem
		}
		switch w {
		case 1:
			size = int(b[pos])
		case 2:
			size = int(binary.BigEndian.Uint16(b[pos:]))
		default:
			v := binary.BigEndian.Uint32(b[pos:])
			if v > 1<<20 {
				return elem{}, 0, errElem
			}
			size = int(v)
		}
		pos += w
	}
	if len(b) < pos+size {
		return elem{}, 0, errElem
	}
	body := b[pos : pos+size]
	e := elem{t: t}
	switch t {
	case tUint, tInt, tUUID, tBool:
		if t == tUUID && size != 2 && size != 4 && size != 16 {
			return elem{}, 0, errElem
		}
		e.n = size
		if size == 16 {
			e.b = append([]byte(nil), body...)
		} else {
			for _, c := range body {
				e.u = e.u<<8 | uint64(c)
			}
		}
	case tStr, tURL:
		e.b = append([]byte(nil), body...)
	case tSeq, tAlt:
		for len(body) > 0 {
			k, n, err := decodeDepth(body, depth+1)
			if err != nil {
				return elem{}, 0, err
			}
			if len(e.kids) == maxKids {
				return elem{}, 0, errElem
			}
			e.kids = append(e.kids, k)
			body = body[n:]
		}
	default:
		return elem{}, 0, errElem
	}
	return e, pos + size, nil
}
