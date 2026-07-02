package mp4

// hls.go — RemuxToHLS: a fragmented-MP4 / CMAF HLS presentation (master.m3u8,
// init.mp4, segNNNNN.m4s, playlist.m3u8, plus one segmented WebVTT playlist per
// text subtitle track) produced from a Matroska/WebM file without transcoding.
// This is the "copy rung" of an HLS ladder: mkvgo does the CMAF packaging;
// bitrate variants (real ABR) remain a transcoder's job.
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
func RemuxToHLS(ctx context.Context, srcPath, outputDir string, opts ...Options) (err error) {
	o := optionsFrom(opts)
	fs := o.FS
	segMs := o.SegmentMs
	if segMs <= 0 {
		segMs = defaultSegmentMs
	}

	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}
	planned, _, err := planTracks(c, o)
	if err != nil {
		return err
	}
	// The fragmented rendition carries audio+video; text subtitles become
	// WebVTT renditions instead of mdat tracks.
	var media []*outTrack
	for _, t := range planned {
		if t.isChapter || t.spec.text {
			continue
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return errf("no audio or video track to segment")
	}
	subs := planSubTracks(c, o)

	if err := fs.DoMkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	fts := make([]*fragTrack, len(media))
	routing := make(map[uint64]*fragTrack, len(media))
	for i, t := range media {
		tmpPath := filepath.Join(outputDir, fmt.Sprintf(".mkvgo-hls-t%d.tmp", t.mp4ID))
		tmp, cerr := fs.DoCreate(tmpPath)
		if cerr != nil {
			return cerr
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

	// Phase 1 — stream every block, buffering media sample bytes to the
	// per-track temp files and collecting subtitle cues.
	if err := collectFragSamples(ctx, srcPath, fs, c, routing, subs, o.Progress); err != nil {
		return err
	}
	for _, ft := range fts {
		if len(ft.samples) == 0 {
			return errf("track %d produced no samples", ft.outTrack.mp4ID)
		}
		if cerr := ft.tmp.Close(); cerr != nil {
			return cerr
		}
		ft.tmp = nil
		off, hasCTS, totalTS := fillFragTiming(ft.samples, ft.outTrack.frameDurMs, ft.timescale)
		ft.offsetMs, ft.hasCTS, ft.durMediaTS = off, hasCTS, totalTS
		ft.durMovieMs = totalTS
		if ft.timescale != movieTimescale {
			ft.durMovieMs = totalTS * int64(movieTimescale) / int64(ft.timescale)
		}
		ft.presentMs = off + ft.durMovieMs
	}

	// Phase 2 — segment boundaries from the video track's keyframes.
	video := pickVideoFrag(fts)
	if video == nil {
		return errf("HLS output requires a video track")
	}
	bounds := segmentBoundaries(video.samples, segMs)

	meta := movieMeta{title: c.Info.Title, tags: globalTags(c), cover: pickCoverArt(c.Attachments)}
	initData := buildInitSegment(fts, meta)
	if err := fs.DoWriteFile(filepath.Join(outputDir, "init.mp4"), initData, 0o644); err != nil {
		return err
	}

	segs, err := writeSegments(ctx, fs, outputDir, fts, bounds)
	if err != nil {
		return err
	}
	durs := make([]float64, len(segs))
	for i := range segs {
		durs[i] = segs[i].durSec
	}
	if err := writeMediaPlaylist(fs, filepath.Join(outputDir, "playlist.m3u8"), durs, "init.mp4",
		func(i int) string { return fmt.Sprintf("seg%05d.m4s", i+1) }); err != nil {
		return err
	}
	for i := range subs {
		if err := writeSubtitleRendition(fs, outputDir, i, &subs[i], bounds, durs, fts); err != nil {
			return err
		}
	}
	return writeMasterPlaylist(fs, outputDir, fts, subs, segs)
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
		if errors.Is(err, io.EOF) {
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
		ft.samples = append(ft.samples, fragSample{size: uint32(len(data)), ptsMs: b.Timecode, sync: b.Keyframe})
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
		if video[i].ptsMs >= last+targetMs {
			bounds = append(bounds, video[i].ptsMs)
			last = video[i].ptsMs
		}
	}
	return bounds
}

// writeSegments writes one .m4s per boundary interval and returns each segment's
// duration and byte size, for the playlists.
func writeSegments(ctx context.Context, fs *mkv.FS, dir string, fts []*fragTrack, bounds []int64) ([]segInfo, error) {
	// A sequential reader over each track's temp file: samples are stored in
	// decode order, segments are emitted in order and keyframe-aligned (closed
	// GOPs), so each segment's bytes are a contiguous forward run.
	readers := make([]mkv.ReadSeekCloser, len(fts))
	for i, ft := range fts {
		r, err := fs.DoOpen(ft.tmpPath)
		if err != nil {
			return nil, err
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

	cursors := make([]int, len(fts)) // next unwritten sample index per track
	var infos []segInfo
	for k := 0; k < len(bounds); k++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		segStart := bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < len(bounds) {
			segEnd = bounds[k+1]
		}

		var segs []trackSegment
		for i, ft := range fts {
			start := cursors[i]
			j := start
			for j < len(ft.samples) && ft.samples[j].ptsMs < segEnd {
				j++
			}
			if j == start {
				continue // no samples for this track in this interval
			}
			seg := trackSegment{
				trackID:      ft.outTrack.mp4ID,
				baseDecodeTS: ft.samples[start].dtsTS,
				samples:      ft.samples[start:j],
				hasCTS:       ft.hasCTS,
			}
			for x := start; x < j; x++ {
				seg.dataLen += int64(ft.samples[x].size)
			}
			segs = append(segs, seg)
			cursors[i] = j
		}
		if len(segs) == 0 {
			continue
		}

		moof := buildMoof(uint32(k+1), segs)
		name := fmt.Sprintf("seg%05d.m4s", k+1)
		out, err := fs.DoCreate(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		werr := writeOneSegment(out, moof, segs, fts, readers)
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
		}

		var segBytes int64
		for i := range segs {
			segBytes += segs[i].dataLen
		}
		segBytes += int64(len(moof)) + int64(len(buildStyp())) + mdatHeaderLen
		infos = append(infos, segInfo{
			durSec: float64(segEndOrLast(bounds, k, fts)-segStart) / 1000,
			bytes:  segBytes,
		})
	}
	return infos, nil
}

// writeOneSegment writes styp + moof + mdat(sample bytes) for one segment. The
// mdat sample bytes are read sequentially from each track's temp file in the same
// order the trafs appear, matching the data_offsets buildMoof computed.
func writeOneSegment(out io.Writer, moof []byte, segs []trackSegment, fts []*fragTrack, readers []mkv.ReadSeekCloser) error {
	if _, err := out.Write(buildStyp()); err != nil {
		return err
	}
	if _, err := out.Write(moof); err != nil {
		return err
	}
	var mdatData int64
	for i := range segs {
		mdatData += segs[i].dataLen
	}
	if _, err := out.Write(mdatHeader(mdatData)); err != nil {
		return err
	}
	// Sample bytes, per traf, read from the matching track's sequential reader.
	for i := range segs {
		ri := trackReaderIndex(fts, segs[i].trackID)
		if _, err := io.CopyN(out, readers[ri], segs[i].dataLen); err != nil {
			return errf("copy segment media: %w", err)
		}
	}
	return nil
}

func trackReaderIndex(fts []*fragTrack, trackID uint32) int {
	for i, ft := range fts {
		if ft.outTrack.mp4ID == trackID {
			return i
		}
	}
	return 0
}

// segEndOrLast returns the presentation end of segment k: the next boundary, or
// the video track's last PTS+duration for the final segment.
func segEndOrLast(bounds []int64, k int, fts []*fragTrack) int64 {
	if k+1 < len(bounds) {
		return bounds[k+1]
	}
	v := pickVideoFrag(fts)
	if v == nil || len(v.samples) == 0 {
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
func writeMediaPlaylist(fs *mkv.FS, path string, durs []float64, mapURI string, segName func(i int) string) error {
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
	if mapURI != "" {
		b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q\n", mapURI)...)
	}
	for i, d := range durs {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n%s\n", d, segName(i))...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return fs.DoWriteFile(path, b, 0o644)
}

// writeSubtitleRendition writes one subtitle track as segmented WebVTT: one
// .vtt per media-segment window (cues overlapping the window, absolute
// timestamps) plus its media playlist.
func writeSubtitleRendition(fs *mkv.FS, dir string, idx int, st *hlsSubTrack, bounds []int64, durs []float64, fts []*fragTrack) error {
	subtitle.ResolveCueEnds(st.cues, 2000)
	for k := range durs {
		segStart := bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < len(bounds) {
			segEnd = bounds[k+1]
		}
		var window []subtitle.Cue
		for _, cue := range st.cues {
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
	return writeMediaPlaylist(fs, filepath.Join(dir, fmt.Sprintf("sub%d.m3u8", idx+1)), durs, "",
		func(i int) string { return fmt.Sprintf("sub%d_%05d.vtt", idx+1, i+1) })
}

// writeMasterPlaylist writes the multivariant playlist: the muxed audio+video
// rendition (BANDWIDTH from the peak segment bitrate, RESOLUTION/FRAME-RATE,
// CODECS when every track's RFC 6381 string is known) and the subtitle
// renditions as an EXT-X-MEDIA group.
func writeMasterPlaylist(fs *mkv.FS, dir string, fts []*fragTrack, subs []hlsSubTrack, segs []segInfo) error {
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n"...)

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
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=\"sub%d.m3u8\"\n", attrs, i+1)...)
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
	if len(subs) > 0 {
		inf += ",SUBTITLES=\"subs\""
	}
	b = append(b, (inf + "\nplaylist.m3u8\n")...)
	return fs.DoWriteFile(filepath.Join(dir, "master.m3u8"), b, 0o644)
}
