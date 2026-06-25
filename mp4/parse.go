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
	languageKnown bool   // a usable language was read (mdhd or elng)
	enabled       bool   // tkhd track_enabled flag (ffprobe's "default" disposition)
	flagsKnown    bool   // a tkhd was parsed, so enabled is meaningful
	forced        bool   // a DASH-role kind box marks this track forced
	forcedKnown   bool   // a DASH-role kind box was read, so forced is meaningful
	editShiftMs   int64  // edit-list (elst) presentation shift applied to sample times

	// first sample location (stco[0] + stsz[0]), read head-only for the optional
	// in-band colour fallback; firstSampleSize == 0 when unavailable.
	firstSampleOffset int64
	firstSampleSize   uint32

	// colour code points (CICP), nil when the entry had no colr box.
	colorPrimaries *uint16
	colorTransfer  *uint16
	colorMatrix    *uint16
	colorRange     *uint16

	// Dolby Vision configuration (dvcC/dvvC), nil when absent.
	dolbyVision *mkv.DolbyVision
}

// movie is the parsed result: the tracks RemuxFromMP4 will emit, plus any tracks
// that were recognised but not carried (so callers can surface them).
type movie struct {
	tracks     []inTrack
	chapters   []mkv.Chapter
	dropped    []DroppedTrack
	durationMs int64           // from mvhd, used when the sample table was not built
	tags       []mkv.SimpleTag // file-level metadata from udta/meta/ilst
	title      string          // ©nam, for Info.Title
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

func findMemBox(boxes []memBox, typ string) (memBox, bool) {
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return memBox{}, false
}

// findMoov scans the top-level boxes of a seekable MP4 and returns the file
// offset and length of the moov box payload. moov may appear before or after
// mdat, so the file is scanned by header until it is found.
func findMoov(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
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
	return parseMoov(moovPayload, size, mode)
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
	if udta, ok := findMemBox(moovBoxes, "udta"); ok {
		if ub, err := iterBoxes(udta.payload); err == nil {
			if chpl, ok := findMemBox(ub, "chpl"); ok {
				mv.chapters = parseChpl(chpl.payload)
			}
			mv.tags, mv.title = parseMP4Tags(ub)
		}
	}
	return &mv, nil
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
// (©nam), for Info.Title. Non-text values (cover art, etc.) are skipped.
func parseMP4Tags(udtaBoxes []memBox) (tags []mkv.SimpleTag, title string) {
	meta, ok := findMemBox(udtaBoxes, "meta")
	if !ok || len(meta.payload) < 4 {
		return nil, ""
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
		return nil, ""
	}
	atoms, err := iterBoxes(ilst.payload)
	if err != nil {
		return nil, ""
	}
	for _, a := range atoms {
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
	return tags, title
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
		trackID = uint64(tkhdTrackID(tkhd.payload))
		tr.rotation = tkhdRotation(tkhd.payload)
	}
	// MP4 has no native "forced" flag; ffmpeg records it as a track-level kind box
	// with the DASH role scheme (e.g. value "forced-subtitle"), which its demuxer
	// reads back as AV_DISPOSITION_FORCED — regardless of the track's media type.
	if udta, ok := findMemBox(trakBoxes, "udta"); ok {
		if ub, err := iterBoxes(udta.payload); err == nil {
			for _, kb := range ub {
				if kb.typ != "kind" {
					continue
				}
				if scheme, value := parseKind(kb.payload); scheme == dashRoleScheme {
					tr.forcedKnown = true
					if strings.HasPrefix(value, "forced") {
						tr.forced = true
					}
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
	// Edit list (edts/elst): the presentation timeline shift ffmpeg applies. An
	// empty edit adds a delay (movie timebase); a non-empty edit's media_time
	// trims the start (media timebase). Net shift = empty_delay - media_time.
	if edts, ok := findMemBox(trakBoxes, "edts"); ok {
		if eb, err := iterBoxes(edts.payload); err == nil {
			if elst, ok := findMemBox(eb, "elst"); ok {
				if mediaTime, emptyDur, ok := parseElst(elst.payload); ok {
					tr.editShiftMs = ticksToMs(emptyDur, movieTS) - ticksToMs(mediaTime, tr.timescale)
				}
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

	switch mode {
	case sampleKeyframes:
		// Only the keyframe index is needed: derive sync-sample times from
		// stss + stts/ctts, skipping the byte-offset resolution (stsz/stco/stsc)
		// that the full table builds.
		// endMs is computed for every track (the movie duration is the max across
		// tracks); only video collects the sync-sample keyframe times.
		isVideo := tr.trackType == mkv.VideoTrack
		kf, endMs, err := buildKeyframeTimes(stblBoxes, tr.timescale, tr.editShiftMs, isVideo)
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
	const audioHdr = 28

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

func parseAudioFields(tr *inTrack, payload []byte) {
	// AudioSampleEntry: reserved(8) channels(2) samplesize(2) pre(2) res(2) rate(4 fixed16.16)
	if len(payload) >= 28 {
		tr.channels = uint8(binary.BigEndian.Uint16(payload[16:18]))
		tr.sampleRate = float64(binary.BigEndian.Uint32(payload[24:28]) >> 16)
		parseBitrate(tr, payload, 28) // a btrt box may sit among the entry's children
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
