// Git wire protocol: pkt-line codec, packfile read/write, delta application,
// and fetch object selection (including shallow/deepen semantics).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	var out []byte
	out = append(out, []byte(hex4(n))...)
	out = append(out, payload...)
	return out
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

func hex4(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(n>>12)&0xf], digits[(n>>8)&0xf], digits[(n>>4)&0xf], digits[n&0xf]})
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktKind: "data", "flush", "delim", "resp-end", "eof", "bad"
type pktReader struct {
	data []byte
	pos  int
}

func (p *pktReader) next() (kind string, payload []byte) {
	if p.pos+4 > len(p.data) {
		return "eof", nil
	}
	hdr := string(p.data[p.pos : p.pos+4])
	n, err := hexToInt(hdr)
	if err != nil || n < 0 {
		return "bad", nil
	}
	switch n {
	case 0:
		p.pos += 4
		return "flush", nil
	case 1:
		p.pos += 4
		return "delim", nil
	case 2:
		p.pos += 4
		return "resp-end", nil
	}
	if n < 4 || p.pos+n > len(p.data) {
		return "bad", nil
	}
	payload = p.data[p.pos+4 : p.pos+n]
	p.pos += n
	return "data", payload
}

func hexToInt(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, errBadHex
		}
		n = n*16 + v
	}
	return n, nil
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

const errBadHex = simpleError("bad hex")

// ---------------------------------------------------------------------------
// Packfile write (plain objects only, no deltas)
// ---------------------------------------------------------------------------

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
	objOfsDel = 6
	objRefDel = 7
)

func objTypeCode(typ string) int {
	switch typ {
	case "commit":
		return objCommit
	case "tree":
		return objTree
	case "blob":
		return objBlob
	case "tag":
		return objTag
	}
	return 0
}

func appendObjHeader(out []byte, typ int, size int) []byte {
	b := byte((size & 0x0f) | (typ << 4))
	size >>= 4
	for size != 0 {
		out = append(out, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	return append(out, b)
}

// writePackFromRepo builds a deterministic pack for the given oids.
func writePackFromRepo(r *repo, oids []string) []byte {
	sorted := append([]string(nil), oids...)
	sort.Strings(sorted)
	// Keep only objects that exist, so the header count is always exact.
	present := sorted[:0]
	for _, oid := range sorted {
		if r != nil && r.objects[oid] != nil {
			present = append(present, oid)
		}
	}
	var buf bytes.Buffer
	buf.WriteString("PACK")
	buf.Write([]byte{0, 0, 0, 2})
	n := len(present)
	buf.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	for _, oid := range present {
		o := r.objects[oid]
		buf.Write(appendObjHeader(nil, objTypeCode(o.typ), len(o.data)))
		zw, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
		zw.Write(o.data)
		zw.Close()
	}
	h := sha1.Sum(buf.Bytes())
	buf.Write(h[:])
	return buf.Bytes()
}

func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	h := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(h[:], pack[len(pack)-20:])
}

// ---------------------------------------------------------------------------
// Packfile read
// ---------------------------------------------------------------------------

type packEntry struct {
	typ      int
	data     []byte
	baseOID  string // for ref-delta
	baseOff  int    // for ofs-delta: entry start offset of the base
	delta    bool
	deltaRaw []byte
	offset   int
}

// readPack parses a pack, resolving deltas against pack entries and the repo.
// Returns objects keyed by oid, the delta count, and an error.
func readPack(r *repo, pack []byte) (map[string]*obj, int, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, 0, simpleError("bad pack header")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, 0, simpleError("bad pack version")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	pos := 12
	entries := make([]*packEntry, 0, count)
	deltaCount := 0
	for i := 0; i < count; i++ {
		start := pos
		typ, size, npos, err := readObjHeader(pack, pos)
		if err != nil {
			return nil, 0, err
		}
		pos = npos
		e := &packEntry{typ: typ, offset: start}
		switch typ {
		case objOfsDel, objRefDel:
			deltaCount++
			e.delta = true
			if typ == objOfsDel {
				off, np, err := readOfsDeltaOffset(pack, pos)
				if err != nil {
					return nil, 0, err
				}
				pos = np
				e.baseOff = start - off
			} else {
				if pos+20 > len(pack) {
					return nil, 0, simpleError("short ref-delta")
				}
				e.baseOID = hex.EncodeToString(pack[pos : pos+20])
				pos += 20
			}
			raw, consumed, err := inflateAt(pack, pos)
			if err != nil {
				return nil, 0, err
			}
			e.deltaRaw = raw
			pos += consumed
			_ = size
		default:
			raw, consumed, err := inflateAt(pack, pos)
			if err != nil {
				return nil, 0, err
			}
			if len(raw) != size {
				return nil, 0, simpleError("object size mismatch")
			}
			e.data = raw
			pos += consumed
		}
		entries = append(entries, e)
	}
	// Resolve deltas by fixpoint: a base may come from the pack or the repo.
	byOffset := map[int]*packEntry{}
	for _, e := range entries {
		byOffset[e.offset] = e
	}
	out := map[string]*obj{}
	// First pass: plain objects.
	for _, e := range entries {
		if !e.delta {
			typName := typeName(e.typ)
			oid := hashObject(typName, e.data)
			out[oid] = &obj{typ: typName, data: e.data}
		}
	}
	// Delta resolution loop.
	for round := 0; round < len(entries)+2; round++ {
		progress := false
		for _, e := range entries {
			if !e.delta || e.data != nil {
				continue
			}
			var base *obj
			if e.typ == objOfsDel {
				if be, ok := byOffset[e.baseOff]; ok && be.data != nil {
					base = &obj{typ: typeName(be.typ), data: be.data}
				}
			} else {
				if bo, ok := out[e.baseOID]; ok {
					base = bo
				} else if ro := r.getObject(e.baseOID); ro != nil {
					base = ro
				}
			}
			if base == nil {
				continue
			}
			data, err := applyDelta(base.data, e.deltaRaw)
			if err != nil {
				return nil, 0, err
			}
			e.data = data
			oid := hashObject(base.typ, data)
			out[oid] = &obj{typ: base.typ, data: data}
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, e := range entries {
		if e.delta && e.data == nil {
			return nil, 0, simpleError("unresolved delta base")
		}
	}
	return out, deltaCount, nil
}

func typeName(code int) string {
	switch code {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "unknown"
}

func readObjHeader(pack []byte, pos int) (typ, size, npos int, err error) {
	if pos >= len(pack) {
		return 0, 0, 0, simpleError("short object header")
	}
	b := pack[pos]
	pos++
	typ = int((b >> 4) & 0x7)
	size = int(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if pos >= len(pack) {
			return 0, 0, 0, simpleError("short object header")
		}
		b = pack[pos]
		pos++
		size |= int(b&0x7f) << shift
		shift += 7
	}
	return typ, size, pos, nil
}

func readOfsDeltaOffset(pack []byte, pos int) (int, int, error) {
	if pos >= len(pack) {
		return 0, 0, simpleError("short ofs-delta offset")
	}
	b := pack[pos]
	pos++
	off := int(b & 0x7f)
	for b&0x80 != 0 {
		if pos >= len(pack) {
			return 0, 0, simpleError("short ofs-delta offset")
		}
		b = pack[pos]
		pos++
		off = ((off + 1) << 7) | int(b&0x7f)
	}
	return off, pos, nil
}

// inflateAt inflates one zlib stream starting at pos, returning the data and
// the number of pack bytes consumed.
func inflateAt(pack []byte, pos int) ([]byte, int, error) {
	br := bytes.NewReader(pack[pos:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(pack[pos:]) - br.Len()
	return data, consumed, nil
}

// applyDelta applies a git delta to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, simpleError("short delta varint")
			}
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, simpleError("delta base size mismatch")
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, simpleError("short delta copy")
					}
					off |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, simpleError("short delta copy")
					}
					size |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, simpleError("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			n := int(op)
			if pos+n > len(delta) {
				return nil, simpleError("short delta insert")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, simpleError("delta op 0")
		}
	}
	if len(out) != dstSize {
		return nil, simpleError("delta result size mismatch")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fetch planning
// ---------------------------------------------------------------------------

type fetchRequest struct {
	wants       []string
	haves       []string
	shallow     []string
	depth       int // -1 = none
	deepenSince int64
	deepenNot   []string
}

type fetchPlan struct {
	sendCommits []string // commit oids to send, sorted
	shallowOut  []string
	unshallow   []string
	ack         string
	packOIDs    []string // every object oid for the pack, sorted
}

// walkStop walks commits from tips, including boundary commits but not
// expanding past them.
func (r *repo) walkStop(tips []string, stopAt map[string]bool) map[string]bool {
	out := map[string]bool{}
	queue := append([]string(nil), tips...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || cur == zeroOID || out[cur] {
			continue
		}
		o := r.objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		out[cur] = true
		if stopAt[cur] {
			continue
		}
		_, parents, _, _ := parseCommit(o.data)
		queue = append(queue, parents...)
	}
	return out
}

func (r *repo) planFetch(req fetchRequest) fetchPlan {
	oldShallow := map[string]bool{}
	for _, s := range req.shallow {
		oldShallow[s] = true
	}
	// Included commits.
	included := map[string]int{}
	if req.depth > 0 {
		included = r.commitsWithin(req.wants, req.depth-1)
	} else if req.deepenSince > 0 {
		all := r.commitsWithin(req.wants, -1)
		for oid := range all {
			if o := r.objects[oid]; o != nil && o.typ == "commit" {
				_, _, _, ts := parseCommit(o.data)
				if ts > req.deepenSince {
					included[oid] = all[oid]
				}
			}
		}
	} else {
		included = r.commitsWithin(req.wants, -1)
	}
	// deepen-not excludes commits reachable from those refs.
	if len(req.deepenNot) > 0 {
		var notTips []string
		for _, ref := range req.deepenNot {
			if oid := r.resolveRef(ref); oid != "" {
				notTips = append(notTips, oid)
			}
		}
		excl := map[string]bool{}
		for _, t := range notTips {
			for c := range r.ancestors(t) {
				excl[c] = true
			}
			excl[t] = true
		}
		for oid := range included {
			if excl[oid] {
				delete(included, oid)
			}
		}
	}
	// Held commits: closure from haves, stopping at old shallow boundaries.
	held := r.walkStop(req.haves, oldShallow)
	send := map[string]bool{}
	for oid := range included {
		if !held[oid] {
			send[oid] = true
		}
	}
	// New shallow boundaries: included commits with a parent not included.
	plan := fetchPlan{}
	if req.depth > 0 || req.deepenSince > 0 {
		for oid := range included {
			o := r.objects[oid]
			if o == nil || o.typ != "commit" {
				continue
			}
			_, parents, _, _ := parseCommit(o.data)
			if len(parents) == 0 {
				continue
			}
			anyIncluded := false
			for _, p := range parents {
				if _, ok := included[p]; ok {
					anyIncluded = true
					break
				}
			}
			if !anyIncluded {
				plan.shallowOut = append(plan.shallowOut, oid)
			}
		}
		sort.Strings(plan.shallowOut)
	}
	// Unshallow: old boundaries now included and not still boundaries.
	newShallowSet := map[string]bool{}
	for _, s := range plan.shallowOut {
		newShallowSet[s] = true
	}
	for s := range oldShallow {
		if _, ok := included[s]; ok && !newShallowSet[s] {
			plan.unshallow = append(plan.unshallow, s)
		}
	}
	sort.Strings(plan.unshallow)
	// ACK: first have the server holds as a commit.
	for _, h := range req.haves {
		if o := r.objects[h]; o != nil && o.typ == "commit" {
			plan.ack = h
			break
		}
	}
	// Objects the client already holds: held commits plus their full tree
	// closure. A pack must never re-send these (fetch-incremental asserts it).
	heldObjects := map[string]bool{}
	for oid := range held {
		heldObjects[oid] = true
		if o := r.objects[oid]; o != nil && o.typ == "commit" {
			r.addTreeClosure(heldObjects, r.commitTree(oid))
		}
	}
	// Collect objects: sent commits + their trees/blobs (+ tag targets).
	packSet := map[string]bool{}
	for oid := range send {
		o := r.objects[oid]
		if o == nil {
			continue
		}
		plan.sendCommits = append(plan.sendCommits, oid)
		if o.typ == "tag" {
			packSet[oid] = true
			// Resolve tag target: "object <oid>" line.
			for _, line := range strings.Split(string(o.data), "\n") {
				if strings.HasPrefix(line, "object ") {
					target := strings.TrimSpace(line[7:])
					if to := r.objects[target]; to != nil && to.typ == "commit" {
						packSet[target] = true
						r.addTreeClosure(packSet, r.commitTree(target))
					}
				}
			}
			continue
		}
		if o.typ != "commit" {
			continue
		}
		packSet[oid] = true
		r.addTreeClosure(packSet, r.commitTree(oid))
	}
	for oid := range heldObjects {
		delete(packSet, oid)
	}
	sort.Strings(plan.sendCommits)
	for oid := range packSet {
		plan.packOIDs = append(plan.packOIDs, oid)
	}
	sort.Strings(plan.packOIDs)
	return plan
}

// packCacheKey builds a cache key for a fetch pack.
func packCacheKey(r *repo, req fetchRequest) string {
	var b strings.Builder
	b.WriteString("v=")
	b.WriteString(itoa(int(r.version)))
	b.WriteString("|w=")
	b.WriteString(strings.Join(req.wants, ","))
	b.WriteString("|h=")
	b.WriteString(strings.Join(req.haves, ","))
	b.WriteString("|s=")
	b.WriteString(strings.Join(req.shallow, ","))
	b.WriteString("|d=")
	b.WriteString(itoa(req.depth))
	b.WriteString("|ds=")
	b.WriteString(itoa(int(req.deepenSince)))
	b.WriteString("|dn=")
	b.WriteString(strings.Join(req.deepenNot, ","))
	return b.String()
}
