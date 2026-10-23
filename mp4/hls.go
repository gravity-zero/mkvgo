package mp4

// hls.go - RemuxToHLS: a fragmented-MP4 / CMAF presentation produced from a
// Matroska/WebM file without transcoding, served through two manifests over
// the SAME segments (the point of CMAF): HLS (master.m3u8 + media playlists)
// and DASH (manifest.mpd). Tracks are demuxed - one rendition per track, as
// Apple recommends for HLS and as DASH players require - which also gives
// multi-audio (VF/VO) selection for free. This is the "copy rung" of an ABR
// ladder: mkvgo does the packaging; bitrate variants remain a transcoder's job.
//
// Memory is bounded: per-sample metadata (size/pts/sync/offset - the same
// order of magnitude the progressive muxer already holds) is kept in RAM,
// while the sample *bytes* stay in the source until the segments are written:
// a local file is walked by its block headers, then read once, at the recorded
// offsets. A network source (each read a round trip) or a track that is not
// stored as it is written (compressed, converted) streams its bytes to one
// temp file per track, read back sequentially. Subtitle cues are small text
// and are held in RAM. Nothing holds the whole media.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// isBlockWalkEnd reports whether a BlockReader.Next error ends the walk cleanly.
// io.EOF is the normal end. io.ErrUnexpectedEOF means the source declared more
// bytes than the file physically holds - a truncated download, or an
// unfinalised/over-declared Segment (both seen in real libraries: a Segment or
// final cluster whose size runs past the real end of file). A forward block walk
// can never recover data past the file end, and only the final element can
// extend past the single EOF, so the packaging paths deliver every complete
// block read so far and stop here, instead of failing the whole remux - exactly
// as the metadata reader tolerates an over-declared tail. The strict BlockReader
// still returns the raw error, so integrity paths (validate/compare) can report
// the truncation.
//
// A damaged region INSIDE the file (reader.ErrDamagedRegion: an element that
// cannot be read while media continues behind it) ends the walk the same way
// here: these are the on-demand paths, where a segment that stops at the
// damage beats a segment that fails. A region the reader could not see past
// (DamageError.Resume < 0: longer than the plan's damageCap) is not an end
// but the refusal it is. A path that writes the WHOLE file out uses
// isSourceEnd instead, and refuses.
func isBlockWalkEnd(err error) bool {
	if isSourceEnd(err) {
		return true
	}
	var de *reader.DamageError
	return errors.As(err, &de) && de.Resume >= 0
}

// isSourceEnd reports whether a BlockReader.Next error is the end of the
// source: io.EOF, or a truncated/over-declared tail. Damage inside the file is
// NOT the end of it - taken for one, a remux delivered the first second of a
// 37 s file and reported success.
func isSourceEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// keepTrackSet returns Options.KeepTracks as a lookup set (the Virtual Edit
// Layer's track filter), or nil when empty - meaning keep every track.
func keepTrackSet(o *Options) map[uint64]bool {
	if len(o.KeepTracks) == 0 {
		return nil
	}
	m := make(map[uint64]bool, len(o.KeepTracks))
	for _, id := range o.KeepTracks {
		m[id] = true
	}
	return m
}

// filterSubTracks drops subtitle renditions whose track is not in keep (nil keep
// keeps all), so a virtual version carries only its selected subtitles.
func filterSubTracks(subs []hlsSubTrack, keep map[uint64]bool) []hlsSubTrack {
	if keep == nil {
		return subs
	}
	out := subs[:0:0]
	for _, s := range subs {
		if keep[s.track.ID] {
			out = append(out, s)
		}
	}
	return out
}

const defaultSegmentMs = 6000

// tempWriteBuf batches a track's per-sample temp-file writes into 1 MiB write calls.
const tempWriteBuf = 1 << 20

// bufferedTemp is a track's temp file behind a write buffer, flushed before a seek and on close.
type bufferedTemp struct {
	f mkv.WriteSeekCloser
	w *bufio.Writer
}

func newBufferedTemp(f mkv.WriteSeekCloser) *bufferedTemp {
	return &bufferedTemp{f: f, w: bufio.NewWriterSize(f, tempWriteBuf)}
}

func (b *bufferedTemp) Write(p []byte) (int, error) { return b.w.Write(p) }

func (b *bufferedTemp) Seek(off int64, whence int) (int64, error) {
	if err := b.w.Flush(); err != nil {
		return 0, err
	}
	return b.f.Seek(off, whence)
}

func (b *bufferedTemp) Close() error {
	err := b.w.Flush()
	if cerr := b.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// fragTrack augments an outTrack with the fragmented-writer state: the media
// timescale, the collected samples, the temp file holding their bytes, and the
// derived durations/offset.
type fragTrack struct {
	outTrack  *outTrack
	timescale uint32
	samples   []fragSample
	tmp       mkv.WriteSeekCloser
	tmpPath   string
	// offs, for a track read from the source directly (no temp file), holds
	// each sample's offset there: its stored bytes, after any stripped header.
	offs       []int64
	offsetMs   int64
	durMediaTS int64
	durMovieMs int64
	presentMs  int64
	hasCTS     bool
	// ctsShiftTS is how far this track's presentation is pushed back so its
	// composition offsets stay non-negative (compositionShiftTS); the init's
	// edit list takes exactly this much back out.
	ctsShiftTS int64
	// ptsTS, when not nil, holds each sample's presentation time in the
	// track's own timescale (an MP4 source's video keeps its native ticks: see
	// mp4NativeTimescale); the timing derivation then works on it instead of
	// the millisecond times, and releases it.
	ptsTS []int64
	// sampleBps is the bit rate the samples measured (sampleBandwidth), kept
	// by an MP4 plan that releases its sample arrays once built.
	sampleBps      int64
	sampleBpsKnown bool
}

// hlsSubTrack is one text subtitle track carried as a segmented WebVTT
// playlist (HLS's native subtitle form - text subs are not fragmented as mdat).
type hlsSubTrack struct {
	track mkv.Track
	cues  []subtitle.Cue
}

// segInfo describes one written media segment, for the playlists: its duration
// (seconds) and its total byte size (BANDWIDTH computation).
type segInfo struct {
	durSec     float64
	bytes      int64
	audioBytes int64 // the part of bytes in non-video renditions: what a video-only ABR variant shares from the reference
}

// RemuxToHLS reads the Matroska/WebM file at srcPath and writes a fragmented-MP4
// HLS presentation into outputDir:
//
//	master.m3u8            multivariant playlist (BANDWIDTH/RESOLUTION/CODECS,
//	                       subtitle renditions)
//	playlist.m3u8          the muxed audio+video media playlist
//	init.mp4               CMAF initialisation segment
//	seg00001.m4s …         media segments (styp + moof + mdat)
//	subN.m3u8, subN_*.vtt  one segmented WebVTT playlist per text subtitle track
//
// It never transcodes - samples are copied verbatim into CMAF fragments - so
// only the codecs RemuxToMP4 supports are carried. Segments are cut on video
// keyframes at roughly Options.SegmentMs (default 6 s) and are independently
// decodable. Text subtitles (SRT, WebVTT, ASS/SSA flattened to plain text) ride
// as WebVTT renditions; bitmap subtitles (PGS/VOBSUB) are dropped and reported
// via Options.OnDrop.
func RemuxToHLS(ctx context.Context, srcPath, outputDir string, opts ...Options) error {
	if err := refusePackagedSource(outputDir, srcPath); err != nil {
		return err
	}
	o := optionsFrom(opts)
	_, err := remuxToHLSInto(ctx, srcPath, outputDir, &o)
	return err
}

// hlsResult carries what a packaging pass established - the ABR packager
// builds its top-level master from these, and the concat packager (concat.go)
// additionally uses bounds to window subtitle cues per part.
type hlsResult struct {
	fts     []*fragTrack
	subs    []hlsSubTrack
	segs    []segInfo
	durs    []float64
	bounds  []int64     // segment start times (ms), same length as durs
	iframes []iframeRef // trick-play I-frames (MP4 sources, unencrypted); nil otherwise
}

// remuxToHLSInto is RemuxToHLS's body, returning the packaging facts.
func remuxToHLSInto(ctx context.Context, srcPath, outputDir string, op *Options) (res *hlsResult, err error) {
	o := *op
	fs := o.FS
	segMs := o.SegmentMs
	if segMs <= 0 {
		segMs = defaultSegmentMs
	}
	if o.Encrypt != nil {
		if err := o.Encrypt.validate(); err != nil {
			return nil, err
		}
		if o.SingleFile {
			return nil, errf("SingleFile and Encrypt cannot be combined (AES-128 byte ranges are not supported)")
		}
	}

	ps, err := openPackagingSource(ctx, srcPath, fs, o.moovLimit())
	if err != nil {
		return nil, err
	}
	defer ps.Close()
	c := ps.c
	planned, _, err := planTracks(c, o)
	if err != nil {
		return nil, err
	}
	if err := applyFrameConverterToTracks(o.FrameConverter, planned); err != nil {
		return nil, err
	}
	// One demuxed rendition per track: the video, each audio, and the text
	// subtitles as WebVTT renditions. Secondary video tracks have no HLS/DASH
	// form here (they would be camera angles) and are dropped with a reason.
	keep := keepTrackSet(&o)
	var media []*outTrack
	var videoSeen bool
	for _, t := range planned {
		if t.isChapter || t.spec.text {
			continue
		}
		if keep != nil && !keep[t.mkv.ID] {
			continue // Virtual Edit Layer: this track is not in the kept subset
		}
		if t.spec.video {
			if videoSeen {
				o.report(DroppedTrack{ID: t.mkv.ID, Type: t.mkv.Type, Codec: t.mkv.Codec,
					Reason: "the presentation carries one video track; secondary video tracks are dropped"})
				continue
			}
			videoSeen = true
		} else if o.VideoOnly {
			continue // an ABR variant contributes only its video rendition
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return nil, errf("no audio or video track to segment")
	}
	if err := cencPreflight(&o, media); err != nil {
		return nil, err
	}
	var subs []hlsSubTrack
	if !o.VideoOnly {
		subs = filterSubTracks(planSubTracks(c, o), keep)
	}

	if err := fs.DoMkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}

	// A local source is read again for the segments, at the offsets the walk
	// records; a track it cannot give as stored goes through a temp file.
	src, err := openDirectSource(&o, fs, srcPath)
	if err != nil {
		return nil, err
	}
	var srcStamp string
	if src != nil {
		defer src.Close()
		fi, err := fs.DoStat(srcPath)
		if err != nil {
			return nil, err
		}
		srcStamp = statStamp(fi)
	}
	fts := make([]*fragTrack, len(media))
	routing := make(map[uint64]*fragTrack, len(media))
	// Best-effort cleanup of the per-track temp files, armed before the first is created.
	defer func() {
		for _, ft := range fts {
			if ft == nil || ft.tmpPath == "" {
				continue
			}
			if ft.tmp != nil {
				ft.tmp.Close()
			}
			_ = fs.DoRemove(ft.tmpPath)
		}
	}()
	for i, t := range media {
		fts[i] = &fragTrack{outTrack: t, timescale: mediaTimescale(t)}
		if src != nil && directTrack(t) {
			fts[i].offs = []int64{}
		} else {
			tmpPath := filepath.Join(outputDir, fmt.Sprintf(".mkvgo-hls-t%d.tmp", t.mp4ID))
			tmp, cerr := fs.DoCreate(tmpPath)
			if cerr != nil {
				return nil, cerr
			}
			fts[i].tmp, fts[i].tmpPath = newBufferedTemp(tmp), tmpPath
		}
		if ts := mp4NativeTimescale(ps, t); ts != 0 {
			fts[i].timescale, fts[i].ptsTS = ts, []int64{}
		}
		routing[t.mkv.ID] = fts[i]
	}

	// Phase 1 - stream every sample, buffering media bytes to the per-track
	// temp files and collecting subtitle cues (source-specific walk).
	if ps.mv != nil {
		err = collectFromMP4(ctx, ps, routing, subs, o.Progress)
	} else {
		err = collectFragSamples(ctx, srcPath, fs, c, routing, subs, o.Progress, src)
	}
	if err != nil {
		return nil, err
	}
	for _, ft := range fts {
		if len(ft.samples) == 0 {
			return nil, errf("track %d produced no samples", ft.outTrack.mp4ID)
		}
		if ft.tmp != nil {
			if cerr := ft.tmp.Close(); cerr != nil {
				return nil, cerr
			}
			ft.tmp = nil
		}
		grid := audioGridTS(ft.outTrack, ft.timescale)
		if grid == 0 {
			grid = laceGridTS(ft.outTrack, len(ft.samples), func(i int) int64 { return ft.samples[i].blockPtsMs }, ft.timescale)
			last := ft.samples[len(ft.samples)-1].blockPtsMs
			lastFrames := int64(0)
			for i := len(ft.samples) - 1; i >= 0 && ft.samples[i].blockPtsMs == last; i-- {
				lastFrames++
			}
			grid = settleStride(grid, ps.c, ft.outTrack, ft.timescale, ft.samples[0].blockPtsMs, last, lastFrames)
		}
		if grid == 0 && ps.mv != nil {
			grid = mp4FrameGridTS(ps, ft.outTrack, ft.timescale)
		}
		off, hasCTS, totalTS, ctsShift := fillFragTimingTS(ft.samples, ft.ptsTS, ft.outTrack.frameDurMs, ft.timescale, grid)
		ft.ptsTS = nil
		ft.offsetMs, ft.hasCTS, ft.durMediaTS = off, hasCTS, totalTS
		ft.ctsShiftTS = ctsShift
		ft.durMovieMs = totalTS
		if ft.timescale != movieTimescale {
			ft.durMovieMs = totalTS * int64(movieTimescale) / int64(ft.timescale)
		}
		ft.presentMs = off + ft.durMovieMs
	}
	// Options.AudioPresentationShift: re-base the shifted audio tracks'
	// edit-list offsets. Applied after the timing derivation, so the sample
	// decode times keep the unshifted origin - only the init's edit list
	// moves (and PlanHLS applies the same rule: full pass and plan stay
	// byte-identical for the same shift).
	applyAudioPresentationShift(&o, fts)

	// Phase 2 - segment boundaries from the primary track: the video's
	// keyframes, or (audio-only presentation) the first audio's sample grid
	// (every audio sample is a sync point, so the same cut applies).
	bounds := segmentBoundaries(fts[primaryIndex(fts)].samples, segMs)

	cover, err := loadCoverArt(fs, c.Attachments)
	if err != nil {
		return nil, err
	}
	meta := movieMeta{title: c.Info.Title, tags: globalTags(c), cover: cover}

	var segs []segInfo
	var rends []sfRendition
	var iframesAll []iframeRef
	if o.SingleFile {
		// One progressive file per rendition (init + sidx + fragments); the
		// playlists reference it by byte ranges.
		segs, rends, err = writeSingleFileRenditions(ctx, &o, fs, outputDir, fts, bounds, meta, src)
		if err != nil {
			return nil, err
		}
	} else {
		// One init segment per rendition (demuxed CMAF); the movie metadata
		// (title, tags, cover art) rides on the video rendition's init.
		for i, ft := range fts {
			m := movieMeta{}
			if i == primaryIndex(fts) {
				m = meta
			}
			initData := buildInitSegment([]*fragTrack{ft}, m, o.CENC)
			if err := fs.DoWriteFile(filepath.Join(outputDir, renditionInit(fts, i)), initData, 0o644); err != nil {
				return nil, err
			}
		}
		segs, iframesAll, err = writeSegments(ctx, &o, fs, outputDir, fts, bounds, src)
		if err != nil {
			return nil, err
		}
		// Trick-play: the standard I-frame playlist - one keyframe per
		// segment, referenced by byte range into the existing segments (no
		// extra media). Skipped when encrypting (a ciphertext subrange is
		// not independently decryptable).
		if v := pickVideoFrag(fts); v != nil && o.Encrypt == nil && o.CENC == nil && len(iframesAll) > 0 {
			pl := buildIFramePlaylist(&o, fts, dursOf(segs), iframesAll)
			if err := fs.DoWriteFile(filepath.Join(outputDir, "iframe.m3u8"), pl, 0o644); err != nil {
				return nil, err
			}
		}
	}
	if src != nil {
		// The segments were read from the source after the walk: it must still be the file walked.
		fi, err := fs.DoStat(srcPath)
		if err != nil {
			return nil, err
		}
		if statStamp(fi) != srcStamp {
			return nil, errf("the source changed during the remux: %w", ErrSourceChanged)
		}
	}
	durs := make([]float64, len(segs))
	for i := range segs {
		durs[i] = segs[i].durSec
	}
	// Options.ChapterMarkers: the video rendition's playlist only (SingleFile
	// carries no chapter markers in this version - see buildByteRangePlaylist).
	chapters := chapterMarkers(&o, c.Chapters, fts[primaryIndex(fts)].presentMs)
	for i := range fts {
		i := i
		var pl []byte
		if o.SingleFile {
			pl = buildByteRangePlaylist(&o, durs, &rends[i])
		} else {
			var chs []mkv.Chapter
			if i == primaryIndex(fts) && pickVideoFrag(fts) != nil {
				chs = chapters
			}
			pl = buildMediaPlaylist(&o, durs, renditionInit(fts, i),
				func(k int) string { return renditionSegment(fts, i, k) }, chs)
		}
		if err := fs.DoWriteFile(filepath.Join(outputDir, renditionPlaylist(fts, i)), pl, 0o644); err != nil {
			return nil, err
		}
	}
	for i := range subs {
		if err := writeSubtitleRendition(&o, fs, outputDir, i, &subs[i], bounds, durs, fts); err != nil {
			return nil, err
		}
	}
	if err := writeMasterPlaylist(&o, fs, outputDir, fts, subs, segs, iframesAll); err != nil {
		return nil, err
	}
	res = &hlsResult{fts: fts, subs: subs, segs: segs, durs: durs, bounds: bounds}
	if o.Encrypt == nil && o.CENC == nil {
		res.iframes = iframesAll
	}
	if o.Encrypt != nil {
		return res, nil // AES-128 is HLS-only; no DASH manifest for an encrypted presentation
	}
	// The DASH side of the CMAF packaging: same init + segments, second
	// manifest. The MPD references each subtitle rendition as one whole file
	// (subN.vtt), which writeSubtitleRendition also wrote.
	var mpd []byte
	if o.SingleFile {
		mpd = buildDASHManifestSingle(&o, fts, subs, durs, peakBandwidth(segs), rends)
	} else {
		mpd = buildDASHManifest(&o, fts, subs, durs, peakBandwidth(segs), chapters)
	}
	return res, fs.DoWriteFile(filepath.Join(outputDir, "manifest.mpd"), mpd, 0o644)
}

// planSubTracks selects the text subtitle tracks carried as WebVTT renditions.
// Bitmap formats have no text form and are dropped with a reason.
func planSubTracks(c *mkv.Container, o Options) []hlsSubTrack {
	var subs []hlsSubTrack
	for _, t := range c.Tracks {
		if t.Type != mkv.SubtitleTrack {
			continue
		}
		switch canonicalSubCodec(t.Codec) {
		case "srt", "webvtt", "ass", "ssa":
			subs = append(subs, hlsSubTrack{track: t})
		case "pgs":
			// HLS carries subtitles as WebVTT renditions, and a picture has no
			// WebVTT form. Burning it into the video or shipping a sprite sheet
			// is player-side packaging, which is the consumer's call - so name
			// the extractor rather than pretend the track is unusable.
			o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
				Reason: "PGS subtitles are pictures and HLS renditions carry WebVTT; extract them with matroska.ExtractSubtitlePGS and overlay them player-side"})
		default:
			o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
				Reason: "subtitle format not representable as WebVTT (bitmap formats cannot be carried)"})
		}
	}
	return subs
}

// collectFragSamples reads every block once, writing each media sample's bytes to
// its track's temp file and recording (size, pts, sync); subtitle blocks become
// WebVTT cues. Lazily builds the sample entry for codecs that derive it from the
// first frame.
func collectFragSamples(ctx context.Context, srcPath string, fs *mkv.FS, c *mkv.Container,
	routing map[uint64]*fragTrack, subs []hlsSubTrack, progress mkv.ProgressFunc, direct io.ReadSeeker) error {
	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
	if err != nil {
		return err
	}
	// Explicit (not just the walk-by pickup): the on-demand plan feeds its
	// mid-file readers the same durations, and full-pass ↔ plan byte parity
	// must not depend on the source's element order.
	br.SetTrackDefaultDurations(reader.TrackDefaultDurations(c.Tracks))
	if progress != nil {
		if st, _ := fs.DoStat(srcPath); st != nil {
			br.SetProgress(progress, st.Size())
		}
	}
	subRouting := make(map[uint64]*hlsSubTrack, len(subs))
	for i := range subs {
		subRouting[subs[i].track.ID] = &subs[i]
	}
	// The tracks read from the source later are walked by their headers alone.
	var headerOnly, kept []uint64
	for id, ft := range routing {
		kept = append(kept, id)
		if ft.offs != nil {
			headerOnly = append(headerOnly, id)
		}
	}
	if len(headerOnly) > 0 {
		for id := range subRouting {
			kept = append(kept, id)
		}
		br.KeepTracks(kept...)
		br.SetHeaderOnly(true)
		br.SetHeaderOnlyTracks(headerOnly...)
		br.SetSparse(true)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if isSourceEnd(err) {
			break
		}
		if err != nil {
			return errf("read block: %w", err)
		}
		if st, ok := subRouting[b.TrackNumber]; ok {
			cue, ok, err := subCueFromBlock(&st.track, b)
			if err != nil {
				return err
			}
			if ok {
				st.cues = append(st.cues, cue)
			}
			continue
		}
		ft, ok := routing[b.TrackNumber]
		if !ok {
			continue
		}
		if ft.offs != nil {
			if err := collectDirectSample(ft, b, direct); err != nil {
				return err
			}
			continue
		}
		data, err := ft.outTrack.mkv.DecodePayload(b.Data)
		if err != nil {
			return err
		}
		data, err = ft.outTrack.convertFrame(data)
		if err != nil {
			return errf("convert frame: %w", err)
		}
		ft.outTrack.learnFrame(data)
		if ft.outTrack.sampleEntry == nil {
			entry, err := ft.outTrack.spec.sampleEntry(&ft.outTrack.mkv, data)
			if err != nil {
				return err
			}
			ft.outTrack.sampleEntry = entry
		}
		if _, err := ft.tmp.Write(data); err != nil {
			return errf("buffer sample: %w", err)
		}
		ft.samples = append(ft.samples, fragSample{size: uint32(len(data)),
			ptsMs: b.Timecode, blockPtsMs: b.BlockTimecode, sync: b.Keyframe})
	}
	return nil
}

// collectDirectSample records a block read by its header alone: its size and
// place in the source. The track's first frame is read, for what is learned
// from it.
func collectDirectSample(ft *fragTrack, b mkv.Block, src io.ReadSeeker) error {
	t := ft.outTrack
	size := int64(len(t.mkv.HeaderStripping)) + b.Size
	if size > math.MaxUint32 {
		return errf("track %d: sample of %d bytes exceeds MP4 sample size limit", t.mkv.ID, size)
	}
	if err := learnFirstFrame(t, b, src); err != nil {
		return err
	}
	ft.offs = append(ft.offs, b.DataOffset)
	ft.samples = append(ft.samples, fragSample{size: uint32(size),
		ptsMs: b.Timecode, blockPtsMs: b.BlockTimecode, sync: b.Keyframe})
	return nil
}

// subCueFromBlock converts one Matroska subtitle block into a WebVTT cue. ASS
// dialogue lines are flattened to their plain text (styling is lost - WebVTT
// has no ASS form); SRT and WebVTT payloads pass through.
func subCueFromBlock(t *mkv.Track, b mkv.Block) (subtitle.Cue, bool, error) {
	cue, ok, _, err := subCueFromBlockRecovered(t, b)
	return cue, ok, err
}

// subCueFromBlockRecovered is subCueFromBlock that also reports an undeclared zlib block it inflated.
func subCueFromBlockRecovered(t *mkv.Track, b mkv.Block) (subtitle.Cue, bool, bool, error) {
	data, recovered, err := t.DecodePayloadRecovered(b.Data)
	if err != nil {
		return subtitle.Cue{}, false, false, err
	}
	var text string
	switch canonicalSubCodec(t.Codec) {
	case "ass", "ssa":
		text = subtitle.FlattenASSBlock(data)
	default: // srt, webvtt: the block payload is the cue text
		text = strings.TrimRight(string(data), "\n\r\x00")
	}
	if strings.TrimSpace(text) == "" {
		return subtitle.Cue{}, false, recovered, nil
	}
	cue := subtitle.Cue{StartMs: b.Timecode, Text: text}
	if b.Duration > 0 {
		cue.EndMs = b.Timecode + b.Duration
	}
	return cue, true, recovered, nil
}

// pickVideoFrag returns the first video track, the one whose keyframes drive the
// segment boundaries.
func pickVideoFrag(fts []*fragTrack) *fragTrack {
	for _, ft := range fts {
		if ft.outTrack.spec.video {
			return ft
		}
	}
	return nil
}

// videoCodecOf returns the video track's mkvgo short codec name among media,
// or "" for an audio-only presentation - cencPreflight's codec check.
func videoCodecOf(media []*outTrack) string {
	for _, t := range media {
		if t.spec.video {
			return t.mkv.Codec
		}
	}
	return ""
}

// segmentBoundaries returns the presentation times (ms) at which segments start:
// always 0, then each video keyframe whose PTS is at least targetMs past the
// previous boundary. The result drives both the fragment cut and the playlist.
func segmentBoundaries(video []fragSample, targetMs int64) []int64 {
	bounds := []int64{0}
	last := int64(0)
	for i := range video {
		if !video[i].sync {
			continue
		}
		if video[i].blockPtsMs >= last+targetMs {
			bounds = append(bounds, video[i].blockPtsMs)
			last = video[i].blockPtsMs
		}
	}
	return bounds
}

// primaryIndex returns the index of the presentation's primary track: the
// video track, or - audio-only presentation - the first track.
// applyAudioPresentationShift re-bases each shifted audio track's edit-list
// presentation offset (Options.AudioPresentationShift, ns; positive = the
// track's content starts late and is presented earlier). It runs AFTER the
// fragment timing derivation on purpose: sample decode times keep the
// unshifted origin, so the media segments are byte-identical with or without
// a shift - only the init segment's edit list (and the track's presented
// duration) moves. A shift landing before the presentation start clamps to
// 0. Video tracks and unknown track numbers are ignored.
func applyAudioPresentationShift(o *Options, fts []*fragTrack) {
	if len(o.AudioPresentationShift) == 0 {
		return
	}
	for _, ft := range fts {
		if ft.outTrack.spec.video {
			continue
		}
		ns, ok := o.AudioPresentationShift[ft.outTrack.mkv.ID]
		if !ok || ns == 0 {
			continue
		}
		ft.offsetMs -= roundNsToMs(ns)
		if ft.offsetMs < 0 {
			ft.offsetMs = 0
		}
		ft.presentMs = ft.offsetMs + ft.durMovieMs
	}
}

// roundNsToMs converts nanoseconds to milliseconds, rounding half away from
// zero (the same rounding the retime op applies to its shifts).
func roundNsToMs(ns int64) int64 {
	if ns >= 0 {
		return (ns + 500_000) / 1_000_000
	}
	return -((-ns + 500_000) / 1_000_000)
}

func primaryIndex(fts []*fragTrack) int {
	for i := range fts {
		if fts[i].outTrack.spec.video {
			return i
		}
	}
	return 0
}

// renditionInit / renditionSegment / renditionPlaylist name track i's demuxed
// rendition files. The primary rendition (the video, or the first audio of an
// audio-only presentation) keeps the historical names (init.mp4, seg%05d.m4s,
// playlist.m3u8); each other audio rendition is suffixed by its 1-based index
// (init_a1.mp4, seg_a1_00001.m4s, audio1.m3u8).
func renditionInit(fts []*fragTrack, i int) string {
	if i == primaryIndex(fts) {
		return "init.mp4"
	}
	return fmt.Sprintf("init_a%d.mp4", audioIndex(fts, i))
}

func renditionSegment(fts []*fragTrack, i, k int) string {
	if i == primaryIndex(fts) {
		return fmt.Sprintf("seg%05d.m4s", k+1)
	}
	return fmt.Sprintf("seg_a%d_%05d.m4s", audioIndex(fts, i), k+1)
}

func renditionPlaylist(fts []*fragTrack, i int) string {
	if i == primaryIndex(fts) {
		return "playlist.m3u8"
	}
	return fmt.Sprintf("audio%d.m3u8", audioIndex(fts, i))
}

// audioIndex returns track i's 1-based position among the non-primary audio
// renditions.
func audioIndex(fts []*fragTrack, i int) int {
	p := primaryIndex(fts)
	n := 0
	for j := 0; j <= i; j++ {
		if j != p && !fts[j].outTrack.spec.video {
			n++
		}
	}
	return n
}

// segmentWindow returns track i's cursor window for boundary k, advancing the
// cursor: the samples from the cursor to the first one at/past segEnd.
func segmentWindow(ft *fragTrack, cursor *int, segStart, segEnd int64) trackSegment {
	start := *cursor
	j := start
	for j < len(ft.samples) && ft.samples[j].blockPtsMs < segEnd {
		j++
	}
	*cursor = j
	seg := trackSegment{
		trackID: ft.outTrack.mp4ID,
		samples: ft.samples[start:j],
		hasCTS:  windowHasCTS(ft.samples[start:j]),
	}
	if j > start {
		seg.baseDecodeTS = ft.samples[start].dtsTS
		for x := start; x < j; x++ {
			seg.dataLen += int64(ft.samples[x].size)
		}
	} else {
		// An empty window: an empty trun keeps the rendition's segments aligned,
		// timed where the window starts, at the stream end once the track has ended.
		seg.baseDecodeTS = ft.durMediaTS
		if len(ft.samples) > 0 {
			seg.baseDecodeTS = emptyWindowTS(ft.timescale, ft.samples[0].ptsMs, segStart, ft.durMediaTS)
		}
	}
	return seg
}

// iframeRef locates one I-frame for the trick-play playlist: the segment it
// opens (segments are keyframe-cut, so sample 0 IS the I-frame) and the
// fragment that serves it alone, iframeNNNNN.m4s.
type iframeRef struct {
	seg    int    // 0-based segment index
	length int64  // the fragment's bytes
	head   []byte // the fragment's styp + moof + mdat header (on-demand Matroska plans)
	off    int64  // where the keyframe's stored payload starts in the source (on-demand Matroska plans)
	raw    uint32 // the stored payload's size
}

// iframeName is the resource serving segment seg's I-frame.
func iframeName(seg int) string { return fmt.Sprintf("iframe%05d.m4s", seg+1) }

// iframeSegment is seg's leading keyframe alone: what its I-frame fragment frames, a player appending each one
// after the other without the rest of the segment its trun would otherwise list.
func iframeSegment(seg trackSegment) trackSegment {
	one := seg
	one.samples = seg.samples[:1]
	one.dataLen = int64(seg.samples[0].size)
	one.hasCTS = windowHasCTS(one.samples)
	return one
}

// writeSegments writes each rendition's .m4s per boundary interval and returns
// the per-boundary aggregate duration/bytes (across renditions) plus the
// video I-frame references, for the playlists.
func writeSegments(ctx context.Context, o *Options, fs *mkv.FS, dir string, fts []*fragTrack, bounds []int64, src mkv.ReadSeekCloser) ([]segInfo, []iframeRef, error) {
	// Each track's samples in decode order: segments are emitted in order and
	// keyframe-aligned (closed GOPs), so a temp file is read as one forward run.
	media, err := openTrackMedia(fs, fts, src, make([]byte, fullPassBuf), o.Encrypt != nil || o.CENC != nil)
	if err != nil {
		return nil, nil, err
	}
	defer closeTrackMedia(media)

	video := pickVideoFrag(fts)
	cursors := make([]int, len(fts)) // next unwritten sample index per track
	var infos []segInfo
	var iframes []iframeRef
	for k := 0; k < len(bounds); k++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		segStart := bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < len(bounds) {
			segEnd = bounds[k+1]
		}

		var segBytes, audioBytes int64
		for i, ft := range fts {
			from := cursors[i]
			seg := segmentWindow(ft, &cursors[i], segStart, segEnd)
			var cipherData []byte
			if o.CENC != nil {
				// CENC needs the plaintext bytes before the head is built: the
				// segment's moof carries per-sample senc/saiz (sized from the
				// samples themselves), computed here - the same "know the size
				// before the size is needed" two-pass principle buildMoof
				// already applies to trun's data_offset.
				plain, err := media[i].read(seg.samples, from, seg.dataLen)
				if err != nil {
					return nil, nil, err
				}
				td, cipher, err := prepareCENCSegment(o.CENC, ft.outTrack.spec.video, ft.outTrack.mkv.Codec, ft.outTrack.mkv.CodecPrivate, ft.outTrack.mkv.ID, seg.samples, plain)
				if err != nil {
					return nil, nil, err
				}
				seg.cenc = td
				cipherData = cipher
			}
			head := buildSegmentFile(uint32(k+1), seg)
			var ifrHead []byte // this segment's I-frame fragment head, written once the segment is
			if ft == video && !o.NoIFrames && o.Encrypt == nil && o.CENC == nil && len(seg.samples) > 0 && seg.samples[0].sync {
				ifrHead = buildSegmentFile(uint32(k+1), iframeSegment(seg))
				iframes = append(iframes, iframeRef{seg: k, length: int64(len(ifrHead)) + int64(seg.samples[0].size)})
			}
			var fileBytes int64
			switch {
			case o.Encrypt != nil:
				// Whole-segment AES-128-CBC: assemble in memory (one segment at
				// a time), encrypt, write.
				buf, err := media[i].read(seg.samples, from, seg.dataLen)
				if err != nil {
					return nil, nil, err
				}
				plain := make([]byte, 0, int64(len(head))+seg.dataLen)
				plain = append(plain, head...)
				plain = append(plain, buf...)
				enc, err := o.Encrypt.encryptSegment(plain, uint32(k))
				if err != nil {
					return nil, nil, err
				}
				if err := fs.DoWriteFile(filepath.Join(dir, renditionSegment(fts, i, k)), enc, 0o644); err != nil {
					return nil, nil, err
				}
				fileBytes = int64(len(enc))
			case o.CENC != nil:
				out := make([]byte, 0, int64(len(head))+int64(len(cipherData)))
				out = append(out, head...)
				out = append(out, cipherData...)
				if err := fs.DoWriteFile(filepath.Join(dir, renditionSegment(fts, i, k)), out, 0o644); err != nil {
					return nil, nil, err
				}
				fileBytes = int64(len(out))
			default:
				lead, samples, at, rest := head, seg.samples, from, seg.dataLen
				var key []byte // the keyframe, read once for the segment and its I-frame fragment
				if ifrHead != nil {
					if key, err = media[i].read(seg.samples[:1], from, int64(seg.samples[0].size)); err != nil {
						return nil, nil, err
					}
					lead = append(append(make([]byte, 0, len(head)+len(key)), head...), key...)
					samples, at, rest = seg.samples[1:], from+1, seg.dataLen-int64(len(key))
				}
				out, err := fs.DoCreate(filepath.Join(dir, renditionSegment(fts, i, k)))
				if err != nil {
					return nil, nil, err
				}
				werr := media[i].write(out, lead, samples, at, rest)
				if cerr := out.Close(); werr == nil {
					werr = cerr
				}
				if werr != nil {
					return nil, nil, werr
				}
				if ifrHead != nil {
					if err := fs.DoWriteFile(filepath.Join(dir, iframeName(k)), append(ifrHead, key...), 0o644); err != nil {
						return nil, nil, err
					}
				}
				fileBytes = int64(len(head)) + seg.dataLen
			}
			segBytes += fileBytes
			if !ft.outTrack.spec.video {
				audioBytes += fileBytes
			}
		}
		infos = append(infos, segInfo{
			durSec:     float64(segEndOrLast(bounds, k, fts)-segStart) / 1000,
			bytes:      segBytes,
			audioBytes: audioBytes,
		})
	}
	return infos, iframes, nil
}

// buildSegmentFile returns one rendition segment's styp + moof + mdat header;
// the caller appends the seg.dataLen sample bytes after it.
// segmentFileLen is len(buildSegmentFile(seq, seg)) for a segment with no CENC
// data, computed from the box layout instead of building it: a plan needs the
// length of every segment head of every rendition (BANDWIDTH, the I-frame byte
// ranges) and none of their bytes.
func segmentFileLen(seg trackSegment) int64 {
	perSample := int64(12) // duration, size, flags
	if seg.hasCTS {
		perSample += 4
	}
	const (
		styp = smallBoxHeaderLen + 4 + 4 + 3*4
		mfhd = smallBoxHeaderLen + 4 + 4
		tfhd = smallBoxHeaderLen + 4 + 4
		tfdt = smallBoxHeaderLen + 4 + 8
		trun = smallBoxHeaderLen + 4 + 4 + 4 // + the samples
	)
	traf := int64(smallBoxHeaderLen + tfhd + tfdt + trun)
	return styp + smallBoxHeaderLen + mfhd + traf + int64(len(seg.samples))*perSample + mdatHeaderLen
}

func buildSegmentFile(seq uint32, seg trackSegment) []byte {
	moof := buildMoof(seq, []trackSegment{seg})
	out := make([]byte, 0, len(moof)+32)
	out = append(out, buildStyp()...)
	out = append(out, moof...)
	out = append(out, mdatHeader(seg.dataLen)...)
	return out
}

// writeSegmentFile writes the built head then streams dataLen sample bytes
// from the track's sequential temp-file reader.
func writeSegmentFile(out io.Writer, head []byte, dataLen int64, r io.Reader) error {
	m, err := out.Write(head)
	if err == nil && m < len(head) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if dataLen > 0 {
		if _, err := io.CopyN(out, r, dataLen); err != nil {
			return errf("copy segment media: %w", err)
		}
	}
	return nil
}

// windowHasCTS reports whether any sample in the window carries a non-zero
// composition offset - the trun writes its CTS column only then. Window-local
// (not track-global) so the full pass and the on-demand path (hlsplan.go)
// produce identical fragments.
func windowHasCTS(samples []fragSample) bool {
	for i := range samples {
		if samples[i].ctsTS != 0 {
			return true
		}
	}
	return false
}

// segEndOrLast returns the presentation end of segment k: the next boundary, or
// the primary track's highest PTS plus its final sample's duration for the final segment.
func segEndOrLast(bounds []int64, k int, fts []*fragTrack) int64 {
	if k+1 < len(bounds) {
		return bounds[k+1]
	}
	v := fts[primaryIndex(fts)]
	if len(v.samples) == 0 {
		return bounds[len(bounds)-1]
	}
	last := v.samples[len(v.samples)-1]
	endPts := last.ptsMs
	for i := range v.samples {
		if v.samples[i].ptsMs > endPts {
			endPts = v.samples[i].ptsMs
		}
	}
	durMs := last.durTS
	if v.timescale != movieTimescale {
		durMs = last.durTS * int64(movieTimescale) / int64(v.timescale)
	}
	return endPts + durMs
}

// writeMediaPlaylist writes a VOD HLS media playlist. mapURI, when non-empty,
// is emitted as EXT-X-MAP (the fMP4 init segment); WebVTT playlists pass "".
func writeMediaPlaylist(o *Options, fs *mkv.FS, path string, durs []float64, mapURI string, segName func(i int) string) error {
	return fs.DoWriteFile(path, buildMediaPlaylist(o, durs, mapURI, segName, nil), 0o644)
}

// buildMediaPlaylist renders a VOD HLS media playlist. chapters, when
// non-nil (Options.ChapterMarkers, video rendition only), adds one
// #EXT-X-DATERANGE per chapter right after EXT-X-PLAYLIST-TYPE - every
// caller that has no chapters to carry (audio/subtitle renditions, or the
// option off) passes nil and the playlist is unchanged from before the
// option existed.
func buildMediaPlaylist(o *Options, durs []float64, mapURI string, segName func(i int) string, chapters []mkv.Chapter) []byte {
	rw := urlRewriter(o)
	var max float64
	for _, d := range durs {
		if d > max {
			max = d
		}
	}
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int64(max+0.999))...)
	b = append(b, "#EXT-X-PLAYLIST-TYPE:VOD\n"...)
	if len(chapters) > 0 {
		b = append(b, buildChapterDateRanges(chapters)...)
	}
	if mapURI != "" {
		// EXT-X-MAP MUST precede any EXT-X-KEY: per RFC 8216 a key applies to
		// every EXT-X-MAP that follows it, but the init segment (moov) is always
		// in the clear even when the media samples are encrypted. Emitting the
		// key first made players (hls.js) try to decrypt the clear init and fail
		// in a retry loop. With the map first, the init carries no key and the
		// key below applies only to the media segments that follow it. Subtitle
		// playlists (mapURI == "") carry neither a map nor a key.
		b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%s\n", hlsURI(rw(mapURI)))...)
	}
	// keyLineFor returns the EXT-X-KEY governing segment i, or "" for a clear /
	// subtitle playlist. AES-128 may rotate the key across the presentation, so
	// this is evaluated per segment and emitted only when it changes (below):
	// one key prints once, a rotating schedule prints at each period boundary.
	keyLineFor := func(i int) string {
		if mapURI == "" {
			return ""
		}
		switch {
		case o != nil && o.Encrypt != nil:
			return o.Encrypt.keyLineForSegment(i)
		case o != nil && o.CENC != nil:
			return o.CENC.keyLine()
		}
		return ""
	}
	var lastKey string
	for i, d := range durs {
		if kl := keyLineFor(i); kl != "" && kl != lastKey {
			b = append(b, kl...)
			lastKey = kl
		}
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n%s\n", d, uriLine(rw(segName(i))))...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return b
}

// urlRewriter returns the Options.RewriteURL hook, or the identity.
func urlRewriter(o *Options) func(string) string {
	if o != nil && o.RewriteURL != nil {
		return o.RewriteURL
	}
	return func(s string) string { return s }
}

// writeSubtitleRendition writes one subtitle track as segmented WebVTT: one
// .vtt per media-segment window (cues overlapping the window, absolute
// timestamps) plus its media playlist. Options.SubtitleOffsetMs shifts every
// cue (see subtitle.ShiftCues) before windowing, so a re-synced track lands
// in the right segment and, for a zero offset, is byte-identical to before.
func writeSubtitleRendition(o *Options, fs *mkv.FS, dir string, idx int, st *hlsSubTrack, bounds []int64, durs []float64, fts []*fragTrack) error {
	subtitle.SettleCueEnds(st.cues, 0)
	cues := subtitle.ShiftCues(st.cues, o.SubtitleOffsetMs)
	for k := range durs {
		segStart := bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < len(bounds) {
			segEnd = bounds[k+1]
		}
		var window []subtitle.Cue
		for _, cue := range cues {
			if cue.EndMs > segStart && cue.StartMs < segEnd {
				window = append(window, cue)
			}
		}
		var buf strings.Builder
		if err := subtitle.WriteWebVTT(&buf, window); err != nil {
			return err
		}
		name := fmt.Sprintf("sub%d_%05d.vtt", idx+1, k+1)
		if err := fs.DoWriteFile(filepath.Join(dir, name), []byte(buf.String()), 0o644); err != nil {
			return err
		}
	}
	// The whole rendition as one file too - what the DASH manifest references.
	var whole strings.Builder
	if err := subtitle.WriteWebVTT(&whole, cues); err != nil {
		return err
	}
	if err := fs.DoWriteFile(filepath.Join(dir, fmt.Sprintf("sub%d.vtt", idx+1)), []byte(whole.String()), 0o644); err != nil {
		return err
	}
	return writeMediaPlaylist(o, fs, filepath.Join(dir, fmt.Sprintf("sub%d.m3u8", idx+1)), durs, "",
		func(i int) string { return fmt.Sprintf("sub%d_%05d.vtt", idx+1, i+1) })
}

// writeMasterPlaylist writes the multivariant playlist: the video rendition
// (BANDWIDTH from the peak aggregate segment bitrate, RESOLUTION/FRAME-RATE,
// CODECS when every track's RFC 6381 string is known), each audio track as an
// EXT-X-MEDIA AUDIO rendition, and the subtitle renditions as another group.
func writeMasterPlaylist(o *Options, fs *mkv.FS, dir string, fts []*fragTrack, subs []hlsSubTrack, segs []segInfo, iframes []iframeRef) error {
	return fs.DoWriteFile(filepath.Join(dir, "master.m3u8"), buildMasterPlaylist(o, fts, subs, segs, iframes), 0o644)
}

// peakBandwidth returns the highest per-segment bitrate in bits/s.
func peakBandwidth(segs []segInfo) int64 {
	var peak float64
	for _, s := range segs {
		if s.durSec > 0 {
			if bps := float64(s.bytes) * 8 / s.durSec; bps > peak {
				peak = bps
			}
		}
	}
	return int64(peak)
}

func buildMasterPlaylist(o *Options, fts []*fragTrack, subs []hlsSubTrack, segs []segInfo, iframes []iframeRef) []byte {
	rw := urlRewriter(o)
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n"...)

	// Audio renditions (demuxed): one EXT-X-MEDIA per audio track; the first
	// (or the flagged default) is DEFAULT=YES so players auto-select it.
	prim := primaryIndex(fts)
	nAudio := 0
	hasDefaultAudio := false
	for i, ft := range fts {
		hasDefaultAudio = hasDefaultAudio || (i != prim && !ft.outTrack.spec.video && ft.outTrack.mkv.IsDefault)
	}
	for i, ft := range fts {
		if i == prim || ft.outTrack.spec.video {
			continue
		}
		nAudio++
		t := &ft.outTrack.mkv
		name := t.Name
		if name == "" && t.ResolvedLanguage() != "" {
			name = t.ResolvedLanguage()
		}
		if name == "" {
			name = fmt.Sprintf("Audio %d", nAudio)
		}
		attrs := fmt.Sprintf("TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%s,AUTOSELECT=YES", hlsQuoted(name))
		if l := t.ResolvedLanguage(); l != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%s", hlsQuoted(l))
		}
		attrs += hlsChannelsAttr(t)
		if t.IsDefault || (!hasDefaultAudio && nAudio == 1) {
			attrs += ",DEFAULT=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%s\n", attrs, hlsURI(rw(renditionPlaylist(fts, i))))...)
	}

	for i := range subs {
		t := &subs[i].track
		name := t.Name
		if name == "" && t.ResolvedLanguage() != "" {
			name = t.ResolvedLanguage()
		}
		if name == "" {
			name = fmt.Sprintf("Subtitles %d", i+1)
		}
		attrs := fmt.Sprintf("TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=%s,AUTOSELECT=YES", hlsQuoted(name))
		if l := t.ResolvedLanguage(); l != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%s", hlsQuoted(l))
		}
		if t.IsDefault {
			attrs += ",DEFAULT=YES"
		}
		if t.IsForced {
			attrs += ",FORCED=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%s\n", attrs, hlsURI(rw(fmt.Sprintf("sub%d.m3u8", i+1))))...)
	}

	// Peak and average bandwidth over the media segments (bits per second).
	var peak, totalBits float64
	var totalSec float64
	for _, s := range segs {
		if s.durSec > 0 {
			if bps := float64(s.bytes) * 8 / s.durSec; bps > peak {
				peak = bps
			}
		}
		totalBits += float64(s.bytes) * 8
		totalSec += s.durSec
	}
	avg := peak
	if totalSec > 0 {
		avg = totalBits / totalSec
	}
	inf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d", int64(peak), int64(avg))
	if v := pickVideoFrag(fts); v != nil {
		t := &v.outTrack.mkv
		if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
			inf += fmt.Sprintf(",RESOLUTION=%dx%d", *t.Width, *t.Height)
		}
		if t.FrameRate != nil && *t.FrameRate > 0 {
			inf += fmt.Sprintf(",FRAME-RATE=%.3f", *t.FrameRate)
		}
	}
	if codecs := hlsCodecsAttr(fts); codecs != "" {
		inf += fmt.Sprintf(",CODECS=%s", hlsQuoted(codecs))
	}
	if nAudio > 0 {
		inf += ",AUDIO=\"aud\""
	}
	if len(subs) > 0 {
		inf += ",SUBTITLES=\"subs\""
	}
	b = append(b, (inf + "\n" + uriLine(rw("playlist.m3u8")) + "\n")...)
	if len(iframes) > 0 && o != nil && o.Encrypt == nil && o.CENC == nil && !o.NoIFrames {
		b = append(b, iframeStreamInf(o, fts, dursOf(segs), iframes)...)
	}
	return b
}

// dursOf extracts the per-segment durations.
func dursOf(segs []segInfo) []float64 {
	durs := make([]float64, len(segs))
	for i := range segs {
		durs[i] = segs[i].durSec
	}
	return durs
}

// buildIFramePlaylist renders the trick-play playlist (EXT-X-I-FRAMES-ONLY):
// one entry per segment-leading keyframe, its fragment of its own (a byte
// range of the segment would hand a player a trun listing samples it lacks).
func buildIFramePlaylist(o *Options, fts []*fragTrack, durs []float64, iframes []iframeRef) []byte {
	rw := urlRewriter(o)
	var max float64
	for _, d := range durs {
		if d > max {
			max = d
		}
	}
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-I-FRAMES-ONLY\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int64(max+0.999))...)
	b = append(b, "#EXT-X-PLAYLIST-TYPE:VOD\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%s\n", hlsURI(rw("init.mp4")))...)
	for _, ifr := range iframes {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n%s\n", durs[ifr.seg], uriLine(rw(iframeName(ifr.seg))))...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return b
}

// iframeStreamInf renders the master's EXT-X-I-FRAME-STREAM-INF line: the
// trick-play variant's BANDWIDTH is its peak I-frame bitrate.
func iframeStreamInf(o *Options, fts []*fragTrack, durs []float64, iframes []iframeRef) string {
	return iframeStreamInfURI(o, fts, durs, iframes, "iframe.m3u8")
}

// iframeStreamInfURI is iframeStreamInf with an explicit playlist URI, so the
// ABR master can point trick-play at a variant's v{k}/iframe.m3u8.
func iframeStreamInfURI(o *Options, fts []*fragTrack, durs []float64, iframes []iframeRef, uri string) string {
	rw := urlRewriter(o)
	var peak float64
	for _, ifr := range iframes {
		if d := durs[ifr.seg]; d > 0 {
			if bps := float64(ifr.length) * 8 / d; bps > peak {
				peak = bps
			}
		}
	}
	inf := fmt.Sprintf("#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=%d", int64(peak))
	if v := pickVideoFrag(fts); v != nil {
		t := &v.outTrack.mkv
		if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
			inf += fmt.Sprintf(",RESOLUTION=%dx%d", *t.Width, *t.Height)
		}
		if cs := rfc6381Codec(v.outTrack); cs != "" {
			inf += fmt.Sprintf(",CODECS=%s", hlsQuoted(cs))
		}
	}
	return inf + fmt.Sprintf(",URI=%s\n", hlsURI(rw(uri)))
}

// isPackagedName reports whether rel, relative to the output directory, names a file a packaging pass writes:
// playlists, init and media segments, subtitles, the manifest, single-file streams, their temporary files, under an
// ABR or concat prefix (v<n>/, p<n>/). Written out rather than as a regexp, which the WebAssembly build would carry.
func isPackagedName(rel string) bool {
	if len(rel) > 0 && (rel[0] == 'v' || rel[0] == 'p') {
		if r, ok := leadingDigits(rel[1:]); ok && strings.HasPrefix(r, "/") {
			rel = r[1:]
		}
	}
	switch rel {
	case "master.m3u8", "playlist.m3u8", "iframe.m3u8", "manifest.mpd":
		return true
	}
	if r, ok := strings.CutPrefix(rel, "audio"); ok {
		r, ok = leadingDigits(r)
		return ok && r == ".m3u8"
	}
	if r, ok := strings.CutPrefix(rel, "sub"); ok {
		if r, ok = leadingDigits(r); !ok {
			return false
		}
		if strings.HasPrefix(r, "_") {
			if r, ok = leadingDigits(r[1:]); !ok {
				return false
			}
		}
		return r == ".m3u8" || r == ".vtt"
	}
	for _, name := range []string{"init", "stream"} {
		if r, ok := strings.CutPrefix(rel, name); ok {
			if a, ok := strings.CutPrefix(r, "_a"); ok {
				if r, ok = leadingDigits(a); !ok {
					return false
				}
			}
			return r == ".mp4"
		}
	}
	if r, ok := strings.CutPrefix(rel, "iframe"); ok {
		r, ok = leadingDigits(r)
		return ok && r == ".m4s"
	}
	if r, ok := strings.CutPrefix(rel, "seg"); ok {
		if a, ok := strings.CutPrefix(r, "_a"); ok {
			if r, ok = leadingDigits(a); !ok || !strings.HasPrefix(r, "_") {
				return false
			}
			r = r[1:]
		}
		r, ok = leadingDigits(r)
		return ok && r == ".m4s"
	}
	if r, ok := strings.CutPrefix(rel, ".mkvgo-hls-t"); ok {
		r, ok = leadingDigits(r)
		return ok && r == ".tmp"
	}
	return false
}

// leadingDigits drops the ASCII digits s opens with, ok when there was at least one.
func leadingDigits(s string) (rest string, ok bool) {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return s[n:], n > 0
}

// refusePackagedSource refuses a source lying in outputDir under a name the packaging writes: it would be written
// over before, or while, its samples are read.
func refusePackagedSource(outputDir string, sources ...string) error {
	dir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil
	}
	for _, src := range sources {
		abs, err := filepath.Abs(src)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(dir, abs); err == nil && isPackagedName(filepath.ToSlash(rel)) {
			return errf("%s would be written over by the packaging into %s: move it, or package into another directory", shown.Path(src), outputDir)
		}
	}
	return nil
}
