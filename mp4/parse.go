package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// dashRoleScheme is the schemeURI of the DASH role kind box (ISO/IEC 23009-1)
// that ffmpeg uses to record an MP4 track's "forced" disposition.
const dashRoleScheme = "urn:mpeg:dash:role:2011"

// parse.go — a minimal, defensive ISO-BMFF reader for the boxes RemuxFromMP4
// needs: ftyp is ignored, moov is parsed for track configuration and sample
// tables, and mdat is left in place (samples are read from it on demand by their
// recorded offsets). It parses untrusted input, so every length is bounds-checked
// against the data actually available and no allocation is sized by an attacker.

const (
	// maxSamples caps the per-track sample count parsed from a sample table. A
	// forged stsz/stco could otherwise declare billions of entries; this bounds
	// the index memory to a few hundred MB worst case. Real files stay far below.
	maxSamples = 1 << 27 // ~134M samples
)

// inSample is one parsed MP4 sample: where its bytes live in the file, its size,
// and its decode/composition times already converted to milliseconds.
type inSample struct {
	offset int64
	size   uint32
	dtsMs  int64
	ctsMs  int64
	durMs  int64
	sync   bool
}

// inTrack is a parsed MP4 track ready to be written as a Matroska track.
type inTrack struct {
	trackID          uint32 // tkhd track_ID, for correlating mvex/trex and mfra/tfra
	trackType        mkv.TrackType
	codec            string // mkvgo short codec name (e.g. "h264", "aac")
	codecPrivate     []byte
	width            uint32
	height           uint32
	displayWidth     uint32 // from a pasp box (anamorphic); 0 when pixels are square
	displayHeight    uint32
	channels         uint8
	sampleRate       float64
	outputSampleRate float64 // SBR (HE-AAC) decoder output rate; 0 when not SBR
	bitrate          uint32  // average bitrate (btrt box / esds avgBitrate); 0 when unknown
	frameRate        float64 // nominal (CFR) frame rate from stts[0]; 0 when unknown
	frameDurNs       int64   // constant frame duration (audio, single-entry stts); 0 when not constant
	rotation         int     // clockwise display rotation from the tkhd matrix (0/90/180/270)
	frameCount       int64   // sample count from stsz (ffprobe nb_frames); 0 when unknown
	durationMs       int64   // per-track duration from mdhd; 0 when unknown
	timescale        uint32
	samples          []inSample
	keyframesMs      []int64 // sync-sample presentation times (sampleKeyframes mode); nil otherwise
	sampleEndMs      int64   // last sample's cts (sampleKeyframes mode), for the movie duration

	// language and selection flags read from the track header / media header.
	language      string // ISO 639-2 from mdhd (e.g. "fre"); "" when absent/"und"
	languageBCP47 string // BCP-47 from an elng box; "" when absent
	name          string // track name from the QuickTime udta/name box; "" when absent
	languageKnown bool   // a usable language was read (mdhd or elng)
	enabled       bool   // tkhd track_enabled flag (ffprobe's "default" disposition)
	flagsKnown    bool   // a tkhd was parsed, so enabled is meaningful
	forced        bool   // a DASH-role kind box marks this track forced
	forcedKnown   bool   // a DASH-role kind box was read, so forced is meaningful
	editShiftMs   int64  // edit-list (elst) presentation shift applied to sample times
	codecDelayNs  int64  // audio encoder priming (edit-list media_time) → Matroska CodecDelay

	// first sample location (stco[0] + stsz[0]), read head-only for the optional
	// in-band colour fallback; firstSampleSize == 0 when unavailable.
	firstSampleOffset int64
	firstSampleSize   uint32

	// colour code points (CICP), nil when the entry had no colr box.
	colorPrimaries   *uint16
	colorTransfer    *uint16
	colorMatrix      *uint16
	colorRange       *uint16
	colourDetermined bool // a colr box (nclx/nclc) was read for this track

	// Dolby Vision configuration (dvcC/dvvC), nil when absent.
	dolbyVision *mkv.DolbyVision

	// HDR10 static metadata (clli + mdcv boxes), nil when absent.
	hdr *mkv.HDRStaticMetadata

	// 3D stereo arrangement (st3d, mapped to Matroska StereoMode) and 360/spherical
	// projection (sv3d); nil / "" for ordinary flat 2D video.
	stereoMode *uint16
	projection string
}

// movie is the parsed result: the tracks RemuxFromMP4 will emit, plus any tracks
// that were recognised but not carried (so callers can surface them).
type movie struct {
	tracks     []inTrack
	chapters   []mkv.Chapter
	dropped    []DroppedTrack
	durationMs int64             // from mvhd, used when the sample table was not built
	tags       []mkv.SimpleTag   // file-level metadata from udta/meta/ilst
	title      string            // ©nam, for Info.Title
	cover      *mkv.Attachment   // covr cover art, carried as an MKV attachment
	hashes     map[uint32]string // freeform CONTENT_SHA256_<id> atoms, for VerifyContentHashes
	fragmented bool              // an mvex box is present → sample data is in moof fragments
}

// memBox is a box parsed from an in-memory buffer.
type memBox struct {
	typ     string
	payload []byte
}

// iterBoxes parses the boxes laid out in buf. It returns an error (rather than
// panicking) on any malformed length so callers can fail the remux cleanly.
func iterBoxes(buf []byte) ([]memBox, error) {
	var out []memBox
	for off := 0; off+8 <= len(buf); {
		size := int64(binary.BigEndian.Uint32(buf[off : off+4]))
		typ := string(buf[off+4 : off+8])
		hdr := 8
		switch size {
		case 1:
			if off+16 > len(buf) {
				return nil, errf("truncated 64-bit box %q", typ)
			}
			size = int64(binary.BigEndian.Uint64(buf[off+8 : off+16]))
			hdr = 16
		case 0:
			size = int64(len(buf) - off)
		}
		if size < int64(hdr) || off+int(size) > len(buf) {
			return nil, errf("box %q has invalid size %d", typ, size)
		}
		out = append(out, memBox{typ: typ, payload: buf[off+hdr : off+int(size)]})
		off += int(size)
	}
	return out, nil
}

// genericHandlerName reports whether an hdlr name is a generic handler description
// (the default muxers write) rather than a meaningful track name, so it is not
// imported as Track.Name. Most muxers' defaults contain "Handler".
func genericHandlerName(s string) bool {
	switch s {
	case "", "SoundHandler", "VideoHandler", "SubtitleHandler", "DataHandler",
		"Core Media Audio", "Core Media Video", "Core Media Text":
		return true
	}
	return strings.Contains(s, "Handler")
}

func findMemBox(boxes []memBox, typ string) (memBox, bool) {
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return memBox{}, false
}

// findMoov returns the file offset and length of the moov box payload. It walks
// the top-level boxes by header; if that walk desyncs — a box whose declared
// size is wrong sends it into the mdat (some real files have a slightly-off mdat
// size) — it falls back to a bounded backward scan for the moov, which sits near
// the end in those files. That tolerance matches ffprobe, which still reads them.
func findMoov(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
	if dataOff, plen, ferr := findMoovForward(r, size); ferr == nil {
		return dataOff, plen, nil
	}
	return findMoovBackward(r, size)
}

func findMoovForward(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return 0, 0, err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return 0, 0, errf("read box header: %w", err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return 0, 0, errf("read largesize: %w", err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			return 0, 0, errf("box %q has invalid size %d at offset %d", typ, boxSize, off)
		}
		if typ == "moov" {
			return off + headerLen, boxSize - headerLen, nil
		}
		off += boxSize
	}
	return 0, 0, errf("no moov box found")
}

// maxMoovScanWindow caps the backward moov scan; a moov is at most tens of MB
// even for a long movie, so growing past this means there is genuinely no moov.
const maxMoovScanWindow = 256 << 20

// findMoovBackward scans back from EOF for the last moov box, validated by its
// first child being a real moov child (mvhd/trak/…), so a "moov" byte sequence
// inside the mdat is not mistaken for it.
func findMoovBackward(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
	for window := int64(1 << 20); ; window *= 4 {
		start := size - window
		atStart := false
		if start <= 0 {
			start, atStart = 0, true
		}
		buf := make([]byte, size-start)
		if _, err := r.Seek(start, io.SeekStart); err != nil {
			return 0, 0, err
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, 0, err
		}
		for end := len(buf); ; {
			i := bytes.LastIndex(buf[:end], []byte("moov"))
			if i < 4 { // need the 4-byte size field before the type
				break
			}
			boxStart := start + int64(i-4)
			boxSize := int64(binary.BigEndian.Uint32(buf[i-4 : i]))
			if boxSize >= 16 && boxStart+boxSize <= size && validMoovAt(r, boxStart, boxSize, size) {
				return boxStart + 8, boxSize - 8, nil
			}
			end = i
		}
		if atStart || window >= maxMoovScanWindow {
			return 0, 0, errf("no moov box found")
		}
	}
}

// validMoovAt confirms a moov candidate by reading its first child header: a real
// moov opens with a known child box (mvhd in practice) whose size fits inside it.
func validMoovAt(r io.ReadSeeker, boxStart, boxSize, size int64) bool {
	if boxStart+16 > size {
		return false
	}
	var hdr [8]byte
	if _, err := r.Seek(boxStart+8, io.SeekStart); err != nil {
		return false
	}
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return false
	}
	childSize := int64(binary.BigEndian.Uint32(hdr[:4]))
	if childSize < 8 || childSize > boxSize-8 {
		return false
	}
	switch string(hdr[4:8]) {
	case "mvhd", "iods", "trak", "mvex", "udta", "meta":
		return true
	}
	return false
}

// readMoov returns the full moov box payload — every sample-table byte included.
// Used by the remux/extract path, which needs the complete tables.
func readMoov(r io.ReadSeeker, size int64) ([]byte, error) {
	dataOff, payloadLen, err := findMoov(r, size)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, payloadLen)
	if _, err := r.Seek(dataOff, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, errf("read moov: %w", err)
	}
	return payload, nil
}

// lazyContainers are the boxes readMoovLazy descends into to reach the sample
// tables; lazySkipBody are the large sample-table boxes whose bodies it skips
// (reading only the header + first entry). Everything else is read whole.
var lazyContainers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true, "edts": true,
}
var lazySkipBody = map[string]bool{
	"stsz": true, "stco": true, "co64": true, "stsc": true, "stz2": true,
}

// lazyKeepHead is how much of a skipped box's body is still read: its
// version/flags + count + first entry — enough for headerFrameCount and
// firstSampleLoc, which are all the metadata/keyframe path reads from them.
const lazyKeepHead = 16

// lazyChunk is how far ahead the lazy reader fills in one disk read, so the
// scattered small boxes (headers + stsd/stts/stss/ctts bodies) come in a few
// large contiguous reads rather than one read per box.
const lazyChunk = 64 << 10

// readMoovLazy returns a moov payload buffer of the real moov size, reading from
// disk only the small boxes plus the header and first entry of the large
// sample-table boxes (stsz/stco/co64/stsc/stz2); their bodies are left zeroed and
// never fetched. The buffer is structurally identical to a full moov read (same
// box headers at the same offsets), so iterBoxes/parseTrak treat it the same —
// the metadata/keyframe path never reads the skipped bodies. Reads are coalesced
// into lazyChunk-sized runs and only the large bodies are seeked over, so it is
// both I/O-light and round-trip-light on a network mount, where those sample
// tables are most of the moov's bytes. Any read error or out-of-range box returns
// an error, so the caller falls back to the full read.
func readMoovLazy(r io.ReadSeeker, size int64) ([]byte, error) {
	dataOff, payloadLen, err := findMoov(r, size)
	if err != nil {
		return nil, err
	}
	m := &lazyMoov{r: r, base: dataOff, buf: make([]byte, payloadLen)}
	if err := m.parse(0, len(m.buf)); err != nil {
		return nil, err
	}
	return m.buf, nil
}

// lazyMoov fills a moov buffer forward, coalescing reads and skipping the large
// sample-table bodies. filled is how far buf is populated (read, or zeroed by a
// skip); the next disk read starts at base+filled.
type lazyMoov struct {
	r      io.ReadSeeker
	base   int64
	buf    []byte
	filled int
}

// ensure loads buf up to at least n bytes, reading from disk in lazyChunk runs.
func (m *lazyMoov) ensure(n int) error {
	if n <= m.filled {
		return nil
	}
	end := m.filled + lazyChunk
	if end < n {
		end = n
	}
	if end > len(m.buf) {
		end = len(m.buf)
	}
	if _, err := m.r.Seek(m.base+int64(m.filled), io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(m.r, m.buf[m.filled:end]); err != nil {
		return err
	}
	m.filled = end
	return nil
}

// skip advances filled past a box body without reading it (the bytes stay zero);
// the next ensure seeks over the gap.
func (m *lazyMoov) skip(boxEnd int) {
	if boxEnd > m.filled {
		m.filled = boxEnd
	}
}

func (m *lazyMoov) parse(start, end int) error {
	for off := start; off+8 <= end; {
		if err := m.ensure(off + 8); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(m.buf[off : off+4]))
		typ := string(m.buf[off+4 : off+8])
		hdr := 8
		switch boxSize {
		case 1:
			if off+16 > end {
				return errf("truncated 64-bit box %q", typ)
			}
			if err := m.ensure(off + 16); err != nil {
				return err
			}
			boxSize = int64(binary.BigEndian.Uint64(m.buf[off+8 : off+16]))
			hdr = 16
		case 0:
			boxSize = int64(end - off)
		}
		if boxSize < int64(hdr) || off+int(boxSize) > end {
			return errf("box %q has invalid size %d", typ, boxSize)
		}
		boxEnd := off + int(boxSize)
		switch {
		case lazyContainers[typ]:
			if err := m.parse(off+hdr, boxEnd); err != nil {
				return err
			}
		case lazySkipBody[typ]:
			keep := boxEnd
			if off+hdr+lazyKeepHead < keep {
				keep = off + hdr + lazyKeepHead
			}
			if err := m.ensure(keep); err != nil {
				return err
			}
			m.skip(boxEnd)
		default:
			if err := m.ensure(boxEnd); err != nil {
				return err
			}
		}
		off = boxEnd
	}
	return nil
}

// sampleMode selects how much per-sample work parseMP4 does:
//   - sampleNone: head-only, no per-sample table (the cheapest probe).
//   - sampleKeyframes: only the sync-sample presentation times (stss + stts/ctts),
//     for the metadata keyframe index — no byte offsets, so far cheaper than a
//     full table on a long movie.
//   - sampleFull: the full table (offsets + sizes + timing), which a remux or a
//     subtitle/sample extract needs to read sample bytes.
type sampleMode int

const (
	sampleNone sampleMode = iota
	sampleKeyframes
	sampleFull
)

// parseMP4 reads and parses the movie header of a seekable MP4 of the given size.
func parseMP4(r io.ReadSeeker, size int64, mode sampleMode) (*movie, error) {
	moovPayload, err := readMoovForMode(r, size, mode)
	if err != nil {
		return nil, err
	}
	mv, err := parseMoov(moovPayload, size, mode)
	if err != nil {
		return nil, err
	}
	if mode == sampleKeyframes && mv.fragmented {
		// A fragmented file's keyframes live in the moof fragments; recover them
		// head-only from a random-access index, preferring the mfra/tfra at the
		// file tail, then the sidx at the head (the source streaming fMP4 carries
		// when there is no mfra). Best-effort: no usable index leaves Keyframes nil
		// and the caller falls back.
		readFragmentKeyframes(r, size, mv)
		if vt := videoTrack(mv); vt != nil && vt.keyframesMs == nil {
			readSidxKeyframes(r, size, mv)
		}
	}
	return mv, nil
}

func videoTrack(mv *movie) *inTrack {
	for i := range mv.tracks {
		if mv.tracks[i].trackType == mkv.VideoTrack {
			return &mv.tracks[i]
		}
	}
	return nil
}

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
	for _, sidx := range scanSidxBoxes(r, size) {
		times, ok := sidxKeyframeMs(sidx, vt.trackID)
		if !ok {
			continue
		}
		ms := make([]int64, len(times))
		for i, t := range times {
			if v := t + vt.editShiftMs; v > 0 {
				ms[i] = v
			}
		}
		vt.keyframesMs = sortDedupTimes(ms)
		return
	}
}

// scanSidxBoxes returns the payloads of the sidx boxes that precede the first
// fragment (moof) — a bounded head scan of the top-level boxes.
func scanSidxBoxes(r io.ReadSeeker, size int64) [][]byte {
	var out [][]byte
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return out
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return out
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return out
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			return out
		}
		switch typ {
		case "sidx":
			payload := make([]byte, boxSize-headerLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				return out
			}
			out = append(out, payload)
		case "moof", "mdat":
			return out // fragments started; no head sidx beyond here
		}
		off += boxSize
	}
	return out
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

// readMoovForMode returns the moov payload: the full box for the remux/extract
// path (which needs the complete sample tables), or the I/O-light lazy read
// (with a full-read fallback on any unexpected layout) for the metadata path.
func readMoovForMode(r io.ReadSeeker, size int64, mode sampleMode) ([]byte, error) {
	if mode == sampleFull {
		return readMoov(r, size)
	}
	if payload, err := readMoovLazy(r, size); err == nil {
		return payload, nil
	}
	return readMoov(r, size)
}

// parseMoov parses an in-memory moov payload into a movie. Splitting it from the
// read lets the lazy and full moov buffers be parsed identically.
func parseMoov(moovPayload []byte, size int64, mode sampleMode) (*movie, error) {
	moovBoxes, err := iterBoxes(moovPayload)
	if err != nil {
		return nil, errf("parse moov: %w", err)
	}

	var mv movie
	// An mvex box marks a fragmented MP4: its trak sample tables are empty and the
	// real per-sample data (frame rate, keyframes) lives in the moof fragments,
	// which a head-only probe does not read. Record it so the caller can fall back.
	mv.fragmented = hasMemBox(moovBoxes, "mvex")

	// mvhd carries the movie timescale (needed to convert empty-edit durations,
	// which are in the movie timebase) and the movie duration (used as the metadata
	// duration when the sample table is not built).
	var movieTS uint32
	if mvhd, ok := findMemBox(moovBoxes, "mvhd"); ok {
		var durTicks uint64
		movieTS, durTicks = parseMovieHeader(mvhd.payload)
		if movieTS > 0 && durTicks > 0 && durTicks < 1<<62 && durTicks != 0xFFFFFFFF {
			mv.durationMs = int64(durTicks) * 1000 / int64(movieTS)
		}
	}
	if movieTS == 0 {
		movieTS = 1000
	}
	// QuickTime chapter tracks are referenced from the media tracks via tref/chap;
	// their content (the chapter titles) is already read from chpl, so they are not
	// "lost" media and must not be surfaced as dropped tracks.
	chapterTrackIDs := chapterTrackRefs(moovBoxes)

	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		tr, dropped, err := parseTrak(b.payload, size, movieTS, mode)
		if err != nil {
			return nil, err
		}
		if dropped != nil {
			if !chapterTrackIDs[uint32(dropped.ID)] {
				mv.dropped = append(mv.dropped, *dropped)
			}
			continue
		}
		mv.tracks = append(mv.tracks, tr)
	}
	if len(mv.tracks) == 0 {
		return nil, errf("no convertible tracks found in MP4")
	}
	if mv.fragmented {
		// The moov's sample tables are empty, so headerFrameRate gave 0. The
		// per-fragment default sample duration in mvex>trex yields the (CFR) frame
		// rate head-only — what ffprobe reports for a constant-rate fragmented stream.
		defDur := trexDefaultDurations(moovBoxes)
		for i := range mv.tracks {
			t := &mv.tracks[i]
			if t.trackType == mkv.VideoTrack && t.frameRate == 0 && t.timescale > 0 {
				if d := defDur[t.trackID]; d > 0 {
					t.frameRate = float64(t.timescale) / float64(d)
				}
			}
		}
	}
	if udta, ok := findMemBox(moovBoxes, "udta"); ok {
		if ub, err := iterBoxes(udta.payload); err == nil {
			if chpl, ok := findMemBox(ub, "chpl"); ok {
				mv.chapters = parseChpl(chpl.payload)
			}
			mv.tags, mv.title, mv.cover, mv.hashes = parseMP4Tags(ub)
		}
	}
	return &mv, nil
}

// trexDefaultDurations reads mvex>trex and returns each track's
// default_sample_duration — the per-fragment default a CFR fragmented stream
// uses for samples that do not carry their own duration.
func trexDefaultDurations(moovBoxes []memBox) map[uint32]uint32 {
	mvex, ok := findMemBox(moovBoxes, "mvex")
	if !ok {
		return nil
	}
	boxes, err := iterBoxes(mvex.payload)
	if err != nil {
		return nil
	}
	out := make(map[uint32]uint32)
	for _, b := range boxes {
		// trex: version/flags(4) track_ID(4) default_sample_description_index(4)
		// default_sample_duration(4) default_sample_size(4) default_sample_flags(4).
		if b.typ == "trex" && len(b.payload) >= 16 {
			out[binary.BigEndian.Uint32(b.payload[4:8])] = binary.BigEndian.Uint32(b.payload[12:16])
		}
	}
	return out
}

// metaAtomNames maps the common iTunes/QuickTime ilst atom types to Matroska-style
// tag names. The leading byte 0xA9 is the "©" copyright-sign prefix.
var metaAtomNames = map[string]string{
	"\xa9nam": "TITLE",
	"\xa9ART": "ARTIST",
	"\xa9alb": "ALBUM",
	"\xa9day": "DATE_RELEASED",
	"\xa9gen": "GENRE",
	"\xa9cmt": "COMMENT",
	"\xa9too": "ENCODER",
	"\xa9wrt": "COMPOSER",
	"desc":    "DESCRIPTION",
}

// parseMP4Tags reads file-level metadata from a udta box's meta/ilst atoms (the
// iTunes-style tags). It returns the tags as Matroska SimpleTags plus the title
// (©nam), for Info.Title, and the cover art (covr) as a Matroska-style
// attachment. Other non-text values are skipped.
func parseMP4Tags(udtaBoxes []memBox) (tags []mkv.SimpleTag, title string, cover *mkv.Attachment, hashes map[uint32]string) {
	meta, ok := findMemBox(udtaBoxes, "meta")
	if !ok || len(meta.payload) < 4 {
		return nil, "", nil, nil
	}
	// meta is a FullBox: 4 bytes of version/flags precede its child boxes. A few
	// muxers omit them, so fall back to parsing from the start.
	metaBoxes, err := iterBoxes(meta.payload[4:])
	if err != nil || !hasMemBox(metaBoxes, "ilst") {
		if mb, err2 := iterBoxes(meta.payload); err2 == nil && hasMemBox(mb, "ilst") {
			metaBoxes = mb
		}
	}
	ilst, ok := findMemBox(metaBoxes, "ilst")
	if !ok {
		return nil, "", nil, nil
	}
	atoms, err := iterBoxes(ilst.payload)
	if err != nil {
		return nil, "", nil, nil
	}
	for _, a := range atoms {
		if a.typ == "covr" {
			if att := ilstCoverValue(a.payload); att != nil {
				cover = att
			}
			continue
		}
		if a.typ == "----" {
			// Freeform atom: mkvgo's per-track content hashes live here
			// (name "CONTENT_SHA256_<track_ID>"). Not imported as text tags.
			if id, v, ok := freeformContentHash(a.payload); ok {
				if hashes == nil {
					hashes = map[uint32]string{}
				}
				hashes[id] = v
			}
			continue
		}
		name, ok := metaAtomNames[a.typ]
		if !ok {
			continue
		}
		val := ilstDataValue(a.payload)
		if val == "" {
			continue
		}
		tags = append(tags, mkv.SimpleTag{Name: name, Value: val})
		if name == "TITLE" {
			title = val
		}
	}
	return tags, title, cover, hashes
}

// freeformContentHash decodes a freeform ilst atom when it carries one of
// mkvgo's CONTENT_SHA256_<track_ID> values.
func freeformContentHash(atomPayload []byte) (uint32, string, bool) {
	boxes, err := iterBoxes(atomPayload)
	if err != nil {
		return 0, "", false
	}
	nameBox, ok := findMemBox(boxes, "name")
	if !ok || len(nameBox.payload) < 4 {
		return 0, "", false
	}
	name := string(nameBox.payload[4:]) // skip the fullbox version/flags
	const prefix = "CONTENT_SHA256_"
	if !strings.HasPrefix(name, prefix) {
		return 0, "", false
	}
	id, err := strconv.ParseUint(name[len(prefix):], 10, 32)
	if err != nil {
		return 0, "", false
	}
	val := ilstDataValue(atomPayload)
	if val == "" {
		return 0, "", false
	}
	return uint32(id), val, true
}

// ilstCoverValue extracts a covr atom's image as a Matroska-style attachment.
// The data box's well-known type selects the format: 13 = JPEG, 14 = PNG.
func ilstCoverValue(atomPayload []byte) *mkv.Attachment {
	boxes, err := iterBoxes(atomPayload)
	if err != nil {
		return nil
	}
	data, ok := findMemBox(boxes, "data")
	if !ok || len(data.payload) < 8 {
		return nil
	}
	name, mime := "cover.jpg", "image/jpeg"
	switch binary.BigEndian.Uint32(data.payload[0:4]) & 0xFFFFFF {
	case 13: // JPEG
	case 14: // PNG
		name, mime = "cover.png", "image/png"
	default:
		return nil
	}
	img := data.payload[8:]
	if len(img) == 0 {
		return nil
	}
	return &mkv.Attachment{
		ID: 1, Name: name, MIMEType: mime,
		Size: int64(len(img)), Data: append([]byte(nil), img...),
	}
}

func hasMemBox(boxes []memBox, typ string) bool {
	_, ok := findMemBox(boxes, typ)
	return ok
}

// ilstDataValue extracts the UTF-8 text value from an ilst atom's "data" box,
// or "" when the atom carries a non-text value (binary/integer, e.g. cover art).
func ilstDataValue(atomPayload []byte) string {
	boxes, err := iterBoxes(atomPayload)
	if err != nil {
		return ""
	}
	data, ok := findMemBox(boxes, "data")
	if !ok || len(data.payload) < 8 {
		return ""
	}
	// data: version(1)+well-known-type(3) reserved(4) value… Type 1 is UTF-8 text.
	if binary.BigEndian.Uint32(data.payload[0:4])&0xFFFFFF != 1 {
		return ""
	}
	return strings.TrimRight(string(data.payload[8:]), "\x00")
}

// chapterTrackRefs returns the set of track_IDs referenced as QuickTime chapter
// tracks (trak/tref/chap) by any track in the movie.
func chapterTrackRefs(moovBoxes []memBox) map[uint32]bool {
	ids := map[uint32]bool{}
	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		tb, err := iterBoxes(b.payload)
		if err != nil {
			continue
		}
		tref, ok := findMemBox(tb, "tref")
		if !ok {
			continue
		}
		rb, err := iterBoxes(tref.payload)
		if err != nil {
			continue
		}
		chap, ok := findMemBox(rb, "chap")
		if !ok {
			continue
		}
		for off := 0; off+4 <= len(chap.payload); off += 4 {
			ids[binary.BigEndian.Uint32(chap.payload[off:off+4])] = true
		}
	}
	return ids
}

// parseTrak parses one trak box. On success it returns the track and a nil
// *DroppedTrack. When the track has a recognised structure but cannot be carried
// — a non-media handler (hint/timecode/metadata) or an unsupported sample entry
// (e.g. cover art / attached picture) — it returns a zero track and a non-nil
// *DroppedTrack describing it, so the caller can surface it instead of dropping it
// silently. An error is returned only for malformed structure.
func parseTrak(payload []byte, fileSize int64, movieTS uint32, mode sampleMode) (inTrack, *DroppedTrack, error) {
	var tr inTrack
	trakBoxes, err := iterBoxes(payload)
	if err != nil {
		return tr, nil, err
	}
	// tkhd carries the selection flags and the track_ID. track_enabled (bit 0) is
	// what ffmpeg maps to the "default" disposition, so it feeds Track.IsDefault on
	// the read side; the track_ID labels any DroppedTrack for correlation.
	var trackID uint64
	if tkhd, ok := findMemBox(trakBoxes, "tkhd"); ok {
		tr.enabled = tkhdEnabled(tkhd.payload)
		tr.flagsKnown = true
		tr.trackID = tkhdTrackID(tkhd.payload)
		trackID = uint64(tr.trackID)
		tr.rotation = tkhdRotation(tkhd.payload)
	}
	// MP4 has no native "forced" flag; ffmpeg records it as a track-level kind box
	// with the DASH role scheme (e.g. value "forced-subtitle"), which its demuxer
	// reads back as AV_DISPOSITION_FORCED — regardless of the track's media type.
	if udta, ok := findMemBox(trakBoxes, "udta"); ok {
		if ub, err := iterBoxes(udta.payload); err == nil {
			for _, kb := range ub {
				switch kb.typ {
				case "kind":
					if scheme, value := parseKind(kb.payload); scheme == dashRoleScheme {
						tr.forcedKnown = true
						if strings.HasPrefix(value, "forced") {
							tr.forced = true
						}
					}
				case "name":
					// QuickTime track-name box (the way ffmpeg writes a per-track
					// title): the payload is the raw UTF-8 name. -> Matroska Name.
					tr.name = string(bytes.TrimRight(kb.payload, "\x00"))
				}
			}
		}
	}

	mdia, ok := findMemBox(trakBoxes, "mdia")
	if !ok {
		return tr, nil, errf("trak without mdia")
	}
	mdiaBoxes, err := iterBoxes(mdia.payload)
	if err != nil {
		return tr, nil, err
	}

	if mdhd, ok := findMemBox(mdiaBoxes, "mdhd"); ok {
		var lang string
		tr.timescale, lang = parseMdhd(mdhd.payload)
		tr.durationMs = mdhdDurationMs(mdhd.payload)
		if lang != "" {
			tr.language = lang
			tr.languageKnown = true
		}
	}
	if tr.timescale == 0 {
		tr.timescale = 1000
	}
	// Edit list (edts/elst): an empty edit adds a presentation delay (movie
	// timebase); a non-empty edit's media_time trims the start (media timebase).
	// Resolved once the track type is known (below the hdlr): for audio the trim is
	// the encoder priming and becomes CodecDelay; otherwise it folds into editShiftMs.
	var editMediaTime, editEmptyDur int64
	var hasEdit bool
	if edts, ok := findMemBox(trakBoxes, "edts"); ok {
		if eb, err := iterBoxes(edts.payload); err == nil {
			if elst, ok := findMemBox(eb, "elst"); ok {
				editMediaTime, editEmptyDur, hasEdit = parseElst(elst.payload)
			}
		}
	}
	// An elng box (ISO 14496-12) overrides mdhd with a full BCP-47 tag.
	if elng, ok := findMemBox(mdiaBoxes, "elng"); ok {
		if bcp47 := parseElng(elng.payload); bcp47 != "" {
			tr.languageBCP47 = bcp47
			tr.languageKnown = true
		}
	}

	hdlr, ok := findMemBox(mdiaBoxes, "hdlr")
	if !ok {
		return tr, nil, errf("mdia without hdlr")
	}
	if len(hdlr.payload) < 12 {
		return tr, nil, errf("hdlr too short")
	}
	handler := string(hdlr.payload[8:12])
	// Fallback track name: when no udta/name box was present, a non-generic hdlr name
	// (the field ffprobe surfaces as handler_name) is the track's human-readable name.
	if tr.name == "" && len(hdlr.payload) > 24 {
		if hn := string(bytes.TrimRight(hdlr.payload[24:], "\x00")); !genericHandlerName(hn) {
			tr.name = hn
		}
	}
	switch handler {
	case "vide":
		tr.trackType = mkv.VideoTrack
	case "soun":
		tr.trackType = mkv.AudioTrack
	case "text", "sbtl":
		tr.trackType = mkv.SubtitleTrack
	default:
		// hint/timecode/metadata — not media; surface it rather than dropping it.
		return tr, &DroppedTrack{ID: trackID, Codec: handler,
			Reason: "non-media handler " + quoteFourcc(handler)}, nil
	}

	// Resolve the edit list now the track type is known. An audio media_time trim
	// is the gapless/encoder priming: carry it as CodecDelay (preserved across the
	// round-trip) rather than shifting block times. Video keeps the net shift.
	if hasEdit {
		emptyMs := ticksToMs(editEmptyDur, movieTS)
		if tr.trackType == mkv.AudioTrack && editMediaTime > 0 {
			tr.codecDelayNs = ticksToNs(editMediaTime, tr.timescale)
			tr.editShiftMs = emptyMs
		} else {
			tr.editShiftMs = emptyMs - ticksToMs(editMediaTime, tr.timescale)
		}
	}

	minf, ok := findMemBox(mdiaBoxes, "minf")
	if !ok {
		return tr, nil, errf("mdia without minf")
	}
	minfBoxes, err := iterBoxes(minf.payload)
	if err != nil {
		return tr, nil, err
	}
	stbl, ok := findMemBox(minfBoxes, "stbl")
	if !ok {
		return tr, nil, errf("minf without stbl")
	}
	stblBoxes, err := iterBoxes(stbl.payload)
	if err != nil {
		return tr, nil, err
	}

	stsd, ok := findMemBox(stblBoxes, "stsd")
	if !ok {
		return tr, nil, errf("stbl without stsd")
	}
	codecOK, fourcc, err := parseSampleEntry(&tr, stsd.payload)
	if err != nil {
		return tr, nil, err
	}
	if !codecOK {
		// Recognised media handler but an unsupported sample entry — typically cover
		// art (an attached picture in a jpeg/png/… video track). Surface it.
		return tr, &DroppedTrack{ID: trackID, Type: tr.trackType, Codec: fourcc,
			Reason: "unsupported sample entry " + quoteFourcc(fourcc)}, nil
	}

	// The nominal frame rate (stts) and the frame count (stsz) are in the header, so
	// derive them head-only — no need to expand the sample table.
	if tr.trackType == mkv.VideoTrack {
		tr.frameRate = headerFrameRate(stblBoxes, tr.timescale)
		tr.frameCount = headerFrameCount(stblBoxes)
		tr.firstSampleOffset, tr.firstSampleSize = firstSampleLoc(stblBoxes)
	}
	if tr.trackType == mkv.AudioTrack {
		// A single-entry stts is a strictly constant frame duration: exported as
		// the Matroska DefaultDuration, which downstream grid-times the audio
		// (sample-exact fMP4/HLS timing survives the millisecond timeline).
		tr.frameDurNs = headerConstantFrameDurNs(stblBoxes, tr.timescale)
	}

	switch mode {
	case sampleKeyframes:
		// Only the keyframe index is needed: derive sync-sample times from
		// stss + stts/ctts, skipping the byte-offset resolution (stsz/stco/stsc)
		// that the full table builds.
		// endMs is computed for every track (the movie duration is the max across
		// tracks); only video collects the sync-sample keyframe times.
		isVideo := tr.trackType == mkv.VideoTrack
		kf, endMs, err := buildKeyframeTimes(stblBoxes, tr.timescale, tr.editShiftMs, isVideo, fileSize)
		if err != nil {
			return tr, nil, err
		}
		tr.sampleEndMs = endMs
		if isVideo {
			tr.keyframesMs = kf
		}
	case sampleFull:
		if err := buildSampleTable(&tr, stblBoxes, fileSize); err != nil {
			return tr, nil, err
		}
	}
	return tr, nil, nil
}

// headerFrameRate derives the nominal (constant) frame rate from the first stts
// entry without expanding the sample table: timescale / sample_delta. This is
// ffprobe's r_frame_rate for CFR video and is readable head-only. Returns 0 when
// the stts is absent/short or the delta is zero.
func headerFrameRate(stblBoxes []memBox, timescale uint32) float64 {
	if timescale == 0 {
		return 0
	}
	stts, ok := findMemBox(stblBoxes, "stts")
	if !ok || len(stts.payload) < 16 {
		return 0
	}
	// stts: version+flags(4), entry_count(4), then [sample_count(4) sample_delta(4)]…
	delta := binary.BigEndian.Uint32(stts.payload[12:16])
	if delta == 0 {
		return 0
	}
	return float64(timescale) / float64(delta)
}

// headerConstantFrameDurNs returns the constant per-frame duration in
// nanoseconds when the stts declares exactly one entry (every sample the same
// delta), 0 otherwise. Head-only, like headerFrameRate.
func headerConstantFrameDurNs(stblBoxes []memBox, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	stts, ok := findMemBox(stblBoxes, "stts")
	if !ok || len(stts.payload) < 16 {
		return 0
	}
	if binary.BigEndian.Uint32(stts.payload[4:8]) != 1 { // entry_count
		return 0
	}
	delta := binary.BigEndian.Uint32(stts.payload[12:16])
	if delta == 0 {
		return 0
	}
	return (int64(delta)*1_000_000_000 + int64(timescale)/2) / int64(timescale)
}

// headerFrameCount returns the sample count (= frame count for a video track)
// from the stsz/stz2 box, read head-only from the header. 0 when absent.
func headerFrameCount(stblBoxes []memBox) int64 {
	if stsz, ok := findMemBox(stblBoxes, "stsz"); ok && len(stsz.payload) >= 12 {
		// stsz: version+flags(4), sample_size(4), sample_count(4).
		return int64(binary.BigEndian.Uint32(stsz.payload[8:12]))
	}
	if stz2, ok := findMemBox(stblBoxes, "stz2"); ok && len(stz2.payload) >= 12 {
		// stz2: version+flags(4), reserved(3)+field_size(1), sample_count(4).
		return int64(binary.BigEndian.Uint32(stz2.payload[8:12]))
	}
	return 0
}

// parseMovieHeader reads the movie timescale and duration from an mvhd box.
func parseMovieHeader(payload []byte) (timescale uint32, durationTicks uint64) {
	if len(payload) < 4 {
		return 0, 0
	}
	if payload[0] == 1 {
		// version1: creation(8) modification(8) timescale(4)@20 duration(8)@24
		if len(payload) >= 32 {
			timescale = binary.BigEndian.Uint32(payload[20:24])
			durationTicks = binary.BigEndian.Uint64(payload[24:32])
		}
		return timescale, durationTicks
	}
	// version0: creation(4) modification(4) timescale(4)@12 duration(4)@16
	if len(payload) >= 20 {
		timescale = binary.BigEndian.Uint32(payload[12:16])
		durationTicks = uint64(binary.BigEndian.Uint32(payload[16:20]))
	}
	return timescale, durationTicks
}

// parseMdhd reads an mdhd box and returns the media timescale and the ISO 639-2
// language code (decoded from the packed 16-bit field). lang is "" when the box
// is too short, the field is zero, or it decodes to "und" (undefined).
func parseMdhd(payload []byte) (timescale uint32, lang string) {
	if len(payload) < 4 {
		return 0, ""
	}
	// version0: creation(4)+modification(4)+timescale(4)+duration(4)+language(2)
	// version1: creation(8)+modification(8)+timescale(4)+duration(8)+language(2)
	tsOff, langOff := 12, 20
	if payload[0] == 1 {
		tsOff, langOff = 20, 32
	}
	if len(payload) >= tsOff+4 {
		timescale = binary.BigEndian.Uint32(payload[tsOff : tsOff+4])
	}
	if len(payload) >= langOff+2 {
		lang = decodeMdhdLanguage(binary.BigEndian.Uint16(payload[langOff : langOff+2]))
	}
	return timescale, lang
}

// mdhdDurationMs returns the track's duration in milliseconds from an mdhd box
// (duration ÷ media timescale), or 0 when absent/unset. Read head-only — the
// per-track counterpart of the movie duration in mvhd.
func mdhdDurationMs(payload []byte) int64 {
	if len(payload) < 4 {
		return 0
	}
	tsOff, durOff, durSize := 12, 16, 4
	if payload[0] == 1 {
		tsOff, durOff, durSize = 20, 24, 8
	}
	if len(payload) < tsOff+4 || len(payload) < durOff+durSize {
		return 0
	}
	ts := binary.BigEndian.Uint32(payload[tsOff : tsOff+4])
	if ts == 0 {
		return 0
	}
	var ticks uint64
	if durSize == 8 {
		ticks = binary.BigEndian.Uint64(payload[durOff : durOff+8])
	} else {
		ticks = uint64(binary.BigEndian.Uint32(payload[durOff : durOff+4]))
	}
	if ticks == 0 || ticks == 0xFFFFFFFF || ticks >= 1<<62 {
		return 0 // unset/sentinel
	}
	return int64(ticks) * 1000 / int64(ts)
}

// parseElst reads an edit list box and returns the first non-empty edit's
// media_time (media timescale) together with the total duration of any leading
// empty edits (movie timescale). ok is false when the box carries no edit that
// shifts the presentation timeline. Later edits (multi-segment timelines) are not
// modelled — only the leading delay and the start trim, which is what shifts the
// keyframe times.
func parseElst(payload []byte) (mediaTime, emptyDuration int64, ok bool) {
	if len(payload) < 8 {
		return 0, 0, false
	}
	version := payload[0]
	count := binary.BigEndian.Uint32(payload[4:8])
	off := 8
	for i := uint32(0); i < count; i++ {
		var segDur, mt int64
		if version == 1 {
			if off+20 > len(payload) {
				break
			}
			segDur = int64(binary.BigEndian.Uint64(payload[off : off+8]))
			mt = int64(binary.BigEndian.Uint64(payload[off+8 : off+16])) // signed
			off += 20
		} else {
			if off+12 > len(payload) {
				break
			}
			segDur = int64(binary.BigEndian.Uint32(payload[off : off+4]))
			mt = int64(int32(binary.BigEndian.Uint32(payload[off+4 : off+8]))) // signed
			off += 12
		}
		if mt < 0 { // empty edit → presentation delay
			emptyDuration += segDur
			continue
		}
		return mt, emptyDuration, true // first non-empty edit: the start trim
	}
	if emptyDuration > 0 {
		return 0, emptyDuration, true
	}
	return 0, 0, false
}

// decodeMdhdLanguage unpacks the mdhd language field. ISO 639-2/T is packed as
// three 5-bit values, each offset by 0x60, forming a lowercase code. QuickTime-
// origin files instead store a small Macintosh language code: the two forms are
// unambiguous because the smallest valid packed code ("aaa") is 0x421, so any
// value below 0x400 is a Mac code. Returns "" for an unknown/zero/"und" value and
// for anything that is not three a–z letters.
func decodeMdhdLanguage(packed uint16) string {
	if packed < 0x400 {
		return macLanguageToISO(packed)
	}
	c := [3]byte{
		byte((packed>>10)&0x1f) + 0x60,
		byte((packed>>5)&0x1f) + 0x60,
		byte(packed&0x1f) + 0x60,
	}
	for _, ch := range c {
		if ch < 'a' || ch > 'z' {
			return ""
		}
	}
	if s := string(c[:]); s != "und" {
		return s
	}
	return ""
}

// macLanguageCodes maps the Macintosh language codes (QuickTime mdhd) to ISO
// 639-2/T, the same vocabulary the packed form yields. It is deliberately limited
// to codes seen in real media; an unlisted code resolves to "" (treated as absent)
// rather than risk a wrong mapping.
var macLanguageCodes = map[uint16]string{
	0: "eng", 1: "fra", 2: "deu", 3: "ita", 4: "nld", 5: "swe", 6: "spa",
	7: "dan", 8: "por", 9: "nor", 10: "heb", 11: "jpn", 12: "ara", 13: "fin",
	14: "ell", 15: "isl", 16: "mlt", 17: "tur", 18: "hrv", 19: "zho", 20: "urd",
	21: "hin", 22: "tha", 23: "kor", 24: "lit", 25: "pol", 26: "hun", 27: "est",
	28: "lav", 30: "fao", 31: "fas", 32: "rus", 33: "zho", 35: "gle", 36: "sqi",
	37: "ron", 38: "ces", 39: "slk", 40: "slv", 41: "yid", 42: "srp", 43: "mkd",
	44: "bul", 45: "ukr", 46: "bel", 48: "kaz", 49: "aze", 51: "hye", 52: "kat",
	80: "vie", 81: "ind",
}

func macLanguageToISO(code uint16) string { return macLanguageCodes[code] }

// tkhdEnabled reports whether a track header's track_enabled flag (bit 0 of the
// 24-bit flags) is set. ffmpeg maps this to the "default" stream disposition.
func tkhdEnabled(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	return flags&0x000001 != 0
}

// tkhdTrackID reads the track_ID from a track header. Its offset depends on the
// box version (v0 uses 32-bit times, v1 64-bit). Returns 0 when the box is short.
func tkhdTrackID(payload []byte) uint32 {
	off := 12 // v0: version+flags(4) + creation(4) + modification(4)
	if len(payload) > 0 && payload[0] == 1 {
		off = 20 // v1: version+flags(4) + creation(8) + modification(8)
	}
	if len(payload) < off+4 {
		return 0
	}
	return binary.BigEndian.Uint32(payload[off : off+4])
}

// tkhdRotation reads the 3×3 display matrix from a track header and returns the
// clockwise display rotation in degrees, normalised to [0,360). The angle comes
// from the matrix's a,b entries (16.16 fixed point): rotation = atan2(b, a). The
// matrix sits after the (version-dependent) timing fields, the reserved/layer/
// group/volume block. Returns 0 on an identity matrix or a short box.
func tkhdRotation(payload []byte) int {
	if len(payload) < 1 {
		return 0
	}
	matOff := 40 // v0: 4 + (4+4+4+4+4) + 8 + (2+2+2+2)
	if payload[0] == 1 {
		matOff = 52 // v1: timing fields are 8-byte
	}
	if len(payload) < matOff+8 {
		return 0
	}
	a := int32(binary.BigEndian.Uint32(payload[matOff : matOff+4]))
	b := int32(binary.BigEndian.Uint32(payload[matOff+4 : matOff+8]))
	if a == 0 && b == 0 {
		return 0
	}
	deg := int(math.Round(math.Atan2(float64(b), float64(a)) * 180 / math.Pi))
	return ((deg % 360) + 360) % 360
}

// quoteFourcc renders a four-character box type for a reason string, escaping any
// non-printable bytes (cover-art entries sometimes carry an all-zero fourcc).
func quoteFourcc(typ string) string { return strconv.Quote(typ) }

// parseElng reads the null-terminated BCP-47 tag from an elng (Extended Language
// Tag) box, skipping the 4-byte fullbox version/flags header.
func parseElng(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	s := payload[4:]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

// parseKind reads a kind box (ISO/IEC 14496-12): a fullbox holding a
// null-terminated schemeURI followed by a null-terminated value.
func parseKind(payload []byte) (scheme, value string) {
	if len(payload) < 4 {
		return "", ""
	}
	s := payload[4:] // skip version/flags
	i := bytes.IndexByte(s, 0)
	if i < 0 {
		return string(s), ""
	}
	scheme = string(s[:i])
	rest := s[i+1:]
	if j := bytes.IndexByte(rest, 0); j >= 0 {
		rest = rest[:j]
	}
	return scheme, string(rest)
}

// parseSampleEntry reads the first sample entry from an stsd box and fills the
// track's codec, codec private data and dimensions. It returns ok=false for a
// recognised-but-unsupported codec, along with the sample entry's fourcc so the
// caller can report it.
func parseSampleEntry(tr *inTrack, stsdPayload []byte) (bool, string, error) {
	if len(stsdPayload) < 8 {
		return false, "", errf("stsd too short")
	}
	// fullbox header(4) + entry_count(4), then the sample entry box.
	entries, err := iterBoxes(stsdPayload[8:])
	if err != nil {
		return false, "", errf("parse stsd: %w", err)
	}
	if len(entries) == 0 {
		return false, "", errf("stsd has no sample entry")
	}
	entry := entries[0]

	const visualHdr = 78
	audioHdr := audioExtOffset(entry.payload)

	switch entry.typ {
	case "avc1", "avc3", "dvav", "dva1":
		// dvav/dva1 are Dolby Vision over AVC: an avcC plus a dvcC/dvvC box.
		tr.codec = "h264"
		return true, entry.typ, extractVisual(tr, entry.payload, visualHdr, "avcC")
	case "hvc1", "hev1", "dvhe", "dvh1":
		// dvhe/dvh1 are Dolby Vision over HEVC: an hvcC plus a dvcC/dvvC box.
		tr.codec = "hevc"
		return true, entry.typ, extractVisual(tr, entry.payload, visualHdr, "hvcC")
	case "av01", "dav1":
		// dav1 is Dolby Vision over AV1: an av1C plus a dvvC box.
		tr.codec = "av1"
		return true, entry.typ, extractVisual(tr, entry.payload, visualHdr, "av1C")
	case "vp09":
		// The vpcC (FullBox form) becomes the Matroska CodecPrivate, so the
		// colour/bit-depth it carries survives into MKV (the reader parses both
		// forms).
		tr.codec = "vp9"
		return true, entry.typ, extractVisual(tr, entry.payload, visualHdr, "vpcC")
	case "mp4a":
		ok, err := parseMP4A(tr, entry.payload, audioHdr)
		return ok, entry.typ, err
	case "Opus":
		tr.codec = "opus"
		return true, entry.typ, extractOpus(tr, entry.payload, audioHdr)
	case "ac-3":
		tr.codec = "ac3"
		parseAudioFields(tr, entry.payload)
		if dac3, err := childConfig(entry.payload, audioHdr, "dac3"); err == nil {
			if ch := ac3Channels(dac3); ch > 0 {
				tr.channels = ch
			}
		}
		return true, entry.typ, nil
	case "ec-3":
		tr.codec = "eac3"
		parseAudioFields(tr, entry.payload)
		if dec3, err := childConfig(entry.payload, audioHdr, "dec3"); err == nil {
			if ch := eac3Channels(dec3); ch > 0 {
				tr.channels = ch
			}
		}
		return true, entry.typ, nil
	case "fLaC":
		tr.codec = "flac"
		return true, entry.typ, extractFLAC(tr, entry.payload, audioHdr)
	case "tx3g":
		tr.codec = "srt" // tx3g timed text → S_TEXT/UTF8
		return true, entry.typ, nil
	case "wvtt":
		tr.codec = "webvtt" // native WebVTT → S_TEXT/WEBVTT
		extractWVTTConfig(tr, entry.payload)
		return true, entry.typ, nil
	default:
		return false, entry.typ, nil
	}
}

// parseMP4A resolves an mp4a entry to either AAC or MP3 by reading the esds
// object type indication.
func parseMP4A(tr *inTrack, payload []byte, headerLen int) (bool, error) {
	parseAudioFields(tr, payload)
	esds, err := childConfig(payload, headerLen, "esds")
	if err != nil {
		return false, err
	}
	objType, asc, avgBitrate, err := parseESDS(esds)
	if err != nil {
		return false, err
	}
	if tr.bitrate == 0 { // a btrt box, when present, takes precedence
		tr.bitrate = avgBitrate
	}
	switch objType {
	case 0x40, 0x66, 0x67: // MPEG-4/2 AAC
		if len(asc) == 0 {
			return false, errf("mp4a/AAC without AudioSpecificConfig")
		}
		tr.codec = "aac"
		tr.codecPrivate = asc
		// The AudioSampleEntry channelcount and sample rate are unreliable for AAC:
		// multichannel layouts and HE-AAC SBR/PS live in the AudioSpecificConfig.
		cfg := parseAACConfig(asc)
		if cfg.channels > 0 {
			tr.channels = cfg.channels
		}
		if cfg.sampleRate > 0 {
			tr.sampleRate = cfg.sampleRate
		}
		tr.outputSampleRate = cfg.outputRate // SBR-doubled rate, 0 when not SBR
		return true, nil
	case 0x69, 0x6B: // MPEG-2/1 Audio Layer III
		tr.codec = "A_MPEG/L3"
		return true, nil
	case 0xA9, 0xAA, 0xAB, 0xAC: // DTS Coherent Acoustics / DTS-HD / DTS Express
		tr.codec = "dts"
		return true, nil
	default:
		return false, nil // unsupported audio object type — skip the track
	}
}

// extractFLAC rebuilds the MKV FLAC CodecPrivate ("fLaC" marker + metadata
// blocks) from a dfLa box (which holds the metadata blocks only).
func extractFLAC(tr *inTrack, payload []byte, headerLen int) error {
	parseAudioFields(tr, payload)
	dfla, err := childConfig(payload, headerLen, "dfLa")
	if err != nil {
		return err
	}
	if len(dfla) < 4 {
		return errf("dfLa too short")
	}
	meta := dfla[4:] // strip the fullbox version/flags
	cp := make([]byte, 0, 4+len(meta))
	cp = append(cp, "fLaC"...)
	cp = append(cp, meta...)
	tr.codecPrivate = cp
	return nil
}

// childConfig returns the payload of the named configuration box nested in a
// sample entry whose fixed header is headerLen bytes.
func childConfig(entryPayload []byte, headerLen int, configType string) ([]byte, error) {
	if len(entryPayload) < headerLen {
		return nil, errf("sample entry shorter than its %d-byte header", headerLen)
	}
	children, err := iterBoxes(entryPayload[headerLen:])
	if err != nil {
		return nil, err
	}
	cfg, ok := findMemBox(children, configType)
	if !ok {
		// QuickTime wraps an audio entry's config in a 'wave' extension
		// (siDecompressionParam: frma + the config + a terminator atom).
		if wave, wok := findMemBox(children, "wave"); wok {
			if sub, serr := iterBoxes(wave.payload); serr == nil {
				cfg, ok = findMemBox(sub, configType)
			}
		}
	}
	if !ok {
		return nil, errf("sample entry missing %s configuration box", configType)
	}
	return cfg.payload, nil
}

func extractVisual(tr *inTrack, payload []byte, headerLen int, configType string) error {
	if len(payload) >= 32 {
		tr.width = uint32(binary.BigEndian.Uint16(payload[24:26]))
		tr.height = uint32(binary.BigEndian.Uint16(payload[26:28]))
	}
	cfg, err := childConfig(payload, headerLen, configType)
	if err != nil {
		return err
	}
	tr.codecPrivate = append([]byte(nil), cfg...)
	parseColr(tr, payload, headerLen)
	parseHDRStatic(tr, payload, headerLen)
	parseSpatial(tr, payload, headerLen)
	parseDolbyVision(tr, payload, headerLen)
	parsePasp(tr, payload, headerLen)
	parseBitrate(tr, payload, headerLen)
	return nil
}

// parseBitrate reads a btrt box (BitRateBox: bufferSizeDB, maxBitrate, avgBitrate)
// from a sample entry and records the average bitrate — what ffprobe reports as
// bit_rate. It falls back to maxBitrate when the average is zero (some muxers
// only fill the max).
func parseBitrate(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	btrt, ok := findMemBox(children, "btrt")
	if !ok || len(btrt.payload) < 12 {
		return
	}
	avg := binary.BigEndian.Uint32(btrt.payload[8:12])
	if avg == 0 {
		avg = binary.BigEndian.Uint32(btrt.payload[4:8]) // maxBitrate fallback
	}
	if avg > 0 {
		tr.bitrate = avg
	}
}

// parsePasp reads a pasp box (PixelAspectRatio: hSpacing:vSpacing) from a visual
// sample entry and, when the pixels are non-square, records the display aspect as
// DisplayWidth:DisplayHeight = (codedWidth·hSpacing):(codedHeight·vSpacing),
// reduced. The ratio is stored exactly rather than as rounded display pixels:
// rounding would collapse a fine ratio (e.g. a 426:425 pasp) to square and yield
// the wrong sample/display aspect. Square pixels (1:1) leave the fields unset.
func parsePasp(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen || tr.width == 0 || tr.height == 0 {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	pasp, ok := findMemBox(children, "pasp")
	if !ok || len(pasp.payload) < 8 {
		return
	}
	h := binary.BigEndian.Uint32(pasp.payload[0:4])
	v := binary.BigEndian.Uint32(pasp.payload[4:8])
	if h == 0 || v == 0 || h == v {
		return // missing or square pixels
	}
	dw := uint64(tr.width) * uint64(h)
	dh := uint64(tr.height) * uint64(v)
	g := gcdU64(dw, dh)
	tr.displayWidth = uint32(dw / g)
	tr.displayHeight = uint32(dh / g)
}

// parseHDRStatic reads the HDR10 static-metadata boxes from a visual sample entry:
// clli (Content Light Level — MaxCLL/MaxFALL in cd/m²) and mdcv (Mastering Display
// Colour Volume, SMPTE ST 2086). mdcv stores its primaries in G,B,R order with
// chromaticities in units of 0.00002 and luminances in 0.0001 cd/m²; those are
// converted to the units MasteringDisplay holds. Absent boxes leave tr.hdr nil.
func parseHDRStatic(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	if clli, ok := findMemBox(children, "clli"); ok && len(clli.payload) >= 4 {
		h := ensureMP4HDR(tr)
		h.MaxCLL = uint32(binary.BigEndian.Uint16(clli.payload[0:2]))
		h.MaxFALL = uint32(binary.BigEndian.Uint16(clli.payload[2:4]))
	}
	if mdcv, ok := findMemBox(children, "mdcv"); ok && len(mdcv.payload) >= 24 {
		b := mdcv.payload
		chroma := func(off int) float64 { return float64(binary.BigEndian.Uint16(b[off:off+2])) / 50000 }
		lum := func(off int) float64 { return float64(binary.BigEndian.Uint32(b[off:off+4])) / 10000 }
		ensureMP4HDR(tr).MasteringDisplay = &mkv.MasteringDisplay{
			GreenX: chroma(0), GreenY: chroma(2), // mdcv primaries are G, B, R
			BlueX: chroma(4), BlueY: chroma(6),
			RedX: chroma(8), RedY: chroma(10),
			WhiteX: chroma(12), WhiteY: chroma(14),
			LuminanceMax: lum(16), LuminanceMin: lum(20),
		}
	}
}

// parseSpatial reads the spatial-media boxes from a visual sample entry: st3d
// (stereoscopic 3D, mapped to the Matroska StereoMode the model stores) and sv3d
// (spherical/360 projection). Absent boxes leave the fields unset.
func parseSpatial(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	if st3d, ok := findMemBox(children, "st3d"); ok && len(st3d.payload) >= 5 {
		if m := mp4StereoToMatroska(st3d.payload[4]); m != 0 { // [4] = stereo_mode, after version+flags
			sm := m
			tr.stereoMode = &sm
		}
	}
	if sv3d, ok := findMemBox(children, "sv3d"); ok {
		tr.projection = sv3dProjection(sv3d.payload)
	}
}

// mp4StereoToMatroska maps an st3d stereo_mode (0 mono, 1 top-bottom, 2 left-right)
// to the equivalent Matroska StereoMode value; 0 when mono or custom/unknown.
func mp4StereoToMatroska(mode byte) uint16 {
	switch mode {
	case 1:
		return 3 // top-bottom (left eye first)
	case 2:
		return 1 // side by side (left eye first)
	}
	return 0
}

// sv3dProjection returns the projection name from an sv3d box by the proj sub-box
// it carries (equi/cbmp/mesh); "" when none is recognised.
func sv3dProjection(payload []byte) string {
	children, err := iterBoxes(payload)
	if err != nil {
		return ""
	}
	proj, ok := findMemBox(children, "proj")
	if !ok {
		return ""
	}
	pb, err := iterBoxes(proj.payload)
	if err != nil {
		return ""
	}
	switch {
	case hasMemBox(pb, "equi"):
		return "equirectangular"
	case hasMemBox(pb, "cbmp"):
		return "cubemap"
	case hasMemBox(pb, "mesh"):
		return "mesh"
	}
	return ""
}

func ensureMP4HDR(tr *inTrack) *mkv.HDRStaticMetadata {
	if tr.hdr == nil {
		tr.hdr = &mkv.HDRStaticMetadata{}
	}
	return tr.hdr
}

// parseDolbyVision reads a dvcC or dvvC box from a visual sample entry and records
// the decoded Dolby Vision configuration. Both box types carry the same record;
// dvcC is used for profiles without a cross-compatibility id, dvvC for the rest.
func parseDolbyVision(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	for _, typ := range [...]string{"dvvC", "dvcC"} {
		if b, ok := findMemBox(children, typ); ok {
			if dv := mkv.ParseDolbyVisionConfig(b.payload); dv != nil {
				tr.dolbyVision = dv
				return
			}
		}
	}
}

// parseColr reads a colr box from a visual sample entry and records the colour
// code points. Two on-screen colour types are read: 'nclx' (CICP primaries /
// transfer / matrix plus a full-range flag) and 'nclc' (the QuickTime form —
// identical but without the range byte). ICC-profile types ('rICC', 'prof') are
// ignored. Each CICP field is taken independently, so an SDR stream that
// specifies only the matrix (e.g. BT.709) while leaving primaries/transfer
// unspecified still reports its colour_space, matching ffprobe.
func parseColr(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	colr, ok := findMemBox(children, "colr")
	if !ok || len(colr.payload) < 10 {
		return
	}
	typ := string(colr.payload[:4])
	if typ != "nclx" && typ != "nclc" {
		return
	}
	tr.colourDetermined = true // an on-screen colr box is present
	p := binary.BigEndian.Uint16(colr.payload[4:6])
	tr.colorPrimaries = &p
	tc := binary.BigEndian.Uint16(colr.payload[6:8])
	tr.colorTransfer = &tc
	m := binary.BigEndian.Uint16(colr.payload[8:10])
	tr.colorMatrix = &m
	// Only 'nclx' carries the full-range flag. For 'nclc' the range is left unset
	// so the codec-bitstream fallback (SPS VUI) can supply it, as ffmpeg does.
	if typ == "nclx" && len(colr.payload) >= 11 {
		rng := uint16(1) // limited
		if colr.payload[10]&0x80 != 0 {
			rng = 2 // full
		}
		tr.colorRange = &rng
	}
}

func extractOpus(tr *inTrack, payload []byte, headerLen int) error {
	parseAudioFields(tr, payload)
	dops, err := childConfig(payload, headerLen, "dOps")
	if err != nil {
		return err
	}
	head, err := opusHeadFromDOps(dops)
	if err != nil {
		return err
	}
	tr.codecPrivate = head
	return nil
}

// audioExtOffset returns the offset of the first extension box inside an audio
// sample entry payload. ISO and QuickTime version 0 entries have a 28-byte
// fixed part; a QuickTime SoundDescription version 1 adds 16 bytes of
// per-packet compression fields, and version 2 is a 64-byte struct with its
// own float64 sample rate. The version field sits at payload[8:10] — reserved
// (zero) in ISO files, so version 0 keeps the ISO layout.
func audioExtOffset(payload []byte) int {
	if len(payload) < 10 {
		return 28
	}
	switch binary.BigEndian.Uint16(payload[8:10]) {
	case 1:
		return 44
	case 2:
		return 64
	}
	return 28
}

func parseAudioFields(tr *inTrack, payload []byte) {
	ext := audioExtOffset(payload)
	if ext == 64 {
		// QuickTime SoundDescription v2: float64 sample rate at 32, 32-bit
		// channel count at 40 (the v0 fields hold placeholder constants).
		if len(payload) >= 64 {
			tr.sampleRate = math.Float64frombits(binary.BigEndian.Uint64(payload[32:40]))
			tr.channels = uint8(binary.BigEndian.Uint32(payload[40:44]))
			parseBitrate(tr, payload, ext)
		}
		return
	}
	// AudioSampleEntry: reserved(8) channels(2) samplesize(2) pre(2) res(2) rate(4 fixed16.16)
	if len(payload) >= 28 {
		tr.channels = uint8(binary.BigEndian.Uint16(payload[16:18]))
		tr.sampleRate = float64(binary.BigEndian.Uint32(payload[24:28]) >> 16)
	}
	if len(payload) >= ext {
		parseBitrate(tr, payload, ext) // a btrt box may sit among the entry's children
	}
}

// parseESDS walks an esds box payload's MPEG-4 descriptor tree and returns the
// object type indication, the DecoderSpecificInfo (the AudioSpecificConfig for
// AAC; absent for MP3, in which case asc is nil) and the average bitrate from the
// DecoderConfigDescriptor (0 when unset).
func parseESDS(esds []byte) (objType byte, asc []byte, avgBitrate uint32, err error) {
	if len(esds) < 4 {
		return 0, nil, 0, errf("esds too short")
	}
	d := &descReader{buf: esds[4:]} // skip fullbox version/flags
	tag, body, err := d.next()
	if err != nil || tag != 0x03 {
		return 0, nil, 0, errf("esds: expected ES_Descriptor")
	}
	// ES_Descriptor: ES_ID(2) + flags(1) + optional fields.
	es := &descReader{buf: body}
	if err := es.skip(3); err != nil {
		return 0, nil, 0, err
	}
	flags := body[2]
	if flags&0x80 != 0 { // dependsOn_ES_ID
		if err := es.skip(2); err != nil {
			return 0, nil, 0, err
		}
	}
	if flags&0x40 != 0 { // URL
		if es.pos >= len(es.buf) {
			return 0, nil, 0, errf("esds: truncated URL length")
		}
		urlLen := int(es.buf[es.pos])
		if err := es.skip(1 + urlLen); err != nil {
			return 0, nil, 0, err
		}
	}
	if flags&0x20 != 0 { // OCR
		if err := es.skip(2); err != nil {
			return 0, nil, 0, err
		}
	}
	tag, dcfg, err := es.next()
	if err != nil || tag != 0x04 {
		return 0, nil, 0, errf("esds: expected DecoderConfigDescriptor")
	}
	if len(dcfg) < 1 {
		return 0, nil, 0, errf("esds: empty DecoderConfigDescriptor")
	}
	objType = dcfg[0]
	// DecoderConfigDescriptor header: objectType(1) streamType(1) buffer(3)
	// max(4) avg(4) = 13 bytes, optionally followed by a DecoderSpecificInfo.
	if len(dcfg) >= 13 {
		avgBitrate = binary.BigEndian.Uint32(dcfg[9:13])
	}
	dc := &descReader{buf: dcfg}
	if err := dc.skip(13); err != nil {
		return objType, nil, avgBitrate, nil // no DecoderSpecificInfo (e.g. MP3)
	}
	tag, dsi, err := dc.next()
	if err != nil || tag != 0x05 {
		return objType, nil, avgBitrate, nil
	}
	return objType, append([]byte(nil), dsi...), avgBitrate, nil
}

// opusHeadFromDOps rebuilds an OpusHead (RFC 7845) from a dOps box payload.
func opusHeadFromDOps(dops []byte) ([]byte, error) {
	if len(dops) < 11 {
		return nil, errf("dOps too short")
	}
	channels := dops[1]
	preSkip := binary.BigEndian.Uint16(dops[2:4])
	rate := binary.BigEndian.Uint32(dops[4:8])
	gain := binary.BigEndian.Uint16(dops[8:10])
	family := dops[10]

	head := make([]byte, 0, 19)
	head = append(head, "OpusHead"...)
	head = append(head, 1, channels)
	head = binary.LittleEndian.AppendUint16(head, preSkip)
	head = binary.LittleEndian.AppendUint32(head, rate)
	head = binary.LittleEndian.AppendUint16(head, gain)
	head = append(head, family)
	if family != 0 {
		need := 2 + int(channels)
		if len(dops) < 11+need {
			return nil, errf("dOps channel mapping truncated")
		}
		head = append(head, dops[11:11+need]...)
	}
	return head, nil
}

// descReader walks a sequence of MPEG-4 descriptors (tag + expandable length).
type descReader struct {
	buf []byte
	pos int
}

func (d *descReader) skip(n int) error {
	if n < 0 || d.pos+n > len(d.buf) {
		return errf("descriptor: skip past end")
	}
	d.pos += n
	return nil
}

func (d *descReader) next() (tag uint8, body []byte, err error) {
	if d.pos >= len(d.buf) {
		return 0, nil, errf("descriptor: end of data")
	}
	tag = d.buf[d.pos]
	d.pos++
	size := 0
	for i := 0; i < 4; i++ {
		if d.pos >= len(d.buf) {
			return 0, nil, errf("descriptor: truncated length")
		}
		b := d.buf[d.pos]
		d.pos++
		size = size<<7 | int(b&0x7F)
		if b&0x80 == 0 {
			break
		}
	}
	if size < 0 || d.pos+size > len(d.buf) {
		return 0, nil, errf("descriptor: body length %d exceeds data", size)
	}
	body = d.buf[d.pos : d.pos+size]
	d.pos += size
	return tag, body, nil
}
