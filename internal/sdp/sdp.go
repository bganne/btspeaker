// Package sdp is a minimal SDP server exposing static A2DP-sink and AVRCP records.
package sdp

import (
	"encoding/binary"
	"sort"
	"time"

	"btspeaker/internal/bt"
)

const PSM = 1

// SDP PDU ids.
const (
	pduError         = 0x01
	pduSearchReq     = 0x02
	pduSearchRsp     = 0x03
	pduAttrReq       = 0x04
	pduAttrRsp       = 0x05
	pduSearchAttrReq = 0x06
	pduSearchAttrRsp = 0x07
)

// SDP error codes.
const (
	errBadVersion  = 0x0001
	errBadHandle   = 0x0002
	errBadSyntax   = 0x0003
	errBadPDUSize  = 0x0004
	errBadContinue = 0x0005
)

type record struct {
	handle uint32
	attrs  map[uint16]elem
	ids    []uint16 // sorted
}

type Server struct {
	records []*record // ascending handle
}

func newRecord(handle uint32, attrs map[uint16]elem) *record {
	attrs[0x0000] = uintE(4, uint64(handle))
	r := &record{handle: handle, attrs: attrs}
	for id := range attrs {
		r.ids = append(r.ids, id)
	}
	sort.Slice(r.ids, func(i, j int) bool { return r.ids[i] < r.ids[j] })
	return r
}

func u16(v uint16) elem { return uintE(2, uint64(v)) }

func browse() elem { return seqE(uuidE(0x1002)) }

func langBase() elem {
	// "en" (0x656E), UTF-8 (106), base attribute id 0x0100
	return seqE(u16(0x656E), u16(0x006A), u16(0x0100))
}

func avrcpProto() elem {
	return seqE(
		seqE(uuidE(0x0100), u16(0x0017)),
		seqE(uuidE(0x0017), u16(0x0104)), // AVCTP 1.4
	)
}

// NewServer builds the static record set (SDP server, A2DP Sink, AVRCP TG cat-2 + CT).
func NewServer() *Server {
	s := &Server{}
	s.records = []*record{
		newRecord(0x00000000, map[uint16]elem{
			0x0001: seqE(uuidE(0x1000)),
			0x0004: seqE(seqE(uuidE(0x0100), u16(PSM)), seqE(uuidE(0x0001))),
			0x0005: browse(),
			0x0200: seqE(u16(0x0100)),
			0x0201: uintE(4, 0),
		}),
		newRecord(0x00010001, map[uint16]elem{ // A2DP Sink
			0x0001: seqE(uuidE(0x110B)),
			0x0004: seqE(seqE(uuidE(0x0100), u16(0x0019)), seqE(uuidE(0x0019), u16(0x0103))),
			0x0005: browse(),
			0x0006: langBase(),
			0x0009: seqE(seqE(uuidE(0x110D), u16(0x0103))),
			0x0100: strE("Audio Sink"),
			0x0311: u16(0x0002), // SupportedFeatures: Speaker
		}),
		newRecord(0x00010002, map[uint16]elem{ // AVRCP Target
			0x0001: seqE(uuidE(0x110C)),
			0x0004: avrcpProto(),
			0x0005: browse(),
			0x0006: langBase(),
			0x0009: seqE(seqE(uuidE(0x110E), u16(0x0104))),
			0x0100: strE("AVRCP Target"),
			0x0311: u16(0x0002),
		}),
		newRecord(0x00010003, map[uint16]elem{ // AVRCP Controller
			0x0001: seqE(uuidE(0x110E), uuidE(0x110F)),
			0x0004: avrcpProto(),
			0x0005: browse(),
			0x0006: langBase(),
			0x0009: seqE(seqE(uuidE(0x110E), u16(0x0106))),
			0x0100: strE("AVRCP Controller"),
			0x0311: u16(0x0001),
		}),
	}
	return s
}

// Connection timeouts (when the Conn supports deadlines): the first PDU must arrive quickly,
// later ones within idleTimeout, and the whole connection is cut after maxLifetime so a
// slow peer cannot hold one of the few pre-auth slots forever.
const (
	firstPDUTimeout = 5 * time.Second
	idleTimeout     = 15 * time.Second
	maxLifetime     = 60 * time.Second
)

// HandleConn serves one accepted PSM 1 channel until it closes (blocks).
func (s *Server) HandleConn(c bt.Conn) {
	defer c.Close()
	n := c.InMTU()
	if n < 672 {
		n = 672
	}
	buf := make([]byte, n)
	dl, _ := c.(interface{ SetDeadline(time.Time) error })
	start := time.Now()
	end := start.Add(maxLifetime)
	timeout := firstPDUTimeout
	for {
		if dl != nil {
			d := time.Now().Add(timeout)
			if d.After(end) {
				d = end
			}
			dl.SetDeadline(d)
		}
		l, err := c.Read(buf)
		if err != nil {
			return
		}
		timeout = idleTimeout
		if rsp := s.handle(buf[:l], c.OutMTU()); rsp != nil {
			if _, err := c.Write(rsp); err != nil {
				return
			}
		}
	}
}

func pdu(id byte, tid uint16, params []byte) []byte {
	out := make([]byte, 5, 5+len(params))
	out[0] = id
	binary.BigEndian.PutUint16(out[1:], tid)
	binary.BigEndian.PutUint16(out[3:], uint16(len(params)))
	return append(out, params...)
}

func errRsp(tid uint16, code uint16) []byte {
	return pdu(pduError, tid, binary.BigEndian.AppendUint16(nil, code))
}

// handle processes one request PDU and returns the response (nil = drop).
func (s *Server) handle(req []byte, outMTU int) []byte {
	if len(req) < 5 {
		return nil
	}
	id := req[0]
	tid := binary.BigEndian.Uint16(req[1:])
	plen := int(binary.BigEndian.Uint16(req[3:]))
	if plen != len(req)-5 {
		return errRsp(tid, errBadPDUSize)
	}
	p := req[5:]
	if outMTU < 48 {
		outMTU = 48
	}
	switch id {
	case pduSearchReq:
		return s.serviceSearch(tid, p, outMTU)
	case pduAttrReq:
		return s.serviceAttr(tid, p, outMTU)
	case pduSearchAttrReq:
		return s.serviceSearchAttr(tid, p, outMTU)
	}
	return errRsp(tid, errBadSyntax)
}

// parseCont reads the trailing continuation state; returns the offset.
func parseCont(p []byte) (off int, ok bool) {
	if len(p) < 1 {
		return 0, false
	}
	switch p[0] {
	case 0:
		return 0, len(p) == 1
	case 2:
		if len(p) != 3 {
			return 0, false
		}
		return int(binary.BigEndian.Uint16(p[1:])), true
	}
	return 0, false
}

func contBytes(next int) []byte {
	if next < 0 {
		return []byte{0}
	}
	return []byte{2, byte(next >> 8), byte(next)}
}

func (s *Server) matching(pattern []uuid) []*record {
	var out []*record
	for _, r := range s.records {
		ok := true
		for _, u := range pattern {
			found := false
			for _, e := range r.attrs {
				if e.containsUUID(u) {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

func parsePattern(p []byte) ([]uuid, int, bool) {
	e, n, err := decode(p)
	if err != nil || e.t != tSeq || len(e.kids) == 0 || len(e.kids) > 12 {
		return nil, 0, false
	}
	var us []uuid
	for _, k := range e.kids {
		if k.t != tUUID {
			return nil, 0, false
		}
		us = append(us, k.uuid())
	}
	return us, n, true
}

type idRange struct{ lo, hi uint16 }

func parseAttrList(p []byte) ([]idRange, int, bool) {
	e, n, err := decode(p)
	if err != nil || e.t != tSeq || len(e.kids) == 0 {
		return nil, 0, false
	}
	var rs []idRange
	for _, k := range e.kids {
		if k.t != tUint {
			return nil, 0, false
		}
		switch k.n {
		case 2:
			rs = append(rs, idRange{uint16(k.u), uint16(k.u)})
		case 4:
			rs = append(rs, idRange{uint16(k.u >> 16), uint16(k.u)})
		default:
			return nil, 0, false
		}
	}
	return rs, n, true
}

func (r *record) attrList(rs []idRange) elem {
	var kids []elem
	for _, id := range r.ids {
		for _, g := range rs {
			if id >= g.lo && id <= g.hi {
				kids = append(kids, u16(id), r.attrs[id])
				break
			}
		}
	}
	return seqE(kids...)
}

func (s *Server) serviceSearch(tid uint16, p []byte, outMTU int) []byte {
	pat, n, ok := parsePattern(p)
	if !ok || len(p) < n+3 {
		return errRsp(tid, errBadSyntax)
	}
	maxCount := int(binary.BigEndian.Uint16(p[n:]))
	off, ok := parseCont(p[n+2:])
	if !ok {
		return errRsp(tid, errBadContinue)
	}
	recs := s.matching(pat)
	if maxCount < len(recs) {
		recs = recs[:maxCount]
	}
	if off > len(recs) {
		return errRsp(tid, errBadContinue)
	}
	budget := (outMTU - 5 - 4 - 3) / 4
	end := off + budget
	next := end
	if end >= len(recs) {
		end, next = len(recs), -1
	}
	out := binary.BigEndian.AppendUint16(nil, uint16(len(recs)))
	out = binary.BigEndian.AppendUint16(out, uint16(end-off))
	for _, r := range recs[off:end] {
		out = binary.BigEndian.AppendUint32(out, r.handle)
	}
	out = append(out, contBytes(next)...)
	return pdu(pduSearchRsp, tid, out)
}

// chunk slices data for a response honouring maxBytes and the MTU.
func chunk(data []byte, off, maxBytes, outMTU int) (part []byte, next int) {
	n := outMTU - 5 - 2 - 3
	if maxBytes < n {
		n = maxBytes
	}
	if n > len(data)-off {
		n = len(data) - off
	}
	if off+n >= len(data) {
		return data[off:], -1
	}
	return data[off : off+n], off + n
}

func attrResponse(rspID byte, tid uint16, data []byte, off, maxBytes, outMTU int) []byte {
	if off > len(data) {
		return errRsp(tid, errBadContinue)
	}
	part, next := chunk(data, off, maxBytes, outMTU)
	out := binary.BigEndian.AppendUint16(nil, uint16(len(part)))
	out = append(out, part...)
	out = append(out, contBytes(next)...)
	return pdu(rspID, tid, out)
}

func (s *Server) serviceAttr(tid uint16, p []byte, outMTU int) []byte {
	if len(p) < 6 {
		return errRsp(tid, errBadSyntax)
	}
	handle := binary.BigEndian.Uint32(p)
	maxBytes := int(binary.BigEndian.Uint16(p[4:]))
	rs, n, ok := parseAttrList(p[6:])
	if !ok || maxBytes < 7 {
		return errRsp(tid, errBadSyntax)
	}
	off, ok := parseCont(p[6+n:])
	if !ok {
		return errRsp(tid, errBadContinue)
	}
	var rec *record
	for _, r := range s.records {
		if r.handle == handle {
			rec = r
		}
	}
	if rec == nil {
		return errRsp(tid, errBadHandle)
	}
	return attrResponse(pduAttrRsp, tid, rec.attrList(rs).encode(), off, maxBytes, outMTU)
}

func (s *Server) serviceSearchAttr(tid uint16, p []byte, outMTU int) []byte {
	pat, n, ok := parsePattern(p)
	if !ok || len(p) < n+2 {
		return errRsp(tid, errBadSyntax)
	}
	maxBytes := int(binary.BigEndian.Uint16(p[n:]))
	rs, m, ok := parseAttrList(p[n+2:])
	if !ok || maxBytes < 7 {
		return errRsp(tid, errBadSyntax)
	}
	off, ok := parseCont(p[n+2+m:])
	if !ok {
		return errRsp(tid, errBadContinue)
	}
	var lists []elem
	for _, r := range s.matching(pat) {
		lists = append(lists, r.attrList(rs))
	}
	return attrResponse(pduSearchAttrRsp, tid, seqE(lists...).encode(), off, maxBytes, outMTU)
}
