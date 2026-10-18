package mp4

import (
	"encoding/binary"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

// maxMfraBytes bounds the random-access index read; a real mfra is tens of KB
// even for a long movie, so anything larger is treated as malformed.
const maxMfraBytes = 16 << 20

// readFragmentKeyframes recovers a fragmented MP4's video keyframe times from its
// Movie Fragment Random Access index: the mfro box at the very end of the file
// gives the mfra size, the mfra holds one tfra per track mapping presentation
// times to random-access points. Head-only and bounded (one tail read), like the
// Matroska Cues back-scan. Anything missing or out of range leaves keyframes nil.
func readFragmentKeyframes(r io.ReadSeeker, size int64, mv *movie) {
	if size < 16 {
		return
	}
	var tail [16]byte
	if _, err := r.Seek(size-16, io.SeekStart); err != nil {
		return
	}
	if _, err := io.ReadFull(r, tail[:]); err != nil || string(tail[4:8]) != "mfro" {
		return // no random-access index
	}
	mfraSize := int64(binary.BigEndian.Uint32(tail[12:16]))
	if mfraSize < 8 || mfraSize > size || mfraSize > maxMfraBytes {
		return
	}
	buf := make([]byte, mfraSize)
	if _, err := r.Seek(size-mfraSize, io.SeekStart); err != nil {
		return
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return
	}
	boxes, err := iterBoxes(buf)
	if err != nil || len(boxes) == 0 || boxes[0].typ != "mfra" {
		return
	}
	inner, err := iterBoxes(boxes[0].payload)
	if err != nil {
		return
	}
	for _, b := range inner {
		if b.typ != "tfra" {
			continue
		}
		tid, times, ok := parseTfra(b.payload)
		if !ok {
			continue
		}
		t := trackByID(mv, tid)
		if t == nil || t.trackType != mkv.VideoTrack {
			continue
		}
		ms := make([]int64, 0, len(times))
		for _, tk := range times {
			v := ticksToMs(int64(tk), t.timescale) + t.editShiftMs
			if v < 0 {
				v = 0
			}
			ms = append(ms, v)
		}
		t.keyframesMs = sortDedupTimes(ms)
	}
}

func trackByID(mv *movie, id uint32) *inTrack {
	for i := range mv.tracks {
		if mv.tracks[i].trackID == id {
			return &mv.tracks[i]
		}
	}
	return nil
}

// parseTfra reads a Track Fragment Random Access box, returning its track_ID and
// the presentation time (media timescale) of every random-access point.
func parseTfra(payload []byte) (trackID uint32, times []uint64, ok bool) {
	if len(payload) < 16 {
		return 0, nil, false
	}
	version := payload[0]
	trackID = binary.BigEndian.Uint32(payload[4:8])
	// payload[8:12] = reserved(26) + length_size_of_{traf,trun,sample}_num (2 each);
	// the field byte counts are those 2-bit values + 1.
	lens := payload[11]
	trafSize := int((lens>>4)&0x3) + 1
	trunSize := int((lens>>2)&0x3) + 1
	sampleSize := int(lens&0x3) + 1
	n := binary.BigEndian.Uint32(payload[12:16])
	timeSize := 4
	if version == 1 {
		timeSize = 8
	}
	entrySize := timeSize + timeSize + trafSize + trunSize + sampleSize // time + moof_offset + 3 ids
	off := 16
	for i := uint32(0); i < n; i++ {
		if off+entrySize > len(payload) {
			break
		}
		var t uint64
		if version == 1 {
			t = binary.BigEndian.Uint64(payload[off : off+8])
		} else {
			t = uint64(binary.BigEndian.Uint32(payload[off : off+4]))
		}
		times = append(times, t)
		off += entrySize
	}
	return trackID, times, true
}

// readSidxKeyframes recovers a fragmented MP4's video keyframes from a Segment
// Index (sidx), which precedes the fragments and lists each subsegment's duration
// and whether it starts on a Stream Access Point. It is the streaming-fMP4
// keyframe source when there is no mfra. Head-only and bounded (the sidx boxes
// before the first moof); closed-GOP SAPs only (SAP_type <= 2), for a clean
// seek/segment index. Leaves keyframes nil when no matching sidx is present.
func readSidxKeyframes(r io.ReadSeeker, size int64, mv *movie) {
	vt := videoTrack(mv)
	if vt == nil {
		return
	}
	walkSidxBoxes(r, size, func(sidx []byte) bool {
		times, ok := sidxKeyframeMs(sidx, vt.trackID)
		if !ok {
			return false
		}
		ms := make([]int64, len(times))
		for i, t := range times {
			if v := t + vt.editShiftMs; v > 0 {
				ms[i] = v
			}
		}
		vt.keyframesMs = sortDedupTimes(ms)
		return true
	})
}

// maxSidxBytes is the largest sidx payload ISO/IEC 14496-12 allows: a
// version-1 header and 65535 references (reference_count is 16 bits) of 12 bytes.
const maxSidxBytes = 32 + 65535*12

// scanSidxBoxes returns the payloads of the sidx boxes that precede the first
// fragment (moof) - a bounded head scan of the top-level boxes.
func scanSidxBoxes(r io.ReadSeeker, size int64) [][]byte {
	var out [][]byte
	walkSidxBoxes(r, size, func(p []byte) bool { out = append(out, p); return false })
	return out
}

// walkSidxBoxes hands visit the payload of each sidx box before the first fragment, one at a time, until visit
// returns true: only the sidx in hand is held, however many a file stacks ahead of its fragments.
func walkSidxBoxes(r io.ReadSeeker, size int64, visit func([]byte) bool) {
	var hdr [16]byte
	for off, n := int64(0), 0; off+8 <= size && n < maxTopLevelBoxes; n++ {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			return
		}
		switch typ {
		case "sidx":
			if boxSize-headerLen > maxSidxBytes {
				break // past what a sidx can hold: not one, and never read whole
			}
			payload, err := readExact(r, boxSize-headerLen)
			if err != nil || visit(payload) {
				return
			}
		case "moof", "mdat":
			return // fragments started; no head sidx beyond here
		}
		off += boxSize
	}
}

// sidxKeyframeMs reads a sidx box payload and returns the start times (ms) of the
// subsegments that begin with a closed-GOP Stream Access Point. ok is false when
// the box does not reference wantRefID or is malformed.
func sidxKeyframeMs(payload []byte, wantRefID uint32) ([]int64, bool) {
	if len(payload) < 12 || binary.BigEndian.Uint32(payload[4:8]) != wantRefID {
		return nil, false
	}
	version := payload[0]
	timescale := binary.BigEndian.Uint32(payload[8:12])
	if timescale == 0 {
		return nil, false
	}
	off := 12
	var t uint64 // earliest_presentation_time, then the running subsegment start
	if version == 0 {
		if len(payload) < off+8 {
			return nil, false
		}
		t = uint64(binary.BigEndian.Uint32(payload[off : off+4]))
		off += 8 // earliest_presentation_time(4) + first_offset(4)
	} else {
		if len(payload) < off+16 {
			return nil, false
		}
		t = binary.BigEndian.Uint64(payload[off : off+8])
		off += 16 // earliest_presentation_time(8) + first_offset(8)
	}
	if len(payload) < off+4 {
		return nil, false
	}
	count := int(binary.BigEndian.Uint16(payload[off+2 : off+4])) // after 2 reserved bytes
	off += 4
	var times []int64
	for i := 0; i < count; i++ {
		if off+12 > len(payload) {
			break
		}
		subDur := uint64(binary.BigEndian.Uint32(payload[off+4 : off+8]))
		w := binary.BigEndian.Uint32(payload[off+8 : off+12])
		if w>>31 == 1 && (w>>28)&0x7 <= 2 { // starts_with_SAP, closed-GOP SAP_type
			sapDelta := uint64(w & 0x0FFFFFFF)
			times = append(times, ticksToMs(int64(t+sapDelta), timescale))
		}
		t += subDur
		off += 12
	}
	return times, true
}
