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

// buildTrak builds one trak box and returns it with the track's duration (ms).
func buildTrak(t *outTrack, mdatBase int64, co64 bool) ([]byte, uint32) {
	var tim timing
	if t.spec.text || t.isChapter {
		tim = textTiming(t.samples.samples)
	} else {
		tim = reconstructTiming(t.samples.samples, t.frameDurMs)
	}
	dur := uint32(tim.total)

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
	mdiaChildren := [][]byte{buildMdhd(dur, mdhdLanguage(t.mkv))}
	// An elng box carries the BCP-47 language tag (mdhd holds only the legacy
	// ISO 639-2 code), so a full language round-trips through the remux.
	if t.mkv.LanguageBCP47 != "" {
		mdiaChildren = append(mdiaChildren, buildElng(t.mkv.LanguageBCP47))
	}
	mdiaChildren = append(mdiaChildren, buildHdlr(t.spec.handler, handlerName(t.spec.handler)), minf)
	mdia := container("mdia", mdiaChildren...)

	trakChildren := [][]byte{buildTkhd(t, dur)}
	if t.chapterRefID > 0 {
		trakChildren = append(trakChildren, buildTrefChap(t.chapterRefID))
	}
	// MP4 has no native forced flag; record it the way ffmpeg does — a track-level
	// kind box with the DASH role scheme.
	if t.mkv.IsForced {
		trakChildren = append(trakChildren, container("udta", buildKind(dashRoleScheme, "forced-subtitle")))
	}
	trakChildren = append(trakChildren, mdia)
	return container("trak", trakChildren...), dur
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

func buildMdhd(durationMs uint32, lang string) []byte {
	return fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0) // creation_time
		w.u32(0) // modification_time
		w.u32(movieTimescale)
		w.u32(durationMs)
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
