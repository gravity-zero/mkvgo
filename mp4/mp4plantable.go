package mp4

// mp4plantable.go - what an MP4-source plan keeps per sample once it is built.
// The construction works on the full sample arrays (fragSample plus a 64-bit
// file offset each: 64 bytes a sample), but a plan sits in a server cache for
// as long as a viewer might come back, and a long 2160p source carries a few
// hundred thousand samples. The plan therefore keeps this table instead: each
// segment's samples packed into a few bytes apiece - a value every sample of
// the segment shares is written once, the others as varints, the file offsets
// as the gaps between samples that are not stored back to back - and window()
// unpacks one segment when it is asked for. The packing is lossless: a window
// comes back with the very values the full arrays held.

import (
	"encoding/binary"
	"slices"
)

// mp4PlanTable is one track's packed sample table.
type mp4PlanTable struct {
	// blob holds the segments one after another; segment n is
	// blob[segPos[n]:segPos[n+1]]. An empty window is an empty slice.
	//
	//	uvarint  sample count
	//	sizes    packed column (uvarint)
	//	durTS    packed column (uvarint)
	//	ctsTS    packed column (zigzag varint)
	//	sync     1 byte: syncFirst, syncAll, syncNone, or syncBits + a bitset
	//	uvarint  file offset of the first sample
	//	uvarint  number of breaks, then per break: uvarint samples since the
	//	         previous break (or the window start), zigzag varint distance
	//	         from where the sample would sit had it followed the previous one
	//
	// A packed column is colConst + one value when every sample shares it;
	// colDict1/2/4 + a dictionary (uvarint length, the values) + one 1-, 2- or
	// 4-bit dictionary index per sample when the window holds at most 2, 4 or
	// 16 distinct values and that is shorter (durations and composition
	// offsets cycle through a handful of values); else colList + one value per
	// sample.
	blob   []byte
	segPos []uint32
	segDTS []int64 // per segment: its first sample's decode time
}

const (
	colConst = 0
	colList  = 1
	colDict1 = 2
	colDict2 = 3
	colDict4 = 4

	syncFirst = 0 // the first sample only - a keyframe-cut video segment
	syncAll   = 1 // every sample - audio
	syncNone  = 2
	syncBits  = 3 // anything else: one bit per sample follows
)

// newMP4PlanTable packs a track's timed samples and their file offsets;
// segStart is the index of each segment's first sample (a segment ends where
// the next one starts, the last one at the end of the track).
func newMP4PlanTable(samples []fragSample, offs []int64, segStart []int32) *mp4PlanTable {
	t := &mp4PlanTable{
		segPos: make([]uint32, 0, len(segStart)+1),
		segDTS: make([]int64, len(segStart)),
	}
	for k, first := range segStart {
		start, end := int(first), len(samples)
		if k+1 < len(segStart) {
			end = int(segStart[k+1])
		}
		t.segPos = append(t.segPos, uint32(len(t.blob)))
		if end <= start {
			continue
		}
		t.segDTS[k] = samples[start].dtsTS
		t.blob = packWindow(t.blob, samples[start:end], offs[start:end])
	}
	t.segPos = append(t.segPos, uint32(len(t.blob)))
	t.blob = slices.Clone(t.blob) // exact size: the append growth slack would stay held with the plan
	return t
}

func packWindow(b []byte, samples []fragSample, offs []int64) []byte {
	n := len(samples)
	b = binary.AppendUvarint(b, uint64(n))
	b = packColumn(b, n, func(i int) uint64 { return uint64(samples[i].size) })
	b = packColumn(b, n, func(i int) uint64 { return uint64(samples[i].durTS) })
	b = packColumn(b, n, func(i int) uint64 { return zigzag(int64(samples[i].ctsTS)) })

	synced := 0
	for i := range samples {
		if samples[i].sync {
			synced++
		}
	}
	switch {
	case synced == n:
		b = append(b, syncAll)
	case synced == 0:
		b = append(b, syncNone)
	case synced == 1 && samples[0].sync:
		b = append(b, syncFirst)
	default:
		b = append(b, syncBits)
		bits := make([]byte, (n+7)/8)
		for i := range samples {
			if samples[i].sync {
				bits[i/8] |= 1 << (i % 8)
			}
		}
		b = append(b, bits...)
	}

	b = binary.AppendUvarint(b, uint64(offs[0]))
	breaks := 0
	for i := 1; i < n; i++ {
		if offs[i] != offs[i-1]+int64(samples[i-1].size) {
			breaks++
		}
	}
	b = binary.AppendUvarint(b, uint64(breaks))
	last := 0
	for i := 1; i < n; i++ {
		next := offs[i-1] + int64(samples[i-1].size)
		if offs[i] == next {
			continue
		}
		b = binary.AppendUvarint(b, uint64(i-last))
		b = binary.AppendUvarint(b, zigzag(offs[i]-next))
		last = i
	}
	return b
}

func packColumn(b []byte, n int, at func(int) uint64) []byte {
	// The distinct values in order of first appearance, up to the largest
	// dictionary; listLen is what the plain list would cost.
	var dict []uint64
	listLen := 0
	for i := 0; i < n; i++ {
		v := at(i)
		listLen += uvarintLen(v)
		if len(dict) <= 16 && !slices.Contains(dict, v) {
			dict = append(dict, v)
		}
	}
	if len(dict) == 1 {
		b = append(b, colConst)
		return binary.AppendUvarint(b, dict[0])
	}
	mode, bits := byte(colDict4), 4
	switch {
	case len(dict) <= 2:
		mode, bits = colDict1, 1
	case len(dict) <= 4:
		mode, bits = colDict2, 2
	}
	dictLen := 1 + (n*bits+7)/8
	for _, v := range dict {
		dictLen += uvarintLen(v)
	}
	if len(dict) > 16 || dictLen >= listLen {
		b = append(b, colList)
		for i := 0; i < n; i++ {
			b = binary.AppendUvarint(b, at(i))
		}
		return b
	}
	b = append(b, mode)
	b = binary.AppendUvarint(b, uint64(len(dict)))
	for _, v := range dict {
		b = binary.AppendUvarint(b, v)
	}
	idx := make([]byte, (n*bits+7)/8)
	for i := 0; i < n; i++ {
		idx[i*bits/8] |= byte(slices.Index(dict, at(i))) << (i * bits % 8)
	}
	return append(b, idx...)
}

func uvarintLen(v uint64) int {
	n := 1
	for ; v >= 0x80; v >>= 7 {
		n++
	}
	return n
}

func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// planReader walks a packed window. The bytes are the plan's own, written by
// packWindow, so there is no malformed input to refuse.
type planReader struct{ b []byte }

func (r *planReader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	r.b = r.b[n:]
	return v
}

func (r *planReader) byte() byte {
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

// column calls set with each of the n values of a packed column.
func (r *planReader) column(n int, set func(i int, v uint64)) {
	bits := 0
	switch r.byte() {
	case colConst:
		v := r.uvarint()
		for i := 0; i < n; i++ {
			set(i, v)
		}
		return
	case colList:
		for i := 0; i < n; i++ {
			set(i, r.uvarint())
		}
		return
	case colDict1:
		bits = 1
	case colDict2:
		bits = 2
	case colDict4:
		bits = 4
	}
	dict := make([]uint64, r.uvarint())
	for i := range dict {
		dict[i] = r.uvarint()
	}
	for i := 0; i < n; i++ {
		set(i, dict[r.b[i*bits/8]>>(i*bits%8)&(1<<bits-1)])
	}
	r.b = r.b[(n*bits+7)/8:]
}

// window returns segment n's samples, timed exactly as the full arrays were
// (ptsMs/blockPtsMs are not carried: the window is already cut), and each
// sample's file offset.
func (t *mp4PlanTable) window(n int) (samples []fragSample, offs []int64) {
	r := planReader{b: t.blob[t.segPos[n]:t.segPos[n+1]]}
	if len(r.b) == 0 {
		return nil, nil
	}
	count := int(r.uvarint())
	samples = make([]fragSample, count)
	offs = make([]int64, count)
	r.column(count, func(i int, v uint64) { samples[i].size = uint32(v) })
	r.column(count, func(i int, v uint64) { samples[i].durTS = int64(v) })
	r.column(count, func(i int, v uint64) { samples[i].ctsTS = int32(unzigzag(v)) })

	switch r.byte() {
	case syncAll:
		for i := range samples {
			samples[i].sync = true
		}
	case syncFirst:
		samples[0].sync = true
	case syncBits:
		for i := range samples {
			samples[i].sync = r.b[i/8]&(1<<(i%8)) != 0
		}
		r.b = r.b[(count+7)/8:]
	}

	off := int64(r.uvarint())
	breaks := int(r.uvarint())
	nextBreak := -1
	var gap int64
	if breaks > 0 {
		nextBreak, gap = int(r.uvarint()), unzigzag(r.uvarint())
		breaks--
	}
	dts := t.segDTS[n]
	for i := range samples {
		if i == nextBreak {
			off += gap
			nextBreak = -1
			if breaks > 0 {
				nextBreak, gap = i+int(r.uvarint()), unzigzag(r.uvarint())
				breaks--
			}
		}
		offs[i] = off
		off += int64(samples[i].size)
		samples[i].dtsTS = dts
		dts += samples[i].durTS
	}
	return samples, offs
}
