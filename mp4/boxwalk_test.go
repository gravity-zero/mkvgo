package mp4

import (
	"encoding/binary"
	"fmt"
	"testing"
)

// boxwalk_test.go - a small, self-contained MP4 box reader used only by the
// tests in this package to validate RemuxToMP4 output. It is intentionally
// independent of any production parser so the tests check the writer against a
// second implementation, not against itself.

type tbox struct {
	typ     string
	payload []byte // box body (after size+type, and after largesize if present)
	dataOff int64  // absolute file offset of payload[0]
}

// walkBoxes parses the sequence of boxes in data, whose first byte is at
// absolute offset base. It handles both 32-bit sizes and the 64-bit largesize
// form (size==1).
func walkBoxes(t *testing.T, data []byte, base int64) []tbox {
	t.Helper()
	var out []tbox
	for off := 0; off+8 <= len(data); {
		size := int64(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		hdr := 8
		switch size {
		case 1:
			if off+16 > len(data) {
				t.Fatalf("truncated 64-bit box %q", typ)
			}
			size = int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
			hdr = 16
		case 0:
			size = int64(len(data) - off) // extends to end
		}
		if size < int64(hdr) || off+int(size) > len(data) {
			t.Fatalf("box %q has bad size %d at off %d (len %d)", typ, size, off, len(data))
		}
		out = append(out, tbox{
			typ:     typ,
			payload: data[off+hdr : off+int(size)],
			dataOff: base + int64(off+hdr),
		})
		off += int(size)
	}
	return out
}

func findBox(boxes []tbox, typ string) (tbox, bool) {
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return tbox{}, false
}

func mustBox(t *testing.T, boxes []tbox, typ string) tbox {
	t.Helper()
	b, ok := findBox(boxes, typ)
	if !ok {
		t.Fatalf("box %q not found", typ)
	}
	return b
}

// parsedTrack holds the sample table information extracted from one trak.
type parsedTrack struct {
	handler      string
	sampleEntry  string
	durations    []uint32
	syncSamples  []uint32 // 1-based; nil means "all sync"
	cttsVersion  int      // -1 if no ctts box
	cttsOffsets  []int32
	sampleSizes  []uint32
	chunkOffsets []uint64
	samplesPer   []uint32 // per-chunk sample count, expanded from stsc
}

func walkTrak(t *testing.T, trak tbox) parsedTrack {
	t.Helper()
	var pt parsedTrack
	pt.cttsVersion = -1

	trakBoxes := walkBoxes(t, trak.payload, trak.dataOff)
	mdia := mustBox(t, trakBoxes, "mdia")
	mdiaBoxes := walkBoxes(t, mdia.payload, mdia.dataOff)

	hdlr := mustBox(t, mdiaBoxes, "hdlr")
	// FullBox: version/flags(4) + pre_defined(4) + handler_type(4).
	pt.handler = string(hdlr.payload[8:12])

	minf := mustBox(t, mdiaBoxes, "minf")
	minfBoxes := walkBoxes(t, minf.payload, minf.dataOff)
	stbl := mustBox(t, minfBoxes, "stbl")
	stblBoxes := walkBoxes(t, stbl.payload, stbl.dataOff)

	stsd := mustBox(t, stblBoxes, "stsd")
	// stsd payload: version/flags(4) + entry_count(4) + sample entry box
	entries := walkBoxes(t, stsd.payload[8:], stsd.dataOff+8)
	if len(entries) == 0 {
		t.Fatal("stsd has no sample entry")
	}
	pt.sampleEntry = entries[0].typ

	stts := mustBox(t, stblBoxes, "stts")
	pt.durations = expandSTTS(stts.payload)

	if stss, ok := findBox(stblBoxes, "stss"); ok {
		n := binary.BigEndian.Uint32(stss.payload[4:8])
		for i := uint32(0); i < n; i++ {
			pt.syncSamples = append(pt.syncSamples, binary.BigEndian.Uint32(stss.payload[8+i*4:12+i*4]))
		}
	}

	if ctts, ok := findBox(stblBoxes, "ctts"); ok {
		pt.cttsVersion = int(ctts.payload[0])
		pt.cttsOffsets = expandCTTS(ctts.payload)
	}

	stsz := mustBox(t, stblBoxes, "stsz")
	pt.sampleSizes = parseSTSZ(stsz.payload)

	if stco, ok := findBox(stblBoxes, "stco"); ok {
		n := binary.BigEndian.Uint32(stco.payload[4:8])
		for i := uint32(0); i < n; i++ {
			pt.chunkOffsets = append(pt.chunkOffsets, uint64(binary.BigEndian.Uint32(stco.payload[8+i*4:12+i*4])))
		}
	} else if co64, ok := findBox(stblBoxes, "co64"); ok {
		n := binary.BigEndian.Uint32(co64.payload[4:8])
		for i := uint32(0); i < n; i++ {
			pt.chunkOffsets = append(pt.chunkOffsets, binary.BigEndian.Uint64(co64.payload[8+i*8:16+i*8]))
		}
	} else {
		t.Fatal("no stco/co64 box")
	}

	stsc := mustBox(t, stblBoxes, "stsc")
	pt.samplesPer = expandSTSC(t, stsc.payload, len(pt.chunkOffsets))

	return pt
}

func expandSTTS(payload []byte) []uint32 {
	n := binary.BigEndian.Uint32(payload[4:8])
	var out []uint32
	for i := uint32(0); i < n; i++ {
		count := binary.BigEndian.Uint32(payload[8+i*8 : 12+i*8])
		delta := binary.BigEndian.Uint32(payload[12+i*8 : 16+i*8])
		for j := uint32(0); j < count; j++ {
			out = append(out, delta)
		}
	}
	return out
}

func expandCTTS(payload []byte) []int32 {
	n := binary.BigEndian.Uint32(payload[4:8])
	var out []int32
	for i := uint32(0); i < n; i++ {
		count := binary.BigEndian.Uint32(payload[8+i*8 : 12+i*8])
		off := int32(binary.BigEndian.Uint32(payload[12+i*8 : 16+i*8]))
		for j := uint32(0); j < count; j++ {
			out = append(out, off)
		}
	}
	return out
}

func parseSTSZ(payload []byte) []uint32 {
	sampleSize := binary.BigEndian.Uint32(payload[4:8])
	count := binary.BigEndian.Uint32(payload[8:12])
	out := make([]uint32, count)
	if sampleSize != 0 {
		for i := range out {
			out[i] = sampleSize
		}
		return out
	}
	for i := uint32(0); i < count; i++ {
		out[i] = binary.BigEndian.Uint32(payload[12+i*4 : 16+i*4])
	}
	return out
}

// expandSTSC expands the run-length sample-to-chunk table into a per-chunk
// samples-per-chunk slice for nChunks chunks.
func expandSTSC(t *testing.T, payload []byte, nChunks int) []uint32 {
	t.Helper()
	n := binary.BigEndian.Uint32(payload[4:8])
	type entry struct{ first, spc uint32 }
	var entries []entry
	for i := uint32(0); i < n; i++ {
		base := 8 + i*12
		entries = append(entries, entry{
			first: binary.BigEndian.Uint32(payload[base : base+4]),
			spc:   binary.BigEndian.Uint32(payload[base+4 : base+8]),
		})
	}
	out := make([]uint32, nChunks)
	for c := 1; c <= nChunks; c++ {
		spc := uint32(0)
		for _, e := range entries {
			if uint32(c) >= e.first {
				spc = e.spc
			}
		}
		out[c-1] = spc
	}
	return out
}

// extractSamples returns each sample's bytes (in decode order) read from the
// full file using the track's chunk offsets, per-chunk counts and sample sizes.
func extractSamples(t *testing.T, file []byte, pt parsedTrack) [][]byte {
	t.Helper()
	var samples [][]byte
	si := 0
	for c, off := range pt.chunkOffsets {
		pos := off
		for k := uint32(0); k < pt.samplesPer[c]; k++ {
			if si >= len(pt.sampleSizes) {
				t.Fatalf("stsc references more samples than stsz lists")
			}
			size := uint64(pt.sampleSizes[si])
			end := pos + size
			if end > uint64(len(file)) {
				t.Fatalf("sample %d range [%d:%d] out of file (len %d)", si, pos, end, len(file))
			}
			samples = append(samples, file[pos:end])
			pos = end
			si++
		}
	}
	if si != len(pt.sampleSizes) {
		t.Fatalf("extracted %d samples, stsz lists %d", si, len(pt.sampleSizes))
	}
	return samples
}

func (pt parsedTrack) String() string {
	return fmt.Sprintf("track{entry=%s handler=%s samples=%d chunks=%d cttsVer=%d}",
		pt.sampleEntry, pt.handler, len(pt.sampleSizes), len(pt.chunkOffsets), pt.cttsVersion)
}
