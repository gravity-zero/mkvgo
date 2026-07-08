package mp4

// fragment.go — fragmented MP4 (fMP4 / CMAF) output: an init segment
// (ftyp + moov with empty sample tables + mvex/trex) followed by media segments
// (styp + moof + mdat), the container form HLS and DASH stream. Unlike the
// progressive writer (moov after mdat, whole-file sample tables), a fragmented
// presentation is written strictly forward — each segment is self-describing via
// its moof — so it can be produced and served segment by segment.
//
// It reuses the progressive muxer's track planning and sample-entry builders
// (planTracks, codec.go) and the decode-timing reconstruction (sample.go); only
// the box layout differs.

import "sort"

// mp4 sample_flags (ISO/IEC 14496-12 §8.8.3.1): a sync sample depends on nothing
// (sample_depends_on = 2); a non-sync sample depends on others and sets
// sample_is_non_sync_sample. These match what standard fragmented-MP4 muxers write.
const (
	sampleFlagsSync    = 0x02000000
	sampleFlagsNonSync = 0x01010000
)

// tfhd/trun/tf_flags bits used below.
const (
	tfhdDefaultBaseIsMoof = 0x020000 // base_data_offset is the moof start
	trunDataOffset        = 0x000001
	trunSampleDuration    = 0x000100
	trunSampleSize        = 0x000200
	trunSampleFlags       = 0x000400
	trunSampleCTS         = 0x000800
)

// fragSample is one sample's metadata for the fragmented writer, in decode
// order. The data bytes live in a per-track temp file (streamed, not held).
type fragSample struct {
	size  uint32
	ptsMs int64
	// blockPtsMs is the stored timecode of the sample's source block
	// (mkv.Block.BlockTimecode): segment windows key on it so the frames of a
	// laced block - individually timed but sharing one stored timecode - are
	// never split across two windows. Equal to ptsMs for unlaced sources.
	blockPtsMs int64
	sync       bool
	// filled by fillFragTiming, in the track's media timescale (DTS rebased so
	// the track's first sample is 0):
	dtsTS int64
	durTS int64
	ctsTS int32
}

// buildInitSegment assembles the fMP4 initialisation segment: ftyp (with the
// fragmented/CMAF brands) followed by a moov whose every track has an EMPTY
// sample table and a movie-extends (mvex) box declaring the tracks will be
// fragmented. meta carries the title/tags/cover, like the progressive moov.
func buildInitSegment(tracks []*fragTrack, meta movieMeta, cenc *CENCOptions) []byte {
	ftyp := boxf("ftyp", func(w *bw) {
		w.fourcc("iso5") // fragmented-capable major brand
		w.u32(512)       // minor_version
		for _, b := range dedupeBrands([]string{"iso5", "iso6", "cmfc", "mp41", "dash"}) {
			w.fourcc(b)
		}
	})

	var (
		maxID   uint32
		totalMs int64
	)
	traks := make([][]byte, 0, len(tracks))
	for _, ft := range tracks {
		traks = append(traks, buildInitTrak(ft, cenc))
		if ft.outTrack.mp4ID > maxID {
			maxID = ft.outTrack.mp4ID
		}
		if ft.presentMs > totalMs {
			totalMs = ft.presentMs
		}
	}

	children := make([][]byte, 0, len(traks)+4)
	children = append(children, buildMvhd(uint32(totalMs), maxID+1))
	children = append(children, traks...)
	children = append(children, buildMvex(tracks, uint32(totalMs)))
	if mb := buildMovieMeta(meta.title, meta.tags, meta.cover, meta.hashes); mb != nil {
		children = append(children, container("udta", mb))
	}
	children = append(children, buildPsshBoxes(cenc)...)
	return append(ftyp, container("moov", children...)...)
}

// buildInitTrak builds a track box with an EMPTY sample table (stsd carrying the
// sample entry, then zero-entry stts/stsc/stsz/stco). Everything else mirrors the
// progressive trak: header, media header, handler, edit list for A/V offset.
func buildInitTrak(ft *fragTrack, cenc *CENCOptions) []byte {
	t := ft.outTrack

	var mediaHeader []byte
	switch {
	case t.spec.video:
		mediaHeader = vmhd()
	case t.spec.text:
		mediaHeader = nmhd()
	default:
		mediaHeader = smhd()
	}
	sampleEntry := t.sampleEntry
	if cenc != nil {
		sampleEntry = wrapProtectedSampleEntry(t.sampleEntry, t.spec.video, cenc)
	}
	emptyStbl := container("stbl",
		fullBox("stsd", 0, 0, func(w *bw) { w.u32(1); w.bytes(sampleEntry) }),
		fullBox("stts", 0, 0, func(w *bw) { w.u32(0) }),
		fullBox("stsc", 0, 0, func(w *bw) { w.u32(0) }),
		fullBox("stsz", 0, 0, func(w *bw) { w.u32(0); w.u32(0) }),
		fullBox("stco", 0, 0, func(w *bw) { w.u32(0) }),
	)
	minf := container("minf", mediaHeader, buildDinf(), emptyStbl)

	mdiaChildren := [][]byte{buildMdhd(uint32(ft.durMediaTS), ft.timescale, mdhdLanguage(t.mkv))}
	if t.mkv.LanguageBCP47 != "" {
		mdiaChildren = append(mdiaChildren, buildElng(t.mkv.LanguageBCP47))
	}
	hName := handlerName(t.spec.handler)
	if t.mkv.Name != "" {
		hName = t.mkv.Name
	}
	mdiaChildren = append(mdiaChildren, buildHdlr(t.spec.handler, hName), minf)
	mdia := container("mdia", mdiaChildren...)

	trakChildren := [][]byte{buildTkhd(t, uint32(ft.presentMs))}
	if t.mkv.IsForced {
		trakChildren = append(trakChildren, container("udta", buildKind(dashRoleScheme, "forced-subtitle")))
	}
	// Edit list, like the progressive path: the presentation offset (A/V sync —
	// fragment decode times are rebased so each track starts at 0) and the
	// audio gapless priming (CodecDelay) a decoder must discard.
	codecDelay := int64(0)
	if wantsEditList(t.mkv.Codec, t.mp3Delay) {
		codecDelay = t.mkv.CodecDelay
	}
	if ft.offsetMs > 0 || codecDelay > 0 {
		trakChildren = append(trakChildren, buildEdts(codecDelay, ft.offsetMs, uint32(ft.durMovieMs), ft.timescale))
	}
	trakChildren = append(trakChildren, mdia)
	return container("trak", trakChildren...)
}

// buildMvex declares the fragmented tracks: an mehd with the total duration
// (movie timescale) plus one trex per track carrying default sample values.
func buildMvex(tracks []*fragTrack, totalMs uint32) []byte {
	children := [][]byte{
		fullBox("mehd", 0, 0, func(w *bw) { w.u32(totalMs) }),
	}
	for _, ft := range tracks {
		children = append(children, fullBox("trex", 0, 0, func(w *bw) {
			w.u32(ft.outTrack.mp4ID)
			w.u32(1) // default_sample_description_index
			w.u32(0) // default_sample_duration (per-sample in trun)
			w.u32(0) // default_sample_size
			w.u32(sampleFlagsNonSync)
		}))
	}
	return container("mvex", children...)
}

// buildMoof assembles a movie-fragment box for one segment: mfhd (sequence
// number) and one traf per track. Each trun's data_offset points at its track's
// sample bytes: past the whole moof, the 16-byte mdat header (the 64-bit
// largesize form mdatHeader writes) and the preceding tracks' data. The offsets
// depend on the moof's own size, so the moof is built twice — a trun's
// data_offset is a fixed-width u32, so the size of pass two equals pass one.
func buildMoof(seq uint32, segs []trackSegment) []byte {
	assemble := func(moofSize int64) []byte {
		mfhd := fullBox("mfhd", 0, 0, func(w *bw) { w.u32(seq) })
		children := [][]byte{mfhd}
		var dataRun int64
		// trafStart is the byte offset, from the moof box's own first byte, of
		// the next traf's box (header included) - the same anchor trun's
		// data_offset uses (tfhdDefaultBaseIsMoof), so a CENC traf's saio value
		// (computed inside buildTraf) resolves against the same origin.
		trafStart := int64(smallBoxHeaderLen + len(mfhd))
		for i := range segs {
			offset := int32(0)
			if moofSize > 0 {
				offset = int32(moofSize + mdatHeaderLen + dataRun)
			}
			traf := buildTraf(&segs[i], offset, trafStart)
			children = append(children, traf)
			trafStart += int64(len(traf))
			dataRun += segs[i].dataLen
		}
		return container("moof", children...)
	}
	sized := assemble(0)
	return assemble(int64(len(sized)))
}

// buildTraf builds a track-fragment box: tfhd (default-base-is-moof) + tfdt
// (baseMediaDecodeTime) + trun (per-sample duration/size/flags/cts) pointing at
// the track's sample bytes via dataOffset (relative to the moof start), plus -
// when s.cenc is set - senc/saiz/saio describing the track's per-sample CENC
// auxiliary information (see cenc.go). trafStart is this traf's own offset
// from the moof's first byte (buildMoof's running total), needed to compute
// saio's value.
func buildTraf(s *trackSegment, dataOffset int32, trafStart int64) []byte {
	tfhd := fullBox("tfhd", 0, tfhdDefaultBaseIsMoof, func(w *bw) {
		w.u32(s.trackID)
	})
	tfdt := fullBox("tfdt", 1, 0, func(w *bw) {
		w.u64(uint64(s.baseDecodeTS))
	})
	flags := uint32(trunDataOffset | trunSampleDuration | trunSampleSize | trunSampleFlags)
	if s.hasCTS {
		flags |= trunSampleCTS
	}
	trun := fullBox("trun", 1, flags, func(w *bw) {
		w.u32(uint32(len(s.samples)))
		w.i32(dataOffset)
		for i := range s.samples {
			sm := &s.samples[i]
			w.u32(uint32(sm.durTS))
			w.u32(sm.size)
			if sm.sync {
				w.u32(sampleFlagsSync)
			} else {
				w.u32(sampleFlagsNonSync)
			}
			if s.hasCTS {
				w.i32(sm.ctsTS)
			}
		}
	})
	children := [][]byte{tfhd, tfdt, trun}
	if s.cenc != nil {
		aux := sencAuxOffset(trafStart, tfhd, tfdt, trun)
		children = append(children, buildSenc(s.cenc), buildSaiz(s.cenc), buildSaio(aux))
	}
	return container("traf", children...)
}

// trackSegment holds one track's samples for one media segment, ready to frame
// into a traf.
type trackSegment struct {
	trackID      uint32
	baseDecodeTS int64
	samples      []fragSample
	hasCTS       bool
	dataLen      int64 // total sample bytes for this track in the segment
	// cenc holds this segment's CENC auxiliary data (senc/saiz), built from
	// the plaintext samples before the moof is framed; nil when the segment
	// is not CENC-protected.
	cenc *cencTrafData
}

// buildStyp is the segment type box: a fMP4 media segment opens with one so a
// player can start mid-stream. Brands mirror the init ftyp.
func buildStyp() []byte {
	return boxf("styp", func(w *bw) {
		w.fourcc("msdh")
		w.u32(0)
		for _, b := range []string{"msdh", "msix", "cmfs"} {
			w.fourcc(b)
		}
	})
}

// fillFragTiming assigns each sample its decode time, duration and composition
// offset (in the track's media timescale), from the presentation timestamps —
// the same reconstruction the progressive path uses (sample.go reconstructTiming):
// DTS is the sorted PTS in decode order, durations are the diffs, ctts = PTS - DTS.
// DTS is rebased so the track's first sample is 0; the presentation offset (the
// smallest PTS) is returned so the caller can emit it as an edit list.
func fillFragTiming(samples []fragSample, lastDurMs int64, mts uint32, gridTS int64) (offsetMs int64, hasCTS bool, totalTS int64) {
	n := len(samples)
	if n == 0 {
		return 0, false, 0
	}
	scale := func(ms int64) int64 {
		if mts == movieTimescale {
			return ms
		}
		return ms * int64(mts) / int64(movieTimescale)
	}
	if gridTS <= 0 { // laced audio with no DefaultDuration: recover the stride
		gridTS = deriveGridTS(n, func(i int) int64 { return samples[i].blockPtsMs }, mts)
	}

	// Constant-rate audio rides the sample-exact grid (see audioGridTS): frame
	// k decodes at k×gridTS, k re-derived from each block's stored timecode
	// (then +1 within a lace). Millisecond rounding never reaches the trun —
	// no ±1 ms duration jitter, no duplicated DTS at a lace boundary, no
	// one-frame drift per segment — while a real gap still moves k.
	if gridTS > 0 {
		anchor := scale(samples[0].blockPtsMs)
		offsetMs = samples[0].ptsMs
		k := int64(0)
		for i := range samples {
			if i > 0 {
				if samples[i].blockPtsMs != samples[i-1].blockPtsMs {
					nk := gridIndex(scale(samples[i].blockPtsMs)-anchor, gridTS)
					if nk <= k {
						nk = k + 1
					}
					k = nk
				} else {
					k++
				}
				samples[i-1].durTS = k*gridTS - samples[i-1].dtsTS
			}
			samples[i].dtsTS = k * gridTS
			samples[i].ctsTS = 0
			if samples[i].ptsMs < offsetMs {
				offsetMs = samples[i].ptsMs
			}
		}
		samples[n-1].durTS = gridTS
		for i := range samples {
			totalTS += samples[i].durTS
		}
		return offsetMs, false, totalTS
	}
	// DTS = sorted PTS: the i-th stored sample (decode order) gets the i-th
	// smallest presentation time. Rebase to 0.
	dts := make([]int64, n)
	for i := range samples {
		dts[i] = scale(samples[i].ptsMs)
	}
	sort.Slice(dts, func(i, j int) bool { return dts[i] < dts[j] })
	base := dts[0]
	for i := range samples {
		samples[i].dtsTS = dts[i] - base
	}
	for i := 0; i < n-1; i++ {
		samples[i].durTS = dts[i+1] - dts[i]
	}
	switch {
	case lastDurMs > 0:
		samples[n-1].durTS = scale(lastDurMs)
	case n > 1:
		samples[n-1].durTS = samples[n-2].durTS
	default:
		samples[n-1].durTS = 1
	}
	offsetMs = samples[0].ptsMs
	for i := range samples {
		if samples[i].ptsMs < offsetMs {
			offsetMs = samples[i].ptsMs
		}
		off := scale(samples[i].ptsMs) - dts[i]
		samples[i].ctsTS = int32(off)
		if off != 0 {
			hasCTS = true
		}
		totalTS += samples[i].durTS
	}
	return offsetMs, hasCTS, totalTS
}
