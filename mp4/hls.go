package mp4

// hls.go — RemuxToHLS: a fragmented-MP4 / CMAF presentation produced from a
// Matroska/WebM file without transcoding, served through two manifests over
// the SAME segments (the point of CMAF): HLS (master.m3u8 + media playlists)
// and DASH (manifest.mpd). Tracks are demuxed — one rendition per track, as
// Apple recommends for HLS and as DASH players require — which also gives
// multi-audio (VF/VO) selection for free. This is the "copy rung" of an ABR
// ladder: mkvgo does the packaging; bitrate variants remain a transcoder's job.
//
// Memory is bounded: per-sample metadata (size/pts/sync — the same order of
// magnitude the progressive muxer already holds) is kept in RAM, while the
// sample *bytes* are streamed to one temp file per track and read back
// sequentially when the segments are written. Subtitle cues are small text and
// are held in RAM. Nothing holds the whole media.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

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
func isBlockWalkEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// keepTrackSet returns Options.KeepTracks as a lookup set (the Virtual Edit
// Layer's track filter), or nil when empty — meaning keep every track.
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

// fragTrack augments an outTrack with the fragmented-writer state: the media
// timescale, the collected samples, the temp file holding their bytes, and the
// derived durations/offset.
type fragTrack struct {
	outTrack   *outTrack
	timescale  uint32
	samples    []fragSample
	tmp        mkv.WriteSeekCloser
	tmpPath    string
	offsetMs   int64
	durMediaTS int64
	durMovieMs int64
	presentMs  int64
	hasCTS     bool
}

// hlsSubTrack is one text subtitle track carried as a segmented WebVTT
// playlist (HLS's native subtitle form — text subs are not fragmented as mdat).
type hlsSubTrack struct {
	track mkv.Track
	cues  []subtitle.Cue
}

// segInfo describes one written media segment, for the playlists: its duration
// (seconds) and its total byte size (BANDWIDTH computation).
type segInfo struct {
	durSec float64
	bytes  int64
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
// It never transcodes — samples are copied verbatim into CMAF fragments — so
// only the codecs RemuxToMP4 supports are carried. Segments are cut on video
// keyframes at roughly Options.SegmentMs (default 6 s) and are independently
// decodable. Text subtitles (SRT, WebVTT, ASS/SSA flattened to plain text) ride
// as WebVTT renditions; bitmap subtitles (PGS/VOBSUB) are dropped and reported
// via Options.OnDrop.
func RemuxToHLS(ctx context.Context, srcPath, outputDir string, opts ...Options) error {
	o := optionsFrom(opts)
	_, err := remuxToHLSInto(ctx, srcPath, outputDir, &o)
	return err
}

// hlsResult carries what a packaging pass established — the ABR packager
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

	ps, err := openPackagingSource(ctx, srcPath, fs)
	if err != nil {
		return nil, err
	}
	defer ps.Close()
	c := ps.c
	planned, _, err := planTracks(c, o)
	if err != nil {
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
	if err := cencPreflight(&o, videoCodecOf(media)); err != nil {
		return nil, err
	}
	var subs []hlsSubTrack
	if !o.VideoOnly {
		subs = filterSubTracks(planSubTracks(c, o), keep)
	}

	if err := fs.DoMkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}

	fts := make([]*fragTrack, len(media))
	routing := make(map[uint64]*fragTrack, len(media))
	for i, t := range media {
		tmpPath := filepath.Join(outputDir, fmt.Sprintf(".mkvgo-hls-t%d.tmp", t.mp4ID))
		tmp, cerr := fs.DoCreate(tmpPath)
		if cerr != nil {
			return nil, cerr
		}
		fts[i] = &fragTrack{outTrack: t, timescale: mediaTimescale(t), tmp: tmp, tmpPath: tmpPath}
		routing[t.mkv.ID] = fts[i]
	}
	// Best-effort cleanup of the per-track temp files.
	defer func() {
		for _, ft := range fts {
			if ft.tmp != nil {
				ft.tmp.Close()
			}
			_ = fs.DoRemove(ft.tmpPath)
		}
	}()

	// Phase 1 — stream every sample, buffering media bytes to the per-track
	// temp files and collecting subtitle cues (source-specific walk).
	if ps.mv != nil {
		err = collectFromMP4(ctx, ps, routing, subs, o.Progress)
	} else {
		err = collectFragSamples(ctx, srcPath, fs, c, routing, subs, o.Progress)
	}
	if err != nil {
		return nil, err
	}
	for _, ft := range fts {
		if len(ft.samples) == 0 {
			return nil, errf("track %d produced no samples", ft.outTrack.mp4ID)
		}
		if cerr := ft.tmp.Close(); cerr != nil {
			return nil, cerr
		}
		ft.tmp = nil
		off, hasCTS, totalTS := fillFragTiming(ft.samples, ft.outTrack.frameDurMs, ft.timescale,
			audioGridTS(ft.outTrack, ft.timescale))
		ft.offsetMs, ft.hasCTS, ft.durMediaTS = off, hasCTS, totalTS
		ft.durMovieMs = totalTS
		if ft.timescale != movieTimescale {
			ft.durMovieMs = totalTS * int64(movieTimescale) / int64(ft.timescale)
		}
		ft.presentMs = off + ft.durMovieMs
	}

	// Phase 2 — segment boundaries from the primary track: the video's
	// keyframes, or (audio-only presentation) the first audio's sample grid
	// (every audio sample is a sync point, so the same cut applies).
	bounds := segmentBoundaries(fts[primaryIndex(fts)].samples, segMs)

	meta := movieMeta{title: c.Info.Title, tags: globalTags(c), cover: pickCoverArt(c.Attachments)}

	var segs []segInfo
	var rends []sfRendition
	var iframesAll []iframeRef
	if o.SingleFile {
		// One progressive file per rendition (init + sidx + fragments); the
		// playlists reference it by byte ranges.
		segs, rends, err = writeSingleFileRenditions(ctx, &o, fs, outputDir, fts, bounds, meta)
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
		segs, iframesAll, err = writeSegments(ctx, &o, fs, outputDir, fts, bounds)
		if err != nil {
			return nil, err
		}
		// Trick-play: the standard I-frame playlist — one keyframe per
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
	routing map[uint64]*fragTrack, subs []hlsSubTrack, progress mkv.ProgressFunc) error {
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if isBlockWalkEnd(err) {
			break
		}
		if err != nil {
			return errf("read block: %w", err)
		}
		if st, ok := subRouting[b.TrackNumber]; ok {
			if cue, ok := subCueFromBlock(st.track.Codec, b); ok {
				st.cues = append(st.cues, cue)
			}
			continue
		}
		ft, ok := routing[b.TrackNumber]
		if !ok {
			continue
		}
		data := ft.outTrack.mkv.RestoreHeader(b.Data)
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

// subCueFromBlock converts one Matroska subtitle block into a WebVTT cue. ASS
// dialogue lines are flattened to their plain text (styling is lost — WebVTT
// has no ASS form); SRT and WebVTT payloads pass through.
func subCueFromBlock(codec string, b mkv.Block) (subtitle.Cue, bool) {
	var text string
	switch canonicalSubCodec(codec) {
	case "ass", "ssa":
		text = subtitle.FlattenASSBlock(b.Data)
	default: // srt, webvtt: the block payload is the cue text
		text = strings.TrimRight(string(b.Data), "\n\r\x00")
	}
	if strings.TrimSpace(text) == "" {
		return subtitle.Cue{}, false
	}
	cue := subtitle.Cue{StartMs: b.Timecode, Text: text}
	if b.Duration > 0 {
		cue.EndMs = b.Timecode + b.Duration
	}
	return cue, true
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
// video track, or — audio-only presentation — the first track.
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
func segmentWindow(ft *fragTrack, cursor *int, segEnd int64) trackSegment {
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
		// An empty window (the track ended before the presentation): an empty
		// trun keeps the rendition's segments aligned; decode time = stream end.
		seg.baseDecodeTS = ft.durMediaTS
	}
	return seg
}

// iframeRef locates one I-frame for the trick-play playlist: the containing
// segment and the byte length from the segment start through the keyframe
// sample (styp + moof + mdat header + first sample — segments are
// keyframe-cut, so sample 0 IS the I-frame).
type iframeRef struct {
	seg    int   // 0-based segment index
	length int64 // bytes from offset 0 covering the keyframe
}

// writeSegments writes each rendition's .m4s per boundary interval and returns
// the per-boundary aggregate duration/bytes (across renditions) plus the
// video I-frame references, for the playlists.
func writeSegments(ctx context.Context, o *Options, fs *mkv.FS, dir string, fts []*fragTrack, bounds []int64) ([]segInfo, []iframeRef, error) {
	// A sequential reader over each track's temp file: samples are stored in
	// decode order, segments are emitted in order and keyframe-aligned (closed
	// GOPs), so each segment's bytes are a contiguous forward run.
	readers := make([]mkv.ReadSeekCloser, len(fts))
	for i, ft := range fts {
		r, err := fs.DoOpen(ft.tmpPath)
		if err != nil {
			return nil, nil, err
		}
		readers[i] = r
	}
	defer func() {
		for _, r := range readers {
			if r != nil {
				r.Close()
			}
		}
	}()

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

		var segBytes int64
		for i, ft := range fts {
			seg := segmentWindow(ft, &cursors[i], segEnd)
			var cipherData []byte
			if o.CENC != nil {
				// CENC needs the plaintext bytes before the head is built: the
				// segment's moof carries per-sample senc/saiz (sized from the
				// samples themselves), computed here - the same "know the size
				// before the size is needed" two-pass principle buildMoof
				// already applies to trun's data_offset.
				plain := make([]byte, seg.dataLen)
				if seg.dataLen > 0 {
					if _, err := io.ReadFull(readers[i], plain); err != nil {
						return nil, nil, errf("read segment media: %w", err)
					}
				}
				td, cipher, err := prepareCENCSegment(o.CENC, ft.outTrack.spec.video, ft.outTrack.mkv.Codec, seg.samples, plain)
				if err != nil {
					return nil, nil, err
				}
				seg.cenc = td
				cipherData = cipher
			}
			head := buildSegmentFile(uint32(k+1), seg)
			if ft == video && len(seg.samples) > 0 && seg.samples[0].sync {
				iframes = append(iframes, iframeRef{seg: k, length: int64(len(head)) + int64(seg.samples[0].size)})
			}
			var fileBytes int64
			switch {
			case o.Encrypt != nil:
				// Whole-segment AES-128-CBC: assemble in memory (one segment at
				// a time), encrypt, write.
				plain := make([]byte, 0, int64(len(head))+seg.dataLen)
				plain = append(plain, head...)
				if seg.dataLen > 0 {
					buf := make([]byte, seg.dataLen)
					if _, err := io.ReadFull(readers[i], buf); err != nil {
						return nil, nil, errf("read segment media: %w", err)
					}
					plain = append(plain, buf...)
				}
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
				out, err := fs.DoCreate(filepath.Join(dir, renditionSegment(fts, i, k)))
				if err != nil {
					return nil, nil, err
				}
				werr := writeSegmentFile(out, head, seg.dataLen, readers[i])
				if cerr := out.Close(); werr == nil {
					werr = cerr
				}
				if werr != nil {
					return nil, nil, werr
				}
				fileBytes = int64(len(head)) + seg.dataLen
			}
			segBytes += fileBytes
		}
		infos = append(infos, segInfo{
			durSec: float64(segEndOrLast(bounds, k, fts)-segStart) / 1000,
			bytes:  segBytes,
		})
	}
	return infos, iframes, nil
}

// buildSegmentFile returns one rendition segment's styp + moof + mdat header;
// the caller appends the seg.dataLen sample bytes after it.
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
	if _, err := out.Write(head); err != nil {
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
// composition offset — the trun writes its CTS column only then. Window-local
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
// the primary track's last PTS+duration for the final segment.
func segEndOrLast(bounds []int64, k int, fts []*fragTrack) int64 {
	if k+1 < len(bounds) {
		return bounds[k+1]
	}
	v := fts[primaryIndex(fts)]
	if len(v.samples) == 0 {
		return bounds[len(bounds)-1]
	}
	last := v.samples[len(v.samples)-1]
	durMs := last.durTS
	if v.timescale != movieTimescale {
		durMs = last.durTS * int64(movieTimescale) / int64(v.timescale)
	}
	return last.ptsMs + durMs
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
	if o != nil && mapURI != "" {
		// Media segments only (fMP4 renditions); the init and subtitle files
		// stay clear, so subtitle playlists (mapURI == "") carry no key line.
		switch {
		case o.Encrypt != nil:
			b = append(b, o.Encrypt.keyLine()...)
		case o.CENC != nil:
			b = append(b, o.CENC.keyLine()...)
		}
	}
	if mapURI != "" {
		b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q\n", rw(mapURI))...)
	}
	for i, d := range durs {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n%s\n", d, rw(segName(i)))...)
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
	subtitle.ResolveCueEnds(st.cues, 2000)
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
	// The whole rendition as one file too — what the DASH manifest references.
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
		if name == "" && t.Language != "" {
			name = t.Language
		}
		if name == "" {
			name = fmt.Sprintf("Audio %d", nAudio)
		}
		attrs := fmt.Sprintf("TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%q,AUTOSELECT=YES", name)
		if t.Language != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%q", t.Language)
		}
		if t.IsDefault || (!hasDefaultAudio && nAudio == 1) {
			attrs += ",DEFAULT=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw(renditionPlaylist(fts, i)))...)
	}

	for i := range subs {
		t := &subs[i].track
		name := t.Name
		if name == "" && t.Language != "" {
			name = t.Language
		}
		if name == "" {
			name = fmt.Sprintf("Subtitles %d", i+1)
		}
		attrs := fmt.Sprintf("TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=%q,AUTOSELECT=YES", name)
		if t.Language != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%q", t.Language)
		}
		if t.IsDefault {
			attrs += ",DEFAULT=YES"
		}
		if t.IsForced {
			attrs += ",FORCED=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw(fmt.Sprintf("sub%d.m3u8", i+1)))...)
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
		inf += fmt.Sprintf(",CODECS=%q", codecs)
	}
	if nAudio > 0 {
		inf += ",AUDIO=\"aud\""
	}
	if len(subs) > 0 {
		inf += ",SUBTITLES=\"subs\""
	}
	b = append(b, (inf + "\n" + rw("playlist.m3u8") + "\n")...)
	if len(iframes) > 0 && o != nil && o.Encrypt == nil && o.CENC == nil {
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
// one entry per segment-leading keyframe, as a byte range from the segment
// start through the keyframe sample (styp + moof + mdat header + sample 0 —
// what a player needs to decode just that I-frame).
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
	b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q\n", rw("init.mp4"))...)
	for _, ifr := range iframes {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n#EXT-X-BYTERANGE:%d@0\n%s\n",
			durs[ifr.seg], ifr.length, rw(fmt.Sprintf("seg%05d.m4s", ifr.seg+1)))...)
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
			inf += fmt.Sprintf(",CODECS=%q", cs)
		}
	}
	return inf + fmt.Sprintf(",URI=%q\n", rw(uri))
}
