package mp4

import "encoding/binary"

// tablecodec.go - a window table keeps each rendition's samples packed: the
// fields a streamed segment needs (size, sync, decode time, duration,
// composition offset, file offset) as varints, times and offsets as deltas,
// a few bytes a sample instead of a fragSample and an offset. Open unpacks
// the one rendition it serves; the segment head is kept as built.

// packTrack packs seg's samples and their file offsets.
func packTrack(seg trackSegment, offs []int64) []byte {
	b := make([]byte, 0, 16+len(seg.samples)*10)
	b = binary.AppendUvarint(b, uint64(seg.trackID))
	b = binary.AppendVarint(b, seg.baseDecodeTS)
	hasCTS := uint64(0)
	if seg.hasCTS {
		hasCTS = 1
	}
	b = binary.AppendUvarint(b, hasCTS)
	b = binary.AppendUvarint(b, uint64(seg.dataLen))
	b = binary.AppendUvarint(b, uint64(len(seg.samples)))
	prevDts, prevOff := seg.baseDecodeTS, int64(0)
	for i, s := range seg.samples {
		sync := uint64(0)
		if s.sync {
			sync = 1
		}
		b = binary.AppendUvarint(b, uint64(s.size)<<1|sync)
		b = binary.AppendVarint(b, s.dtsTS-prevDts)
		b = binary.AppendVarint(b, s.durTS)
		b = binary.AppendVarint(b, int64(s.ctsTS))
		b = binary.AppendVarint(b, offs[i]-prevOff)
		prevDts, prevOff = s.dtsTS, offs[i]
	}
	return b
}

// unpackTrack is packTrack's inverse (the samples' millisecond times, used
// only while timing a window, are not kept).
func unpackTrack(b []byte) (trackSegment, []int64) {
	u := func() uint64 {
		v, n := binary.Uvarint(b)
		b = b[n:]
		return v
	}
	s := func() int64 {
		v, n := binary.Varint(b)
		b = b[n:]
		return v
	}
	seg := trackSegment{trackID: uint32(u()), baseDecodeTS: s()}
	seg.hasCTS = u() == 1
	seg.dataLen = int64(u())
	count := int(u())
	if count == 0 {
		return seg, nil
	}
	seg.samples = make([]fragSample, count)
	offs := make([]int64, count)
	dts, off := seg.baseDecodeTS, int64(0)
	for i := range seg.samples {
		v := u()
		dts += s()
		seg.samples[i] = fragSample{size: uint32(v >> 1), sync: v&1 == 1, dtsTS: dts, durTS: s(), ctsTS: int32(s())}
		off += s()
		offs[i] = off
	}
	return seg, offs
}

// view is the table as Open serves rendition ti from it: that rendition's
// samples unpacked next to its kept segment head. A table built for one
// request (an MP4 source's) is its own view.
func (t *windowTable) view(ti int) *windowTable {
	if t.packed == nil {
		return t
	}
	v := &windowTable{segs: make([]trackSegment, len(t.packed)), heads: t.heads, offs: make([][]int64, len(t.packed)), data: t.data}
	if t.packed[ti] != nil {
		v.segs[ti], v.offs[ti] = unpackTrack(t.packed[ti])
	}
	return v
}
