package mp4

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

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
	trackType    mkv.TrackType
	codec        string // mkvgo short codec name (e.g. "h264", "aac")
	codecPrivate []byte
	width        uint32
	height       uint32
	channels     uint8
	sampleRate   float64
	timescale    uint32
	samples      []inSample

	// language and selection flags read from the track header / media header.
	language      string // ISO 639-2 from mdhd (e.g. "fre"); "" when absent/"und"
	languageBCP47 string // BCP-47 from an elng box; "" when absent
	languageKnown bool   // a usable language was read (mdhd or elng)
	enabled       bool   // tkhd track_enabled flag (ffprobe's "default" disposition)
	flagsKnown    bool   // a tkhd was parsed, so enabled is meaningful

	// colour code points (CICP), nil when the entry had no colr box.
	colorPrimaries *uint16
	colorTransfer  *uint16
	colorMatrix    *uint16
	colorRange     *uint16
}

// movie is the parsed result: the tracks RemuxFromMP4 will emit.
type movie struct {
	tracks   []inTrack
	chapters []mkv.Chapter
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
		switch {
		case size == 1:
			if off+16 > len(buf) {
				return nil, errf("truncated 64-bit box %q", typ)
			}
			size = int64(binary.BigEndian.Uint64(buf[off+8 : off+16]))
			hdr = 16
		case size == 0:
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

// readMoov scans the top-level boxes of a seekable MP4 and returns the moov box
// payload. moov may appear before or after mdat, so the whole file is scanned by
// header until it is found.
func readMoov(r io.ReadSeeker, size int64) ([]byte, error) {
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return nil, errf("read box header: %w", err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch {
		case boxSize == 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return nil, errf("read largesize: %w", err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case boxSize == 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			return nil, errf("box %q has invalid size %d at offset %d", typ, boxSize, off)
		}
		if typ == "moov" {
			payloadLen := boxSize - headerLen
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				return nil, errf("read moov: %w", err)
			}
			return payload, nil
		}
		off += boxSize
	}
	return nil, errf("no moov box found")
}

// parseMP4 reads and parses the movie header of a seekable MP4 of the given size.
func parseMP4(r io.ReadSeeker, size int64) (*movie, error) {
	moovPayload, err := readMoov(r, size)
	if err != nil {
		return nil, err
	}
	moovBoxes, err := iterBoxes(moovPayload)
	if err != nil {
		return nil, errf("parse moov: %w", err)
	}

	var mv movie
	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		tr, ok, err := parseTrak(b.payload, size)
		if err != nil {
			return nil, err
		}
		if ok {
			mv.tracks = append(mv.tracks, tr)
		}
	}
	if len(mv.tracks) == 0 {
		return nil, errf("no convertible tracks found in MP4")
	}
	if udta, ok := findMemBox(moovBoxes, "udta"); ok {
		if ub, err := iterBoxes(udta.payload); err == nil {
			if chpl, ok := findMemBox(ub, "chpl"); ok {
				mv.chapters = parseChpl(chpl.payload)
			}
		}
	}
	return &mv, nil
}

// parseTrak parses one trak box. ok is false (with nil error) when the track's
// codec is recognised as out of scope and should be skipped silently; an error
// is returned only for malformed structure.
func parseTrak(payload []byte, fileSize int64) (inTrack, bool, error) {
	var tr inTrack
	trakBoxes, err := iterBoxes(payload)
	if err != nil {
		return tr, false, err
	}
	// tkhd carries the selection flags. track_enabled (bit 0) is what ffmpeg maps
	// to the "default" disposition, so it feeds Track.IsDefault on the read side.
	if tkhd, ok := findMemBox(trakBoxes, "tkhd"); ok {
		tr.enabled = tkhdEnabled(tkhd.payload)
		tr.flagsKnown = true
	}

	mdia, ok := findMemBox(trakBoxes, "mdia")
	if !ok {
		return tr, false, errf("trak without mdia")
	}
	mdiaBoxes, err := iterBoxes(mdia.payload)
	if err != nil {
		return tr, false, err
	}

	if mdhd, ok := findMemBox(mdiaBoxes, "mdhd"); ok {
		var lang string
		tr.timescale, lang = parseMdhd(mdhd.payload)
		if lang != "" {
			tr.language = lang
			tr.languageKnown = true
		}
	}
	if tr.timescale == 0 {
		tr.timescale = 1000
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
		return tr, false, errf("mdia without hdlr")
	}
	if len(hdlr.payload) < 12 {
		return tr, false, errf("hdlr too short")
	}
	switch string(hdlr.payload[8:12]) {
	case "vide":
		tr.trackType = mkv.VideoTrack
	case "soun":
		tr.trackType = mkv.AudioTrack
	case "text", "sbtl":
		tr.trackType = mkv.SubtitleTrack
	default:
		return tr, false, nil // hint/meta/etc — skip
	}

	minf, ok := findMemBox(mdiaBoxes, "minf")
	if !ok {
		return tr, false, errf("mdia without minf")
	}
	minfBoxes, err := iterBoxes(minf.payload)
	if err != nil {
		return tr, false, err
	}
	stbl, ok := findMemBox(minfBoxes, "stbl")
	if !ok {
		return tr, false, errf("minf without stbl")
	}
	stblBoxes, err := iterBoxes(stbl.payload)
	if err != nil {
		return tr, false, err
	}

	stsd, ok := findMemBox(stblBoxes, "stsd")
	if !ok {
		return tr, false, errf("stbl without stsd")
	}
	if codecOK, err := parseSampleEntry(&tr, stsd.payload); err != nil {
		return tr, false, err
	} else if !codecOK {
		return tr, false, nil // unsupported codec — skip the track
	}

	if err := buildSampleTable(&tr, stblBoxes, fileSize); err != nil {
		return tr, false, err
	}
	return tr, true, nil
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

// decodeMdhdLanguage unpacks the mdhd language field: three 5-bit values, each
// offset by 0x60, forming a lowercase ISO 639-2/T code. It returns "" for the
// zero field and for "und" (undefined), and for any value that is not three
// a–z letters.
func decodeMdhdLanguage(packed uint16) string {
	if packed == 0 {
		return ""
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

// tkhdEnabled reports whether a track header's track_enabled flag (bit 0 of the
// 24-bit flags) is set. ffmpeg maps this to the "default" stream disposition.
func tkhdEnabled(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	return flags&0x000001 != 0
}

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

// parseSampleEntry reads the first sample entry from an stsd box and fills the
// track's codec, codec private data and dimensions. It returns ok=false for a
// recognised-but-unsupported codec.
func parseSampleEntry(tr *inTrack, stsdPayload []byte) (bool, error) {
	if len(stsdPayload) < 8 {
		return false, errf("stsd too short")
	}
	// fullbox header(4) + entry_count(4), then the sample entry box.
	entries, err := iterBoxes(stsdPayload[8:])
	if err != nil {
		return false, errf("parse stsd: %w", err)
	}
	if len(entries) == 0 {
		return false, errf("stsd has no sample entry")
	}
	entry := entries[0]

	const visualHdr = 78
	const audioHdr = 28

	switch entry.typ {
	case "avc1", "avc3":
		tr.codec = "h264"
		return true, extractVisual(tr, entry.payload, visualHdr, "avcC")
	case "hvc1", "hev1":
		tr.codec = "hevc"
		return true, extractVisual(tr, entry.payload, visualHdr, "hvcC")
	case "av01":
		tr.codec = "av1"
		return true, extractVisual(tr, entry.payload, visualHdr, "av1C")
	case "mp4a":
		return parseMP4A(tr, entry.payload, audioHdr)
	case "Opus":
		tr.codec = "opus"
		return true, extractOpus(tr, entry.payload, audioHdr)
	case "ac-3":
		tr.codec = "ac3"
		parseAudioFields(tr, entry.payload)
		return true, nil
	case "ec-3":
		tr.codec = "eac3"
		parseAudioFields(tr, entry.payload)
		return true, nil
	case "fLaC":
		tr.codec = "flac"
		return true, extractFLAC(tr, entry.payload, audioHdr)
	case "tx3g":
		tr.codec = "srt" // tx3g timed text → S_TEXT/UTF8
		return true, nil
	case "wvtt":
		tr.codec = "webvtt" // native WebVTT → S_TEXT/WEBVTT
		extractWVTTConfig(tr, entry.payload)
		return true, nil
	default:
		return false, nil
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
	objType, asc, err := parseESDS(esds)
	if err != nil {
		return false, err
	}
	switch objType {
	case 0x40, 0x66, 0x67: // MPEG-4/2 AAC
		if len(asc) == 0 {
			return false, errf("mp4a/AAC without AudioSpecificConfig")
		}
		tr.codec = "aac"
		tr.codecPrivate = asc
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
	return nil
}

// parseColr reads a colr box (nclx type) from a visual sample entry and records
// the colour code points. Other colr types (e.g. 'nclc', 'rICC') are ignored.
func parseColr(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	colr, ok := findMemBox(children, "colr")
	if !ok || len(colr.payload) < 11 || string(colr.payload[:4]) != "nclx" {
		return
	}
	p := binary.BigEndian.Uint16(colr.payload[4:6])
	tr.colorPrimaries = &p
	tc := binary.BigEndian.Uint16(colr.payload[6:8])
	tr.colorTransfer = &tc
	m := binary.BigEndian.Uint16(colr.payload[8:10])
	tr.colorMatrix = &m
	rng := uint16(1) // limited
	if colr.payload[10]&0x80 != 0 {
		rng = 2 // full
	}
	tr.colorRange = &rng
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
	}
}

// parseESDS walks an esds box payload's MPEG-4 descriptor tree and returns the
// object type indication and the DecoderSpecificInfo (the AudioSpecificConfig
// for AAC; absent for MP3, in which case asc is nil).
func parseESDS(esds []byte) (objType byte, asc []byte, err error) {
	if len(esds) < 4 {
		return 0, nil, errf("esds too short")
	}
	d := &descReader{buf: esds[4:]} // skip fullbox version/flags
	tag, body, err := d.next()
	if err != nil || tag != 0x03 {
		return 0, nil, errf("esds: expected ES_Descriptor")
	}
	// ES_Descriptor: ES_ID(2) + flags(1) + optional fields.
	es := &descReader{buf: body}
	if err := es.skip(3); err != nil {
		return 0, nil, err
	}
	flags := body[2]
	if flags&0x80 != 0 { // dependsOn_ES_ID
		if err := es.skip(2); err != nil {
			return 0, nil, err
		}
	}
	if flags&0x40 != 0 { // URL
		if es.pos >= len(es.buf) {
			return 0, nil, errf("esds: truncated URL length")
		}
		urlLen := int(es.buf[es.pos])
		if err := es.skip(1 + urlLen); err != nil {
			return 0, nil, err
		}
	}
	if flags&0x20 != 0 { // OCR
		if err := es.skip(2); err != nil {
			return 0, nil, err
		}
	}
	tag, dcfg, err := es.next()
	if err != nil || tag != 0x04 {
		return 0, nil, errf("esds: expected DecoderConfigDescriptor")
	}
	if len(dcfg) < 1 {
		return 0, nil, errf("esds: empty DecoderConfigDescriptor")
	}
	objType = dcfg[0]
	// DecoderConfigDescriptor header: objectType(1) streamType(1) buffer(3)
	// max(4) avg(4) = 13 bytes, optionally followed by a DecoderSpecificInfo.
	dc := &descReader{buf: dcfg}
	if err := dc.skip(13); err != nil {
		return objType, nil, nil // no DecoderSpecificInfo (e.g. MP3)
	}
	tag, dsi, err := dc.next()
	if err != nil || tag != 0x05 {
		return objType, nil, nil
	}
	return objType, append([]byte(nil), dsi...), nil
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
