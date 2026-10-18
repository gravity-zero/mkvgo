package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"math/bits"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// dashRoleScheme is the schemeURI of the DASH role kind box (ISO/IEC 23009-1)
// that mainstream muxers use to record an MP4 track's "forced" disposition.
const dashRoleScheme = "urn:mpeg:dash:role:2011"

// parse.go - a minimal, defensive ISO-BMFF reader for the boxes RemuxFromMP4
// needs: ftyp is ignored, moov is parsed for track configuration and sample
// tables, and mdat is left in place (samples are read from it on demand by their
// recorded offsets). It parses untrusted input, so every length is bounds-checked
// against the data actually available and no allocation is sized by an attacker.

const (
	// maxSamples caps the samples a movie's tables and fragments declare, all tracks together (24 h of 60 fps
	// video and two 48 kHz AAC tracks is 13M): it bounds the index to about 1 GB whatever size a source claims.
	maxSamples = 1 << 24
)

// inSample is one parsed MP4 sample: where its bytes live in the file, its size,
// and its decode/composition times already converted to milliseconds.
type inSample struct {
	offset int64
	dtsMs  int64
	ctsMs  int64
	durMs  int64
	// ctsTicks is the composition time as the file states it: decode time plus
	// composition offset, in the track's timescale, edit list not applied.
	ctsTicks int64
	size     uint32
	sync     bool
	run      bool // the first sample of a chunk or trun: where the file's runs of samples start
}

// inTrack is a parsed MP4 track ready to be written as a Matroska track.
type inTrack struct {
	tableSamples     int64  // the samples its sample table declares, when parsed past the header
	trackID          uint32 // tkhd track_ID, for correlating mvex/trex and mfra/tfra
	trakIndex        int    // 0-based position of the trak among the moov's traks
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
	frameDurNs       int64   // constant frame duration (single-entry stts, or a video one rounded to the timescale); 0 when not constant
	rotation         int     // clockwise display rotation from the tkhd matrix (0/90/180/270)
	hflip            bool    // the tkhd matrix also mirrors the picture horizontally
	frameCount       int64   // sample count from stsz (the conventional nb_frames); 0 when unknown
	durationMs       int64   // per-track duration from mdhd; 0 when unknown
	presentMs        int64   // presentation duration from the edit list (its segments, movie timebase); 0 when none
	timescale        uint32
	// sttsLo/sttsHi are the smallest and largest sample duration an audio
	// track's stts declares, in timescale ticks (a lone final sample aside:
	// muxers close a track on whatever is left); 0 when there is no stts.
	sttsLo, sttsHi uint32
	samples        []inSample
	keyframesMs    []int64 // sync-sample presentation times (sampleKeyframes mode); nil otherwise
	sampleEndMs    int64   // last sample's cts (sampleKeyframes mode), for the movie duration
	sttsClipped    bool    // the stts states a negative delta: the samples' timeline does not give the duration
	unsupported    string  // why a remux cannot carry the track; "" when it can
	encryption     string  // the protected sample entry's scheme (schm); "" when the samples are clear
	bitDepth       uint8   // PCM bits per sample; 0 otherwise
	videoBitDepth  uint16  // the bit depth a video entry's fourcc states (ProRes); 0 when it states none
	pcmFrame       uint32  // PCM bytes per frame (one sample of every channel); 0 when not PCM
	fragDecodeEnd  int64   // where the track's last fragment ended (ticks): the next one's base when it has no tfdt

	// language and selection flags read from the track header / media header.
	language      string // ISO 639-2 from mdhd (e.g. "fre"); "" when absent/"und"
	languageBCP47 string // BCP-47 from an elng box; "" when absent
	name          string // track name from the QuickTime udta/name box; "" when absent
	handlerName   string // the hdlr name, kept apart from name: muxers mostly write a default there
	languageKnown bool   // a usable language was read (mdhd or elng)
	enabled       bool   // tkhd track_enabled flag (the conventional "default" disposition)
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
	tracks      []inTrack
	chapters    []mkv.Chapter
	dropped     []DroppedTrack
	durationMs  int64             // from mvhd, used when the sample table was not built
	tags        []mkv.SimpleTag   // file-level metadata from udta/meta/ilst
	title       string            // ©nam, for Info.Title
	cover       *mkv.Attachment   // covr cover art, carried as an MKV attachment
	hashes      map[uint32]string // freeform CONTENT_SHA256_<id> atoms, for VerifyContentHashes
	fragmented  bool              // an mvex box is present → sample data is in moof fragments
	mehdKnown   bool              // the mvex's mehd stated the fragments' total duration
	chapterTrak []byte            // the trak of a QuickTime chapter track (tref/chap), read when there is no chpl
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
		if size < int64(hdr) || size > int64(len(buf)-off) {
			return nil, errf("box %q has invalid size %d", typ, size)
		}
		out = append(out, memBox{typ: typ, payload: buf[off+hdr : off+int(size)]})
		off += int(size)
	}
	return out, nil
}

// genericHandlerName reports whether an hdlr name is a muxer's default rather than a track title: by rule, not by a list of names.
func genericHandlerName(s, handlerType string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f { // a control character, a leftover length byte
			return true
		}
	}
	l := strings.ToLower(strings.TrimSpace(s))
	switch {
	case l == strings.ToLower(handlerType): // the track type's fourcc: "soun", "vide"
		return true
	case strings.HasSuffix(l, "handler"), strings.HasSuffix(l, "handle"):
		return true
	case !strings.ContainsAny(l, " \t") && strings.ContainsAny(l, "@#"): // one token tagged with a tool: "srt@GPAC..."
		return true
	}
	switch l {
	case "video", "audio", "sound", "subtitle", "subtitles", "text", "timed text":
		return true
	}
	for _, tool := range []string{"gpac", "l-smash", "mainconcept", "core media", "ambarella", "iso media file produced by"} {
		if strings.Contains(l, tool) {
			return true
		}
	}
	return false
}

func findMemBox(boxes []memBox, typ string) (memBox, bool) {
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return memBox{}, false
}

// sampleMode selects how much per-sample work parseMP4 does:
//   - sampleNone: head-only, no per-sample table (the cheapest probe).
//   - sampleKeyframes: only the sync-sample presentation times (stss + stts/ctts),
//     for the metadata keyframe index - no byte offsets, so far cheaper than a
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
	return parseMP4Limit(r, size, mode, DefaultMaxMoovBytes)
}

// parseMP4Limit is parseMP4 refusing a moov past moovLimit bytes before reading it.
func parseMP4Limit(r io.ReadSeeker, size int64, mode sampleMode, moovLimit int64) (*movie, error) {
	moovPayload, err := readMoovForMode(r, size, mode, moovLimit)
	if err != nil {
		return nil, err
	}
	mv, err := parseMoov(moovPayload, size, mode)
	if err != nil {
		return nil, err
	}
	if len(mv.chapters) == 0 && mv.chapterTrak != nil {
		if samples := chapterTrackSamples(mv.chapterTrak, size); len(samples) > 0 {
			mv.chapters = readChapterTrack(r, samples)
		}
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
	if mode == sampleFull && mv.fragmented {
		// The sample tables are empty (samples live in the moof fragments); walk
		// them to build the full table so every downstream path reads a CMAF
		// source like a progressive one.
		moovBoxes, berr := iterBoxes(moovPayload)
		if berr != nil {
			return nil, berr
		}
		if err := readFragmentSamples(r, size, mv, moovBoxes); err != nil {
			return nil, errf("fragmented MP4: %w", err)
		}
	} else if mv.fragmented && !mv.mehdKnown {
		if moovBoxes, berr := iterBoxes(moovPayload); berr == nil {
			fragmentEnds(r, size, moovBoxes, mv)
		}
	}
	if mode == sampleFull {
		for i := range mv.tracks {
			coalescePCM(&mv.tracks[i])
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

// readMoovForMode returns the moov payload: the full box for the remux/extract
// path (which needs the complete sample tables), or the I/O-light lazy read
// (with a full-read fallback on any unexpected layout) for the metadata path.
func readMoovForMode(r io.ReadSeeker, size int64, mode sampleMode, limit int64) ([]byte, error) {
	if mode == sampleFull {
		return readMoovLimit(r, size, limit)
	}
	// The moov is looked for once: a file that has none is not scanned again
	// for the fallback.
	dataOff, payloadLen, err := findMoov(r, size)
	if err != nil {
		return nil, err
	}
	if err := checkMoovSize(payloadLen, limit); err != nil {
		return nil, err
	}
	if payload, err := readMoovLazyAt(r, dataOff, payloadLen); err == nil {
		return payload, nil
	}
	return readMoovAt(r, dataOff, payloadLen)
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
		if movieTS > 0 && headerDurationSet(durTicks) {
			mv.durationMs = headerTicksToMs(durTicks, movieTS)
		}
	}
	if movieTS == 0 {
		movieTS = 1000
	}
	// QuickTime chapter tracks are referenced from the media tracks via tref/chap;
	// their content (the chapter titles) is already read from chpl, so they are not
	// "lost" media and must not be surfaced as dropped tracks.
	chapterTrackIDs := chapterTrackRefs(moovBoxes)

	room := int64(maxSamples)
	trakIndex := -1
	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		trakIndex++
		tr, dropped, err := parseTrakRoom(b.payload, size, movieTS, mode, room)
		if err != nil {
			return nil, err
		}
		tr.trakIndex = trakIndex
		room -= tr.tableSamples
		if chapterTrackIDs[tr.trackID] && (dropped != nil || tr.unsupported != "") {
			if tb, err := iterBoxes(b.payload); err == nil && mv.chapterTrak == nil && (trakHandler(tb) == "text" || trakHandler(tb) == "sbtl") {
				mv.chapterTrak = b.payload // the titles are read from it when there is no chpl (never from a chapter-image track)
			}
			continue
		}
		if dropped != nil {
			mv.dropped = append(mv.dropped, *dropped)
			continue
		}
		mv.tracks = append(mv.tracks, tr)
	}
	if len(mv.tracks) == 0 && len(mv.dropped) == 0 {
		return nil, errf("no tracks found in MP4")
	}
	if mv.fragmented {
		if ms := mehdDurationMs(moovBoxes, movieTS); ms > 0 {
			mv.durationMs, mv.mehdKnown = max(mv.durationMs, ms), true
		}
		// The moov's sample tables are empty, so headerFrameRate gave 0. The
		// per-fragment default sample duration in mvex>trex yields the (CFR) frame
		// rate head-only - what probers report for a constant-rate fragmented stream.
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

// mehdDurationMs reads the fragments' total duration an mvex>mehd states (movie timescale); 0 when absent.
func mehdDurationMs(moovBoxes []memBox, movieTS uint32) int64 {
	mvex, ok := findMemBox(moovBoxes, "mvex")
	if !ok {
		return 0
	}
	boxes, err := iterBoxes(mvex.payload)
	if err != nil {
		return 0
	}
	mehd, ok := findMemBox(boxes, "mehd")
	if !ok || len(mehd.payload) < 8 {
		return 0
	}
	var ticks uint64
	if mehd.payload[0] == 1 && len(mehd.payload) >= 12 {
		ticks = binary.BigEndian.Uint64(mehd.payload[4:12])
	} else {
		ticks = uint64(binary.BigEndian.Uint32(mehd.payload[4:8]))
	}
	if ticks == 0 || ticks >= 1<<62 || ticks == 0xFFFFFFFF {
		return 0
	}
	return headerTicksToMs(ticks, movieTS)
}

// headerTicksToMs converts a header's duration to milliseconds without
// overflowing: a result past int64 is taken as unset (0).
func headerTicksToMs(ticks uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	hi, lo := bits.Mul64(ticks, 1000)
	if hi >= uint64(timescale) {
		return 0
	}
	q, _ := bits.Div64(hi, lo, uint64(timescale))
	if q > math.MaxInt64 {
		return 0
	}
	return int64(q)
}

// trexDefaultDurations reads mvex>trex and returns each track's
// default_sample_duration - the per-fragment default a CFR fragmented stream
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

// parseTrak parses one trak box. On success it returns the track and a nil
// *DroppedTrack. When the track has a recognised structure but cannot be carried
// - a non-media handler (hint/timecode/metadata) or an unsupported sample entry
// (e.g. cover art / attached picture) - it returns a zero track and a non-nil
// *DroppedTrack describing it, so the caller can surface it instead of dropping it
// silently. An error is returned only for malformed structure.
func parseTrak(payload []byte, fileSize int64, movieTS uint32, mode sampleMode) (inTrack, *DroppedTrack, error) {
	return parseTrakRoom(payload, fileSize, movieTS, mode, maxSamples)
}

// parseTrakRoom is parseTrak expanding the sample table only when it declares at most room samples.
func parseTrakRoom(payload []byte, fileSize int64, movieTS uint32, mode sampleMode, room int64) (inTrack, *DroppedTrack, error) {
	var tr inTrack
	trakBoxes, err := iterBoxes(payload)
	if err != nil {
		return tr, nil, err
	}
	// tkhd carries the selection flags and the track_ID. track_enabled (bit 0) is
	// what probers map to the "default" disposition, so it feeds Track.IsDefault on
	// the read side; the track_ID labels any DroppedTrack for correlation.
	var trackID uint64
	if tkhd, ok := findMemBox(trakBoxes, "tkhd"); ok {
		tr.enabled = tkhdEnabled(tkhd.payload)
		tr.flagsKnown = true
		tr.trackID = tkhdTrackID(tkhd.payload)
		trackID = uint64(tr.trackID)
		tr.rotation, tr.hflip = tkhdOrientation(tkhd.payload)
	}
	// MP4 has no native "forced" flag; mainstream muxers record it as a track-level kind box
	// with the DASH role scheme (e.g. value "forced-subtitle"), which its demuxer
	// reads back as AV_DISPOSITION_FORCED - regardless of the track's media type.
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
					// QuickTime track-name box (the way mainstream muxers write a per-track
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
		if entries, err := trakElstEntries(trakBoxes); err == nil && movieTS > 0 {
			var empty, media int64
			for _, e := range entries {
				if e.mediaTime < 0 {
					empty += e.segDur
				} else {
					media += e.segDur
				}
			}
			mediaMs := ticksToMs(media, movieTS)
			if tr.durationMs > 0 && mediaMs > tr.durationMs {
				mediaMs = tr.durationMs // edits presenting more than the media repeat it by mistake
			}
			tr.presentMs = ticksToMs(empty, movieTS) + mediaMs
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
	// (the conventional handler_name field) is the track's human-readable name.
	if len(hdlr.payload) > 24 {
		tr.handlerName = string(bytes.TrimRight(hdlr.payload[24:], "\x00"))
		if n := tr.handlerName; len(n) > 1 && int(n[0]) == len(n)-1 { // a QuickTime Pascal string: length byte first
			tr.handlerName = n[1:]
		}
		if tr.name == "" && !genericHandlerName(tr.handlerName, string(hdlr.payload[8:12])) {
			tr.name = tr.handlerName
		}
	}
	switch handler {
	case "vide":
		tr.trackType = mkv.VideoTrack
	case "soun":
		tr.trackType = mkv.AudioTrack
	case "text", "sbtl", "subp", "subt": // subt: TTML (stpp) and other XML subtitles
		tr.trackType = mkv.SubtitleTrack
	default:
		// hint/timecode/metadata - not media; surface it rather than dropping it.
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

	if stsd, ok := findMemBox(stblBoxes, "stsd"); !ok {
		tr.unsupported = "stbl without stsd: the track states no codec"
	} else {
		described, fourcc, err := parseSampleEntry(&tr, stsd.payload)
		if err != nil {
			return tr, nil, err
		}
		if !described {
			// A still picture (cover art, an attached picture in a jpeg/png/… video track) is no media track.
			return tr, &DroppedTrack{ID: trackID, Type: tr.trackType, Codec: fourcc,
				Reason: "unsupported sample entry " + quoteFourcc(fourcc)}, nil
		}
	}

	// The nominal frame rate (stts) and the frame count (stsz) are in the header, so
	// derive them head-only - no need to expand the sample table.
	if tr.trackType == mkv.VideoTrack {
		tr.frameRate = headerFrameRate(stblBoxes, tr.timescale)
		tr.frameDurNs = headerVideoFrameDurNs(stblBoxes, tr.timescale)
		tr.frameCount = headerFrameCount(stblBoxes)
		tr.firstSampleOffset, tr.firstSampleSize = firstSampleLoc(stblBoxes)
	}
	if tr.trackType == mkv.AudioTrack {
		// A single-entry stts is a strictly constant frame duration: exported as
		// the Matroska DefaultDuration, which downstream grid-times the audio
		// (sample-exact fMP4/HLS timing survives the millisecond timeline).
		tr.frameDurNs = headerConstantFrameDurNs(stblBoxes, tr.timescale)
		tr.sttsLo, tr.sttsHi = sttsDeltaRange(stblBoxes)
	}

	tr.sttsClipped = sttsHasNegativeDelta(stblBoxes)
	pcm, pcmPackets, err := readPCMTable(&tr, stblBoxes)
	if err != nil {
		pcmPackets = false // the sample-by-sample path reads the table, and reports what it cannot
	}
	n := headerFrameCount(stblBoxes)
	if pcmPackets {
		n = pcm.blocks() // one-frame PCM samples are held as the packets they gather into
	}
	if mode != sampleNone {
		if err := oneSampleDescription(stblBoxes); err != nil {
			tr.describedOnly(err)
			mode = sampleNone
		}
	}
	if mode != sampleNone && n > room {
		tr.describedOnly(errf("%d samples, past the %d a movie may hold with the tracks before it", n, max(room, 0)))
		mode = sampleNone
	} else if mode != sampleNone {
		tr.tableSamples = n
	}
	switch mode {
	case sampleKeyframes:
		// Only the keyframe index is needed: derive sync-sample times from
		// stss + stts/ctts, skipping the byte-offset resolution (stsz/stco/stsc)
		// that the full table builds.
		// endMs is computed for every track (the movie duration is the max across
		// tracks); only video collects the sync-sample keyframe times.
		isVideo := tr.trackType == mkv.VideoTrack
		if pcmPackets {
			tr.sampleEndMs = pcm.endMs(tr.timescale, tr.editShiftMs) // a PCM track's end, without expanding its table
			break
		}
		kf, endMs, err := buildKeyframeTimes(stblBoxes, tr.timescale, tr.editShiftMs, isVideo, fileSize)
		if err != nil {
			// A sample table the file cannot hold (a forged one, a moov cut from its media): the track is
			// described, and refused by name should a remux want its samples.
			tr.describedOnly(err)
			break
		}
		tr.sampleEndMs = endMs
		if isVideo {
			tr.keyframesMs = kf
		}
	case sampleFull:
		build := func() error { return buildSampleTable(&tr, stblBoxes, fileSize) }
		if pcmPackets {
			build = func() error { return buildPCMBlocks(&tr, pcm, fileSize) }
		}
		if err := build(); err != nil {
			tr.describedOnly(err)
			tr.samples = nil
		}
	}
	return tr, nil, nil
}

// describedOnly marks the track as one a remux cannot carry, for the reason given, keeping an earlier reason.
func (tr *inTrack) describedOnly(err error) {
	if tr.unsupported == "" {
		tr.unsupported = "sample table: " + err.Error()
	}
}
