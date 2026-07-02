package mp4

// hls.go — RemuxToHLS: a fragmented-MP4 / CMAF HLS presentation (init.mp4,
// segNNNNN.m4s, playlist.m3u8) produced from a Matroska/WebM file without
// transcoding. This is the "copy rung" of an HLS ladder: mkvgo does the CMAF
// packaging; bitrate variants (real ABR) remain a transcoder's job.
//
// Memory is bounded: per-sample metadata (size/pts/sync — the same order of
// magnitude the progressive muxer already holds) is kept in RAM, while the
// sample *bytes* are streamed to one temp file per track and read back
// sequentially when the segments are written. Nothing holds the whole media.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
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

// RemuxToHLS reads the Matroska/WebM file at srcPath and writes a fragmented-MP4
// HLS presentation into outputDir: an initialisation segment (init.mp4), the
// media segments (seg00001.m4s …) and a VOD playlist (playlist.m3u8). It never
// transcodes — samples are copied verbatim into CMAF fragments — so only the
// codecs RemuxToMP4 supports are carried. Segments are cut on video keyframes at
// roughly Options.SegmentMs (default 6 s).
//
// Only audio and video tracks are segmented; subtitle tracks are dropped
// (reported via Options.OnDrop) — HLS carries subtitles as separate WebVTT
// playlists, which this entry point does not yet emit.
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
	// Keep only media tracks; a fragmented HLS rendition carries audio+video.
	var media []*outTrack
	for _, t := range planned {
		if t.isChapter || t.spec.text {
			if !t.isChapter {
				o.report(DroppedTrack{ID: t.mkv.ID, Type: t.mkv.Type, Codec: t.mkv.Codec,
					Reason: "subtitles are not carried into HLS fragments (separate WebVTT playlist not yet emitted)"})
			}
			continue
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return errf("no audio or video track to segment")
	}

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

	// Phase 1 — stream every block, buffering sample bytes to the per-track temp
	// files and recording sample metadata.
	if err := collectFragSamples(ctx, srcPath, fs, c, routing); err != nil {
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

	segDurs, err := writeSegments(ctx, fs, outputDir, fts, bounds)
	if err != nil {
		return err
	}
	return writePlaylist(fs, outputDir, segMs, segDurs)
}

// collectFragSamples reads every block once, writing each media sample's bytes to
// its track's temp file and recording (size, pts, sync). Lazily builds the sample
// entry for codecs that derive it from the first frame.
func collectFragSamples(ctx context.Context, srcPath string, fs *mkv.FS, c *mkv.Container, routing map[uint64]*fragTrack) error {
	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
	if err != nil {
		return err
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
// duration in seconds (from the video track's sample durations) for the playlist.
func writeSegments(ctx context.Context, fs *mkv.FS, dir string, fts []*fragTrack, bounds []int64) ([]float64, error) {
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
	var durs []float64
	for k := 0; k+1 <= len(bounds); k++ {
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

		// EXTINF duration: the video track's covered span, in seconds.
		durs = append(durs, float64(segEndOrLast(bounds, k, fts)-segStart)/1000)
	}
	return durs, nil
}

// writeOneSegment writes styp + moof + mdat(sample bytes) for one segment. The
// mdat sample bytes are read sequentially from each track's temp file in the same
// order the trafs appear, matching the data_offsets buildMoof patched in.
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

// writePlaylist writes a VOD HLS media playlist referencing the init segment
// (EXT-X-MAP) and every media segment (EXTINF).
func writePlaylist(fs *mkv.FS, dir string, targetMs int64, durs []float64) error {
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
	b = append(b, "#EXT-X-MAP:URI=\"init.mp4\"\n"...)
	for i, d := range durs {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\nseg%05d.m4s\n", d, i+1)...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return fs.DoWriteFile(filepath.Join(dir, "playlist.m3u8"), b, 0o644)
}
