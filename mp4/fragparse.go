package mp4

// fragparse.go - fragmented-MP4 / CMAF input. A fragmented file's moov carries
// only track headers and mvex defaults; the samples live in moof/mdat fragments
// (streaming fMP4, DASH/HLS CMAF, the shape most pre-encoded ABR ladders take).
// readFragmentSamples walks those fragments and fills each track's sample table
// (offset/size/dts/cts/duration/sync) exactly as the progressive parser builds
// it from stbl, so every downstream path - RemuxFromMP4, to-mp4, to-hls, ABR  -
// reads a CMAF source like an ordinary progressive one. Media bytes are never
// read here: only the moof metadata (bounded) is parsed and sample offsets are
// recorded for the same ranged reads the progressive path uses.

import (
	"encoding/binary"
	"io"
)

// tfhd flags read below (the writer's tfhdDefaultBaseIsMoof and the trun*
// flags are declared in fragment.go and reused here).
const (
	tfhdBaseDataOffset  = 0x000001
	tfhdSampleDescIndex = 0x000002
	tfhdDefaultDuration = 0x000008
	tfhdDefaultSize     = 0x000010
	tfhdDefaultFlags    = 0x000020
	trunFirstSampleFlag = 0x000004
)

// sampleNonSync is the sample_is_non_sync_sample bit in a sample_flags word.
const sampleNonSync = 0x00010000

// maxFragMoofBytes bounds a single moof read. A moof holds only sample metadata
// (no media), tens to hundreds of KB even for a long fragment; anything larger
// is treated as malformed rather than allocated.
const maxFragMoofBytes = 64 << 20

type trexDefault struct {
	duration uint32
	size     uint32
	flags    uint32
}

// parseTrexDefaults reads the per-track sample defaults from moov>mvex>trex.
func parseTrexDefaults(moovBoxes []memBox) map[uint32]trexDefault {
	out := map[uint32]trexDefault{}
	mvex, ok := findMemBox(moovBoxes, "mvex")
	if !ok {
		return out
	}
	boxes, err := iterBoxes(mvex.payload)
	if err != nil {
		return out
	}
	for _, b := range boxes {
		// version/flags(4) track_ID(4) default_sample_description_index(4)
		// default_sample_duration(4) default_sample_size(4) default_sample_flags(4)
		if b.typ == "trex" && len(b.payload) >= 24 {
			tid := binary.BigEndian.Uint32(b.payload[4:8])
			out[tid] = trexDefault{
				duration: binary.BigEndian.Uint32(b.payload[12:16]),
				size:     binary.BigEndian.Uint32(b.payload[16:20]),
				flags:    binary.BigEndian.Uint32(b.payload[20:24]),
			}
		}
	}
	return out
}

// readFragmentSamples walks the top-level boxes, reads each moof, and appends
// every fragment's samples to the matching track. Malformed fragments abort the
// parse (a remux must not silently drop media). moovBoxes supplies the mvex
// defaults.
func readFragmentSamples(r io.ReadSeeker, size int64, mv *movie, moovBoxes []memBox) error {
	trex := parseTrexDefaults(moovBoxes)
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return err
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			break // truncated tail: stop with the fragments read so far
		}
		if typ == "moof" {
			if boxSize-headerLen > maxFragMoofBytes {
				return errf("moof at offset %d is %d bytes (exceeds limit)", off, boxSize-headerLen)
			}
			payload, err := readExact(r, boxSize-headerLen)
			if err != nil {
				return err
			}
			if err := parseMoofSamples(payload, off, trex, mv); err != nil {
				return err
			}
		}
		off += boxSize
	}
	// The sample tables now stand in for the (absent) stbl; record the counts and
	// per-track duration the progressive path would have derived.
	for i := range mv.tracks {
		t := &mv.tracks[i]
		if n := int64(len(t.samples)); n > 0 {
			t.frameCount = n
			last := t.samples[n-1]
			if end := last.ctsMs + last.durMs; end > mv.durationMs {
				mv.durationMs = end
			}
		}
	}
	return nil
}

// parseMoofSamples parses one Movie Fragment: each traf inside contributes its
// samples, located relative to moofStart (the default-base-is-moof anchor).
func parseMoofSamples(moof []byte, moofStart int64, trex map[uint32]trexDefault, mv *movie) error {
	boxes, err := iterBoxes(moof)
	if err != nil {
		return err
	}
	for _, b := range boxes {
		if b.typ == "traf" {
			if err := parseTrafSamples(b.payload, moofStart, trex, mv); err != nil {
				return err
			}
		}
	}
	return nil
}

// tfhd is a parsed Track Fragment Header: the track the fragment belongs to,
// its data anchor and the per-fragment sample defaults it declares.
type tfhd struct {
	trackID                      uint32
	baseDataOffset               int64
	haveBaseOffset               bool // else the data anchor is the moof start (default-base-is-moof, and the pre-CMAF default alike)
	defDur, defSize, defFlags    uint32
	haveDur, haveSize, haveFlags bool
}

// parseTfhd decodes a tfhd payload.
func parseTfhd(p []byte) (tfhd, error) {
	var h tfhd
	if len(p) < 8 {
		return h, errf("tfhd too short")
	}
	flags := binary.BigEndian.Uint32(p[0:4]) & 0xFFFFFF
	h.trackID = binary.BigEndian.Uint32(p[4:8])
	off := 8
	read4 := func() (uint32, bool) {
		if off+4 > len(p) {
			return 0, false
		}
		v := binary.BigEndian.Uint32(p[off : off+4])
		off += 4
		return v, true
	}
	if flags&tfhdBaseDataOffset != 0 {
		if off+8 > len(p) {
			return h, errf("tfhd truncated base_data_offset")
		}
		h.baseDataOffset = int64(binary.BigEndian.Uint64(p[off : off+8]))
		off += 8
		h.haveBaseOffset = true
	}
	if flags&tfhdSampleDescIndex != 0 {
		read4()
	}
	if flags&tfhdDefaultDuration != 0 {
		h.defDur, h.haveDur = read4()
	}
	if flags&tfhdDefaultSize != 0 {
		h.defSize, h.haveSize = read4()
	}
	if flags&tfhdDefaultFlags != 0 {
		h.defFlags, h.haveFlags = read4()
	}
	return h, nil
}

// parseTfdt returns the fragment's base media decode time (version 0 or 1).
func parseTfdt(p []byte) int64 {
	if len(p) < 8 {
		return 0
	}
	if p[0] == 1 && len(p) >= 12 {
		return int64(binary.BigEndian.Uint64(p[4:12]))
	}
	return int64(binary.BigEndian.Uint32(p[4:8]))
}

// parseTrafSamples reads a Track Fragment (tfhd + tfdt + one or more trun) and
// appends its samples to the track, in the same units the progressive parser
// uses (ms, edit-list shift folded into cts).
func parseTrafSamples(traf []byte, moofStart int64, trex map[uint32]trexDefault, mv *movie) error {
	boxes, err := iterBoxes(traf)
	if err != nil {
		return err
	}

	var (
		h              tfhd
		haveTfhd       bool
		baseDecodeTime int64
	)
	for _, b := range boxes {
		if b.typ != "tfhd" || len(b.payload) < 8 {
			continue
		}
		if h, err = parseTfhd(b.payload); err != nil {
			return err
		}
		haveTfhd = true
	}
	if !haveTfhd {
		return nil // fragment with no tfhd: nothing to place
	}
	trackID := h.trackID
	t := trackByID(mv, trackID)
	if t == nil || t.timescale == 0 {
		return nil // fragment for a track we do not carry
	}

	for _, b := range boxes {
		if b.typ == "tfdt" {
			baseDecodeTime = parseTfdt(b.payload)
		}
	}

	// Defaults the tfhd leaves out come from the init's trex.
	td := trex[trackID]
	if !h.haveDur {
		h.defDur = td.duration
	}
	if !h.haveSize {
		h.defSize = td.size
	}
	if !h.haveFlags {
		h.defFlags = td.flags
	}

	base := moofStart
	if h.haveBaseOffset {
		base = h.baseDataOffset
	}

	dts := baseDecodeTime
	dataCursor := base // where the next trun's data continues when it omits data_offset
	for _, b := range boxes {
		if b.typ != "trun" {
			continue
		}
		sampleOff, newDTS, err := appendTrunSamples(b.payload, t, base, dataCursor, dts, h.defDur, h.defSize, h.defFlags)
		if err != nil {
			return err
		}
		dataCursor = sampleOff
		dts = newDTS
	}
	return nil
}

// appendTrunSamples decodes one trun and appends its samples to the track. It
// returns the byte offset just past the last sample (the continuation point for
// a following trun) and the running decode time.
func appendTrunSamples(p []byte, t *inTrack, base, dataCursor, dts int64, defDur, defSize, defFlags uint32) (int64, int64, error) {
	if len(p) < 8 {
		return dataCursor, dts, errf("trun too short")
	}
	version := p[0]
	flags := binary.BigEndian.Uint32(p[0:4]) & 0xFFFFFF
	count := binary.BigEndian.Uint32(p[4:8])
	off := 8
	need := func(n int) bool { return off+n <= len(p) }

	sampleOff := dataCursor
	if flags&trunDataOffset != 0 {
		if !need(4) {
			return dataCursor, dts, errf("trun truncated data_offset")
		}
		sampleOff = base + int64(int32(binary.BigEndian.Uint32(p[off:off+4])))
		off += 4
	}
	var firstFlags uint32
	haveFirstFlags := flags&trunFirstSampleFlag != 0
	if haveFirstFlags {
		if !need(4) {
			return dataCursor, dts, errf("trun truncated first_sample_flags")
		}
		firstFlags = binary.BigEndian.Uint32(p[off : off+4])
		off += 4
	}

	for i := uint32(0); i < count; i++ {
		dur := defDur
		if flags&trunSampleDuration != 0 {
			if !need(4) {
				return dataCursor, dts, errf("trun truncated sample_duration")
			}
			dur = binary.BigEndian.Uint32(p[off : off+4])
			off += 4
		}
		sz := defSize
		if flags&trunSampleSize != 0 {
			if !need(4) {
				return dataCursor, dts, errf("trun truncated sample_size")
			}
			sz = binary.BigEndian.Uint32(p[off : off+4])
			off += 4
		}
		sflags := defFlags
		if flags&trunSampleFlags != 0 {
			if !need(4) {
				return dataCursor, dts, errf("trun truncated sample_flags")
			}
			sflags = binary.BigEndian.Uint32(p[off : off+4])
			off += 4
		}
		if i == 0 && haveFirstFlags {
			sflags = firstFlags
		}
		var ctsOff int64
		if flags&trunSampleCTS != 0 {
			if !need(4) {
				return dataCursor, dts, errf("trun truncated composition_offset")
			}
			raw := binary.BigEndian.Uint32(p[off : off+4])
			if version == 0 {
				ctsOff = int64(raw)
			} else {
				ctsOff = int64(int32(raw))
			}
			off += 4
		}

		cts := ticksToMs(dts+ctsOff, t.timescale) + t.editShiftMs
		if cts < 0 {
			cts = 0
		}
		t.samples = append(t.samples, inSample{
			offset: sampleOff,
			size:   sz,
			dtsMs:  ticksToMs(dts, t.timescale),
			ctsMs:  cts,
			durMs:  ticksToMs(int64(dur), t.timescale),
			sync:   sflags&sampleNonSync == 0,
		})
		sampleOff += int64(sz)
		dts += int64(dur)
	}
	return sampleOff, dts, nil
}
