package mp4

import "github.com/gravity-zero/mkvgo/mkv"

// moov.go — assembles the movie box (moov) and its sub-tree from the sample
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
func buildMoov(tracks []*outTrack, mdatBase int64, co64 bool, chapters []mkv.Chapter) []byte {
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
	if udta := buildChapterUdta(chapters); udta != nil {
		children = append(children, udta)
	}
	return container("moov", children...)
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
// codec config) and ffmpeg reproduces it from a sample-exact edit list":
//   - AAC, MP3: encoder/decoder delay lives in the MP4 edit list, lost otherwise.
//   - AC-3, E-AC-3: a fixed decoder delay (256 samples) the source trims via the
//     edit list. ffmpeg only trims it when the edit list is sample-exact, which is
//     why audio tracks are written on a sample-rate media timescale (mediaTimescale)
//     rather than the millisecond movie timescale.
//   - Opus, Vorbis: pre-skip is intrinsic to the codec config (OpusHead / setup
//     headers), copied verbatim -> a derived CodecDelay would double-count it.
//   - FLAC/DTS/PCM: no encoder priming.
func hasContainerPriming(codec string) bool {
	switch codec {
	// "A_MPEG/L3" is mkvgo's name for MP3 on both sides (the reader leaves the raw
	// Matroska CodecID unmapped, and from-mp4 tags the inTrack with the same string).
	case "aac", "ac3", "eac3", "A_MPEG/L3":
		return true
	}
	return false
}

// mediaTimescale returns the mdia/mdhd timescale for a track. Audio tracks use their
// sample rate (as ffmpeg does), making the sample table and the CodecDelay-derived
// edit list sample-exact — required for ffmpeg to trim a codec's priming precisely
// (notably AC-3, whose decoder delay it ignores from a millisecond-quantised edit
// list). Everything else uses the movie timescale.
func mediaTimescale(t *outTrack) uint32 {
	if t.spec.handler == "soun" && t.mkv.SampleRate != nil && *t.mkv.SampleRate > 0 {
		return uint32(*t.mkv.SampleRate)
	}
	return movieTimescale
}

// buildEdts writes an edit list that re-signals an audio track's gapless priming
// (Matroska CodecDelay, in ns) as the MP4 encoder delay: one edit starting at
// media_time = priming, so a decoder discards it. This is what carries the priming
// back across an MKV->MP4 round-trip, the way ffmpeg writes it.
func buildEdts(codecDelayNs int64, durMovieMs, mts uint32) []byte {
	// media_time is in the media timescale (mts == sample rate for audio), so the
	// trim is sample-exact; segment_duration is in the movie timescale (ms). Round to
	// nearest so an N-sample priming comes back as exactly N samples, not N-1.
	mediaTime := (codecDelayNs*int64(mts) + 500_000_000) / 1_000_000_000
	segDur := int64(durMovieMs) - codecDelayNs/1_000_000
	if segDur < 0 {
		segDur = 0
	}
	elst := fullBox("elst", 0, 0, func(w *bw) {
		w.u32(1)                // entry_count
		w.u32(uint32(segDur))   // segment_duration (movie timescale)
		w.i32(int32(mediaTime)) // media_time (media timescale)
		w.u16(1)                // media_rate integer (1.0)
		w.u16(0)                // media_rate fraction
	})
	return container("edts", elst)
}

func buildTrak(t *outTrack, mdatBase int64, co64 bool) ([]byte, uint32) {
	// Audio tracks use their sample rate as the media timescale so the sample table
	// and the CodecDelay-derived edit list are sample-exact (see mediaTimescale);
	// text/video stay on the movie timescale. tim.total is then in the media
	// timescale, while tkhd/mvhd and the edit list's segment_duration need the movie
	// timescale (ms) — durMovie.
	mts := mediaTimescale(t)
	var tim timing
	if t.spec.text || t.isChapter {
		mts = movieTimescale
		tim = textTiming(t.samples.samples)
	} else {
		tim = reconstructTiming(t.samples.samples, t.frameDurMs, mts)
	}
	durMedia := uint32(tim.total)
	durMovie := durMedia
	if mts != movieTimescale {
		durMovie = uint32(tim.total * int64(movieTimescale) / int64(mts))
	}

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
	mdiaChildren = append(mdiaChildren, buildHdlr(t.spec.handler, handlerName(t.spec.handler)), minf)
	mdia := container("mdia", mdiaChildren...)

	trakChildren := [][]byte{buildTkhd(t, durMovie)}
	if t.chapterRefID > 0 {
		trakChildren = append(trakChildren, buildTrefChap(t.chapterRefID))
	}
	// MP4 has no native forced flag; record it the way ffmpeg does — a track-level
	// kind box with the DASH role scheme.
	if t.mkv.IsForced {
		trakChildren = append(trakChildren, container("udta", buildKind(dashRoleScheme, "forced-subtitle")))
	}
	// Re-signal the gapless priming (Matroska CodecDelay) as an MP4 edit list, so a
	// decoder discards it and the delay survives the MKV->MP4 round-trip. Limited to
	// the codecs the CodecDelay path reproduces correctly (see hasContainerPriming).
	if t.mkv.CodecDelay > 0 && hasContainerPriming(t.mkv.Codec) {
		trakChildren = append(trakChildren, buildEdts(t.mkv.CodecDelay, durMovie, mts))
	}
	trakChildren = append(trakChildren, mdia)
	return container("trak", trakChildren...), durMovie
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
	// in_movie | in_preview, plus track_enabled when the track is the default —
	// ffmpeg maps track_enabled back to the "default" disposition.
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
