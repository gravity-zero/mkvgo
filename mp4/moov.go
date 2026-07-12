package mp4

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// moov.go - assembles the movie box (moov) and its sub-tree from the sample
// tables collected during the mdat pass. All timing is in the movie timescale
// (milliseconds), which is lossless for the default Matroska TimecodeScale of
// 1 ms; finer Matroska scales are rounded by the block reader before reaching
// this layer.

const movieTimescale = 1000

// buildFtyp builds the file type box. major_brand isom keeps the file broadly
// compatible; brands lists the codec-specific brands to advertise.
func buildFtyp(brands []string) []byte {
	compatible := dedupeBrands(append([]string{"isom", "iso2"}, append(brands, "mp41")...))
	return boxf("ftyp", func(w *bw) {
		w.fourcc("isom")
		w.u32(512) // minor_version
		for _, b := range compatible {
			w.fourcc(b)
		}
	})
}

func dedupeBrands(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, b := range in {
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	return out
}

// buildMoov assembles the complete moov box. Tracks with no samples are skipped.
// mdatBase is the absolute file offset of the mdat payload (added to the stored
// relative chunk offsets); co64 selects 64-bit chunk offsets.
func buildMoov(tracks []*outTrack, mdatBase int64, co64 bool, meta movieMeta) []byte {
	var (
		traks    [][]byte
		movieDur uint32
		maxID    uint32
	)
	for _, t := range tracks {
		if len(t.samples.samples) == 0 {
			continue
		}
		trak, dur := buildTrak(t, mdatBase, co64)
		traks = append(traks, trak)
		if dur > movieDur {
			movieDur = dur
		}
		if t.mp4ID > maxID {
			maxID = t.mp4ID
		}
	}
	children := make([][]byte, 0, len(traks)+2)
	children = append(children, buildMvhd(movieDur, maxID+1))
	children = append(children, traks...)
	// One moov-level udta carrying the movie title (iTunes meta/ilst, what mainstream muxers
	// write and probers read as the format "title") and the chapter list.
	var udtaKids [][]byte
	if mb := buildMovieMeta(meta.title, meta.tags, meta.cover, meta.hashes); mb != nil {
		udtaKids = append(udtaKids, mb)
	}
	if chpl := buildChplBox(meta.chapters); chpl != nil {
		udtaKids = append(udtaKids, chpl)
	}
	if len(udtaKids) > 0 {
		children = append(children, container("udta", udtaKids...))
	}
	return container("moov", children...)
}

// mp4MetaAtoms maps mkvgo/Matroska global tag names to the iTunes ilst atom that
// carries them in MP4 - the inverse of parse.go's metaAtomNames. TITLE is omitted:
// the movie title is written as ©nam from Info.Title (see buildMovieMeta).
var mp4MetaAtoms = map[string]string{
	"ARTIST":        "\xa9ART",
	"ALBUM":         "\xa9alb",
	"DATE_RELEASED": "\xa9day",
	"GENRE":         "\xa9gen",
	"COMMENT":       "\xa9cmt",
	"ENCODER":       "\xa9too",
	"COMPOSER":      "\xa9wrt",
	"DESCRIPTION":   "desc",
}

// coverArt is an image attachment carried as the iTunes covr ilst atom.
type coverArt struct {
	data []byte
	png  bool // selects the data box's well-known type: 14 (PNG) vs 13 (JPEG)
}

// pickCoverArt selects the source attachment to carry as MP4 cover art: the
// first JPEG/PNG image attachment, preferring one whose name starts with
// "cover" (the Matroska cover-art convention). Nil when there is none.
func pickCoverArt(atts []mkv.Attachment) *coverArt {
	var found *coverArt
	for _, a := range atts {
		var png bool
		switch a.MIMEType {
		case "image/jpeg":
		case "image/png":
			png = true
		default:
			continue
		}
		if len(a.Data) == 0 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(a.Name), "cover") {
			return &coverArt{data: a.Data, png: png}
		}
		if found == nil {
			found = &coverArt{data: a.Data, png: png}
		}
	}
	return found
}

// buildMovieMeta builds the iTunes-style metadata box carrying the movie title
// (©nam, what probers report as the format "title"), the other global tags as
// ilst atoms (ARTIST→©ART, ALBUM→©alb, …) and the cover art (covr), exactly as
// mainstream muxers write them and as from-mp4 reads them back. Returns nil when there is
// nothing to write.
func buildMovieMeta(title string, tags []mkv.SimpleTag, cover *coverArt, hashes map[uint32]string) []byte {
	type atom struct{ typ, val string }
	var atoms []atom
	seen := map[string]bool{}
	add := func(typ, val string) {
		if val == "" || seen[typ] {
			return
		}
		seen[typ] = true
		atoms = append(atoms, atom{typ, val})
	}
	add("\xa9nam", title)
	for _, tg := range tags {
		if a, ok := mp4MetaAtoms[tg.Name]; ok {
			add(a, tg.Value)
		}
	}
	if len(atoms) == 0 && cover == nil && len(hashes) == 0 {
		return nil
	}
	ilstChildren := make([][]byte, 0, len(atoms)+1)
	for _, a := range atoms {
		val := a.val
		data := fullBox("data", 0, 1, func(w *bw) { // flags = 1 → UTF-8 text
			w.u32(0) // locale
			w.bytes([]byte(val))
		})
		ilstChildren = append(ilstChildren, container(a.typ, data))
	}
	if cover != nil {
		typ := uint32(13) // JPEG
		if cover.png {
			typ = 14
		}
		data := fullBox("data", 0, typ, func(w *bw) {
			w.u32(0) // locale
			w.bytes(cover.data)
		})
		ilstChildren = append(ilstChildren, container("covr", data))
	}
	// Per-track content hashes as freeform atoms, in track order so the moov
	// bytes are deterministic.
	if len(hashes) > 0 {
		ids := make([]uint32, 0, len(hashes))
		for id := range hashes {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		for _, id := range ids {
			ilstChildren = append(ilstChildren, freeformAtom(
				fmt.Sprintf("CONTENT_SHA256_%d", id), hashes[id]))
		}
	}
	ilst := container("ilst", ilstChildren...)
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) {
		w.u32(0)         // pre_defined
		w.fourcc("mdir") // handler_type: metadata
		w.fourcc("appl") // reserved (Apple)
		w.zeros(8)
		w.u8(0) // null name
	})
	return fullBox("meta", 0, 0, func(w *bw) {
		w.bytes(hdlr)
		w.bytes(ilst)
	})
}

// freeformAtom builds an iTunes freeform ilst atom ("----" with mean/name/data),
// the extension point for tags outside the fixed iTunes vocabulary.
func freeformAtom(name, value string) []byte {
	mean := fullBox("mean", 0, 0, func(w *bw) { w.bytes([]byte("org.mkvgo")) })
	nm := fullBox("name", 0, 0, func(w *bw) { w.bytes([]byte(name)) })
	data := fullBox("data", 0, 1, func(w *bw) { // type 1 = UTF-8 text
		w.u32(0) // locale
		w.bytes([]byte(value))
	})
	return container("----", mean, nm, data)
}

// buildTrackName builds the QuickTime udta/name box carrying a track's name (the
// Matroska TrackEntry Name) - the form mainstream muxers write for a per-track title. The box
// payload is the raw UTF-8 string. Returns nil for an empty name.
func buildTrackName(name string) []byte {
	if name == "" {
		return nil
	}
	return boxf("name", func(w *bw) { w.bytes([]byte(name)) })
}

func buildMvhd(durationMs, nextTrackID uint32) []byte {
	return fullBox("mvhd", 0, 0, func(w *bw) {
		w.u32(0) // creation_time
		w.u32(0) // modification_time
		w.u32(movieTimescale)
		w.u32(durationMs)
		w.u32(0x00010000) // rate 1.0
		w.u16(0x0100)     // volume 1.0
		w.u16(0)          // reserved
		w.zeros(8)        // reserved
		w.matrix(unityMatrix)
		w.zeros(24) // pre_defined
		w.u32(nextTrackID)
	})
}

// hasContainerPriming reports whether mkvgo should carry a codec's encoder/decoder
// delay across the MP4 <-> MKV round trip via Matroska CodecDelay and an MP4 edit
// list. The criterion is "the delay is container-signalled (not intrinsic to the
// codec config) and mainstream demuxers reproduce it from a sample-exact edit list":
//   - AAC: encoder delay lives only in the MP4 edit list, lost otherwise.
//   - AC-3, E-AC-3: a fixed decoder delay (256 samples) the source trims via the
//     edit list. mainstream demuxers only trim it when the edit list is sample-exact, which is
//     why audio tracks are written on a sample-rate media timescale (mediaTimescale)
//     rather than the millisecond movie timescale.
//   - Opus, Vorbis, MP3: the delay is intrinsic to the bitstream (Opus pre-skip in
//     the OpusHead, MP3 encoder delay in the in-band Xing/LAME header) and mainstream demuxers'
//     decoder applies it regardless of the container. A derived edit list ADDS to
//     that, over-trimming and desyncing the head - a native-MKV MP3 lost ~20 ms of
//     real audio at the start before this. mainstream muxers likewise write no useful edit
//     list when copying a native MP3/Opus to MP4.
//   - FLAC/DTS/PCM: no encoder priming.
func hasContainerPriming(codec string) bool {
	switch codec {
	case "aac", "ac3", "eac3":
		return true
	}
	return false
}

// mediaTimescale returns the mdia/mdhd timescale for a track. Audio tracks use their
// sample rate (as mainstream muxers do), making the sample table and the CodecDelay-derived
// edit list sample-exact - required for a demuxer to trim a codec's priming precisely
// (notably AC-3, whose decoder delay it ignores from a millisecond-quantised edit
// list). Everything else uses the movie timescale.
func mediaTimescale(t *outTrack) uint32 {
	if t.spec.handler == "soun" && t.mkv.SampleRate != nil && *t.mkv.SampleRate > 0 {
		return uint32(*t.mkv.SampleRate)
	}
	return movieTimescale
}

// buildEdts writes a track's edit list. Up to two entries, the way mainstream muxers write
// them: an optional leading empty edit (media_time -1) carrying a presentation
// offset - the A/V sync gap, since the sample table is rebased to 0 - followed by
// the media edit, whose media_time re-signals an audio track's gapless priming
// (Matroska CodecDelay) as the MP4 encoder delay so a decoder discards it.
func buildEdts(codecDelayNs, offsetMovieMs int64, durMovieMs, mts uint32) []byte {
	// media_time is in the media timescale (mts == sample rate for audio), so the
	// trim is sample-exact; segment_duration is in the movie timescale (ms). Round to
	// nearest so an N-sample priming comes back as exactly N samples, not N-1.
	mediaTime := (codecDelayNs*int64(mts) + 500_000_000) / 1_000_000_000
	segDur := int64(durMovieMs) - codecDelayNs/1_000_000
	if segDur < 0 {
		segDur = 0
	}
	type entry struct {
		segDur    uint32
		mediaTime int32
	}
	var entries []entry
	if offsetMovieMs > 0 {
		entries = append(entries, entry{uint32(offsetMovieMs), -1}) // empty edit: the offset
	}
	entries = append(entries, entry{uint32(segDur), int32(mediaTime)}) // media edit
	elst := fullBox("elst", 0, 0, func(w *bw) {
		w.u32(uint32(len(entries)))
		for _, e := range entries {
			w.u32(e.segDur)    // segment_duration (movie timescale)
			w.i32(e.mediaTime) // media_time (-1 = empty; else media timescale)
			w.u16(1)           // media_rate integer (1.0)
			w.u16(0)           // media_rate fraction
		}
	})
	return container("edts", elst)
}

func buildTrak(t *outTrack, mdatBase int64, co64 bool) ([]byte, uint32) {
	// Audio tracks use their sample rate as the media timescale so the sample table
	// and the CodecDelay-derived edit list are sample-exact (see mediaTimescale);
	// text/video stay on the movie timescale. tim.total is then in the media
	// timescale, while tkhd/mvhd and the edit list's segment_duration need the movie
	// timescale (ms) - durMovie.
	mts := mediaTimescale(t)
	var tim timing
	if t.spec.text || t.isChapter {
		mts = movieTimescale
		tim = textTiming(t.samples.samples)
	} else {
		tim = reconstructTiming(t.samples.samples, t.frameDurMs, mts, audioGridTS(t, mts))
	}
	durMedia := uint32(tim.total)
	durMovie := durMedia
	if mts != movieTimescale {
		durMovie = uint32(tim.total * int64(movieTimescale) / int64(mts))
	}
	// Presentation offset: the smallest sample PTS is where the track starts on the
	// movie timeline (the A/V sync gap mainstream muxers write as an empty edit). The sample
	// table is rebased to 0, so without re-emitting this the offset is lost and the
	// tracks desync. The track's presentation span is the offset plus its media.
	var offsetMs int64
	if n := len(t.samples.samples); n > 0 {
		offsetMs = t.samples.samples[0].pts
		for _, s := range t.samples.samples {
			if s.pts < offsetMs {
				offsetMs = s.pts
			}
		}
	}
	presentDur := durMovie + uint32(offsetMs)

	var mediaHeader []byte
	switch {
	case t.isChapter:
		mediaHeader = buildGmhd()
	case t.spec.video:
		mediaHeader = vmhd()
	case t.spec.text:
		mediaHeader = nmhd()
	default:
		mediaHeader = smhd()
	}
	minf := container("minf", mediaHeader, buildDinf(), buildStbl(t, tim, mdatBase, co64))
	mdiaChildren := [][]byte{buildMdhd(durMedia, mts, mdhdLanguage(t.mkv))}
	// An elng box carries the BCP-47 language tag (mdhd holds only the legacy
	// ISO 639-2 code), so a full language round-trips through the remux.
	if t.mkv.LanguageBCP47 != "" {
		mdiaChildren = append(mdiaChildren, buildElng(t.mkv.LanguageBCP47))
	}
	// The hdlr name is the human-readable track description QuickTime and probers surface
	// (the conventional handler_name); carry the Matroska track Name there so it is visible,
	// falling back to the generic handler name.
	hName := handlerName(t.spec.handler)
	if t.mkv.Name != "" {
		hName = t.mkv.Name
	}
	mdiaChildren = append(mdiaChildren, buildHdlr(t.spec.handler, hName), minf)
	mdia := container("mdia", mdiaChildren...)

	trakChildren := [][]byte{buildTkhd(t, presentDur)}
	if t.chapterRefID > 0 {
		trakChildren = append(trakChildren, buildTrefChap(t.chapterRefID))
	}
	// One track-level udta: the track name (QuickTime name box, the way mainstream muxers write
	// a per-track title) and - MP4 having no native forced flag - the forced marker as
	// a kind box with the DASH role scheme, the way mainstream muxers record it.
	var trakUdta [][]byte
	if nm := buildTrackName(t.mkv.Name); nm != nil {
		trakUdta = append(trakUdta, nm)
	}
	if t.mkv.IsForced {
		trakUdta = append(trakUdta, buildKind(dashRoleScheme, "forced-subtitle"))
	}
	if len(trakUdta) > 0 {
		trakChildren = append(trakChildren, container("udta", trakUdta...))
	}
	// Emit an edit list for a presentation offset (A/V sync) and/or to re-signal the
	// gapless priming (CodecDelay) so a decoder discards it across the round trip.
	// Priming is limited to codecs the CodecDelay path reproduces (hasContainerPriming).
	codecDelay := int64(0)
	if wantsEditList(t.mkv.Codec, t.mp3Delay) {
		codecDelay = t.mkv.CodecDelay
	}
	if offsetMs > 0 || codecDelay > 0 {
		trakChildren = append(trakChildren, buildEdts(codecDelay, offsetMs, durMovie, mts))
	}
	trakChildren = append(trakChildren, mdia)
	return container("trak", trakChildren...), presentDur
}

// buildElng builds an Extended Language Tag box (ISO/IEC 14496-12) carrying a
// null-terminated BCP-47 language tag.
func buildElng(bcp47 string) []byte {
	return fullBox("elng", 0, 0, func(w *bw) {
		w.bytes([]byte(bcp47))
		w.u8(0)
	})
}

// buildKind builds a kind box: a fullbox with a null-terminated schemeURI followed
// by a null-terminated value.
func buildKind(scheme, value string) []byte {
	return fullBox("kind", 0, 0, func(w *bw) {
		w.bytes([]byte(scheme))
		w.u8(0)
		w.bytes([]byte(value))
		w.u8(0)
	})
}

func buildTkhd(t *outTrack, durationMs uint32) []byte {
	// in_movie | in_preview, plus track_enabled when the track is the default  -
	// probers map track_enabled back to the "default" disposition.
	flags := uint32(0x000006)
	if t.mkv.IsDefault {
		flags |= 0x000001
	}
	var volume uint16
	if t.spec.handler == "soun" {
		volume = 0x0100
	}
	return fullBox("tkhd", 0, flags, func(w *bw) {
		w.u32(0) // creation_time
		w.u32(0) // modification_time
		w.u32(t.mp4ID)
		w.u32(0) // reserved
		w.u32(durationMs)
		w.zeros(8) // reserved
		w.u16(0)   // layer
		w.u16(0)   // alternate_group
		w.u16(volume)
		w.u16(0) // reserved
		w.matrix(unityMatrix)
		if t.spec.video {
			w.u32(fixed16_16(derefU32(t.mkv.Width)))
			w.u32(fixed16_16(derefU32(t.mkv.Height)))
		} else {
			w.u32(0)
			w.u32(0)
		}
	})
}

func buildMdhd(duration, timescale uint32, lang string) []byte {
	return fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0) // creation_time
		w.u32(0) // modification_time
		w.u32(timescale)
		w.u32(duration)
		w.u16(packLanguage(lang))
		w.u16(0) // pre_defined
	})
}

func buildHdlr(handlerType, name string) []byte {
	return fullBox("hdlr", 0, 0, func(w *bw) {
		w.u32(0) // pre_defined
		w.fourcc(handlerType)
		w.zeros(12) // reserved
		w.bytes([]byte(name))
		w.u8(0) // null terminator
	})
}

func buildDinf() []byte {
	url := fullBox("url ", 0, 0x000001, func(w *bw) {}) // self-contained, no location
	dref := fullBox("dref", 0, 0, func(w *bw) {
		w.u32(1) // entry_count
		w.bytes(url)
	})
	return container("dinf", dref)
}

func vmhd() []byte {
	return fullBox("vmhd", 0, 0x000001, func(w *bw) {
		w.u16(0)   // graphicsmode
		w.zeros(6) // opcolor[3]
	})
}

func smhd() []byte {
	return fullBox("smhd", 0, 0, func(w *bw) {
		w.u16(0) // balance
		w.u16(0) // reserved
	})
}

// nmhd is the Null Media Header, used by timed-text (tx3g) tracks.
func nmhd() []byte {
	return fullBox("nmhd", 0, 0, func(w *bw) {})
}

func buildStbl(t *outTrack, tim timing, mdatBase int64, co64 bool) []byte {
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1) // entry_count
		w.bytes(t.sampleEntry)
	})
	children := [][]byte{stsd, buildSTTS(tim.durations)}
	if stss := buildSTSS(t.samples.samples); stss != nil {
		children = append(children, stss)
	}
	if ctts := buildCTTS(tim); ctts != nil {
		children = append(children, ctts)
	}
	children = append(children,
		buildSTSC(t.samples.chunks),
		buildSTSZ(t.samples.samples),
		buildChunkOffsets(t.samples.chunks, mdatBase, co64),
	)
	return container("stbl", children...)
}

func handlerName(handlerType string) string {
	switch handlerType {
	case "vide":
		return "VideoHandler"
	case "text":
		return "SubtitleHandler"
	default:
		return "SoundHandler"
	}
}

// mdhdLanguage picks the 3-letter ISO-639-2 code for mdhd. Matroska's legacy
// Language element already uses that vocabulary; the BCP-47 element does not, so
// it is not used here (packLanguage falls back to "und" for anything else).
func mdhdLanguage(t mkv.Track) string {
	if len(t.Language) == 3 {
		return t.Language
	}
	return "und"
}
