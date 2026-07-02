package mp4

// hlsplan.go — on-demand HLS: PlanHLS derives everything a server needs to
// answer playlist/init requests from three bounded reads (the metadata head
// with its Cues, the first cluster, the last cued cluster), and Segment(n)
// then builds any single media segment by reading only that segment's window —
// seeking straight to its cluster through the Cues. Nothing is pre-generated:
// first-play latency is one window read, storage cost is zero, and a remote
// source (httpfs) transfers only the ranges a viewer actually watches.
//
// The fragments are built by the same code as RemuxToHLS, with the same DTS
// assignment, so Segment(n) is byte-identical to the n-th segment of the full
// pass (given a source whose Cues index every video keyframe, as real muxers
// write). Subtitle tracks are not carried in this mode (a text track has no
// cue index to window it by) and neither is cover art (attachments are not
// read); the title and global tags ride in the init segment as usual.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// HLSPlan is the result of PlanHLS: the playlists, the init segment, and the
// per-segment window index. It is immutable after PlanHLS returns and safe for
// concurrent Segment calls (each opens its own reader).
type HLSPlan struct {
	srcPath  string
	fs       *mkv.FS
	tcScale  int64
	tracks   []*planTrack
	bounds   []int64 // segment start times (ms); bounds[0] == 0
	offsets  []int64 // absolute file offset of the cluster holding bounds[k]
	durs     []float64
	initSeg  []byte
	media    []byte
	master   []byte
	segCount int
}

// planTrack is one media track's plan state: the outTrack (sample entry ready)
// plus the timing anchors the per-segment DTS derivation needs.
type planTrack struct {
	ft         *fragTrack
	firstPtsMs int64 // the track's first sample PTS — the global DTS origin
	lastDurTS  int64 // the track's final sample duration (fillFragTiming's rule)
}

// PlanHLS reads the source's metadata, Cues, first and last clusters — a few
// bounded reads, head-only in spirit — and returns a plan that serves an HLS
// presentation segment by segment. The source must carry a Cues index (every
// real muxer writes one; `mkvgo reindex` adds one). See HLSPlan.Segment.
func PlanHLS(ctx context.Context, srcPath string, opts ...Options) (*HLSPlan, error) {
	o := optionsFrom(opts)
	fs := o.FS
	segMs := o.SegmentMs
	if segMs <= 0 {
		segMs = defaultSegmentMs
	}

	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs, reader.WithCues(), reader.WithTags())
	if err != nil {
		return nil, err
	}
	if len(c.Cues) == 0 {
		return nil, errf("%s: no Cues index — on-demand HLS seeks through the Cues (run `mkvgo reindex` first)", srcPath)
	}
	planned, _, err := planTracks(c, o)
	if err != nil {
		return nil, err
	}
	var media []*outTrack
	for _, t := range planned {
		if t.isChapter {
			continue
		}
		if t.spec.text {
			o.report(DroppedTrack{ID: t.mkv.ID, Type: t.mkv.Type, Codec: t.mkv.Codec,
				Reason: "on-demand HLS does not carry subtitle tracks (use RemuxToHLS for WebVTT renditions)"})
			continue
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return nil, errf("no audio or video track to segment")
	}
	var hasVideo bool
	for _, t := range media {
		hasVideo = hasVideo || t.spec.video
	}
	if !hasVideo {
		return nil, errf("HLS output requires a video track")
	}

	p := &HLSPlan{srcPath: srcPath, fs: fs, tcScale: c.Info.TimecodeScale}
	for _, t := range media {
		p.tracks = append(p.tracks, &planTrack{
			ft:         &fragTrack{outTrack: t, timescale: mediaTimescale(t)},
			firstPtsMs: -1,
		})
	}

	// Segment boundaries from the cue times (the video keyframes), like
	// segmentBoundaries does from the sample flags.
	p.bounds = []int64{0}
	p.offsets = []int64{c.SegmentStart + c.Cues[0].ClusterPos}
	last := int64(0)
	for _, cue := range c.Cues {
		if cue.TimeMs >= last+segMs {
			p.bounds = append(p.bounds, cue.TimeMs)
			p.offsets = append(p.offsets, c.SegmentStart+cue.ClusterPos)
			last = cue.TimeMs
		}
	}
	p.segCount = len(p.bounds)

	// Bounded peeks: the first cluster fixes each track's first PTS and builds
	// the sample entries lazy codecs derive from their first frame; the last
	// cued cluster fixes each track's final PTS pair (exact init durations).
	if err := p.peekHead(ctx); err != nil {
		return nil, err
	}
	lastPts, prevPts, err := p.peekTail(ctx, c.SegmentStart+c.Cues[len(c.Cues)-1].ClusterPos)
	if err != nil {
		return nil, err
	}

	// Per-track durations, exactly as fillFragTiming derives them.
	var video *planTrack
	for i, pt := range p.tracks {
		ft := pt.ft
		if pt.firstPtsMs < 0 {
			return nil, errf("track %d produced no samples", ft.outTrack.mp4ID)
		}
		scale := tsScale(ft.timescale)
		switch {
		case ft.outTrack.frameDurMs > 0:
			pt.lastDurTS = scale(ft.outTrack.frameDurMs)
		case prevPts[i] >= 0 && lastPts[i] > prevPts[i]:
			pt.lastDurTS = scale(lastPts[i]) - scale(prevPts[i])
		default:
			pt.lastDurTS = 1
		}
		ft.offsetMs = pt.firstPtsMs
		ft.durMediaTS = scale(lastPts[i]) - scale(pt.firstPtsMs) + pt.lastDurTS
		ft.durMovieMs = ft.durMediaTS
		if ft.timescale != movieTimescale {
			ft.durMovieMs = ft.durMediaTS * int64(movieTimescale) / int64(ft.timescale)
		}
		ft.presentMs = ft.offsetMs + ft.durMovieMs
		if video == nil && ft.outTrack.spec.video {
			video = pt
		}
	}

	// Playlists. Segment durations come from the boundaries; the last segment
	// ends at the video's final PTS + duration (segEndOrLast's rule).
	vi := 0
	for i, pt := range p.tracks {
		if pt == video {
			vi = i
		}
	}
	endMs := lastPts[vi] + video.lastDurTS*int64(movieTimescale)/int64(video.ft.timescale)
	p.durs = make([]float64, p.segCount)
	for k := 0; k < p.segCount; k++ {
		end := endMs
		if k+1 < p.segCount {
			end = p.bounds[k+1]
		}
		p.durs[k] = float64(end-p.bounds[k]) / 1000
	}
	p.media = buildMediaPlaylist(p.durs, "init.mp4",
		func(i int) string { return fmt.Sprintf("seg%05d.m4s", i+1) })

	// Master playlist with BANDWIDTH estimated from the cue cluster offsets —
	// the source bytes between two boundaries approximate the fragment size.
	var segs []segInfo
	srcSize := int64(0)
	if st, serr := fs.DoStat(srcPath); serr == nil {
		srcSize = st.Size()
	}
	for k := 0; k < p.segCount; k++ {
		end := srcSize
		if k+1 < p.segCount {
			end = p.offsets[k+1]
		}
		segs = append(segs, segInfo{durSec: p.durs[k], bytes: end - p.offsets[k]})
	}
	fts := make([]*fragTrack, len(p.tracks))
	for i, pt := range p.tracks {
		fts[i] = pt.ft
	}
	p.master = buildMasterPlaylist(fts, nil, segs)
	p.initSeg = buildInitSegment(fts, movieMeta{title: c.Info.Title, tags: globalTags(c)})
	return p, nil
}

// tsScale returns the ms→timescale converter fillFragTiming uses.
func tsScale(mts uint32) func(int64) int64 {
	return func(ms int64) int64 {
		if mts == movieTimescale {
			return ms
		}
		return ms * int64(mts) / int64(movieTimescale)
	}
}

// NumSegments returns the number of media segments the plan serves.
func (p *HLSPlan) NumSegments() int { return p.segCount }

// SegmentName returns the n-th (0-based) segment's file name in the playlist.
func (p *HLSPlan) SegmentName(n int) string { return fmt.Sprintf("seg%05d.m4s", n+1) }

// MasterPlaylist returns master.m3u8 (BANDWIDTH estimated from the source's
// cluster sizes; no subtitle renditions in on-demand mode).
func (p *HLSPlan) MasterPlaylist() []byte { return p.master }

// MediaPlaylist returns playlist.m3u8.
func (p *HLSPlan) MediaPlaylist() []byte { return p.media }

// InitSegment returns init.mp4.
func (p *HLSPlan) InitSegment() []byte { return p.initSeg }

// peekHead reads from the first cued cluster until every track has its first
// sample: the first PTS anchors the DTS timeline, and the sample entry is
// built for codecs that derive it from the first frame.
func (p *HLSPlan) peekHead(ctx context.Context) error {
	need := len(p.tracks)
	return p.walkBlocks(ctx, p.offsets[0], func(b mkv.Block, pt *planTrack) (bool, error) {
		if pt.firstPtsMs >= 0 {
			return need > 0, nil
		}
		data := pt.ft.outTrack.mkv.RestoreHeader(b.Data)
		if pt.ft.outTrack.sampleEntry == nil {
			entry, err := pt.ft.outTrack.spec.sampleEntry(&pt.ft.outTrack.mkv, data)
			if err != nil {
				return false, err
			}
			pt.ft.outTrack.sampleEntry = entry
		}
		pt.firstPtsMs = b.Timecode
		need--
		return need > 0, nil
	})
}

// peekTail reads from the last cued cluster to EOF and returns each track's
// two largest PTS (the final sample and its predecessor), -1 when unseen.
func (p *HLSPlan) peekTail(ctx context.Context, off int64) (lastPts, prevPts []int64, err error) {
	lastPts = make([]int64, len(p.tracks))
	prevPts = make([]int64, len(p.tracks))
	for i := range lastPts {
		lastPts[i], prevPts[i] = -1, -1
	}
	idx := map[*planTrack]int{}
	for i, pt := range p.tracks {
		idx[pt] = i
	}
	err = p.walkBlocks(ctx, off, func(b mkv.Block, pt *planTrack) (bool, error) {
		i := idx[pt]
		switch {
		case b.Timecode > lastPts[i]:
			prevPts[i], lastPts[i] = lastPts[i], b.Timecode
		case b.Timecode > prevPts[i]:
			prevPts[i] = b.Timecode
		}
		return true, nil
	})
	return lastPts, prevPts, err
}

// walkBlocks runs fn over the media-track blocks from the cluster at off until
// fn returns false or the stream ends. Blocks of non-plan tracks are skipped.
func (p *HLSPlan) walkBlocks(ctx context.Context, off int64, fn func(mkv.Block, *planTrack) (bool, error)) error {
	src, err := p.fs.DoOpen(p.srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	br, err := reader.NewBlockReaderAt(src, p.tcScale, off)
	if err != nil {
		return err
	}
	routing := make(map[uint64]*planTrack, len(p.tracks))
	for _, pt := range p.tracks {
		routing[pt.ft.outTrack.mkv.ID] = pt
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errf("read block: %w", err)
		}
		pt, ok := routing[b.TrackNumber]
		if !ok {
			continue
		}
		more, err := fn(b, pt)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// segSample is one sample of an on-demand window: the fragment metadata plus
// its bytes (a single segment's data is held in memory — a few MB).
type segSample struct {
	fragSample
	data []byte
}

// Segment builds the n-th (0-based) media segment — styp + moof + mdat —
// reading only that segment's window from the source: it seeks to the window's
// cluster through the Cues and stops one sample past the window's end. The
// bytes equal the n-th segment RemuxToHLS writes.
func (p *HLSPlan) Segment(ctx context.Context, n int) ([]byte, error) {
	if n < 0 || n >= p.segCount {
		return nil, errf("segment %d out of range (0..%d)", n, p.segCount-1)
	}
	segStart := p.bounds[n]
	var segEnd int64 = 1<<63 - 1
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}

	// Cursor semantics, exactly like the full pass: a track's window starts at
	// its first sample (decode order) with PTS >= segStart and ends just before
	// its first sample with PTS >= segEnd. Everything in between belongs to the
	// window whatever its PTS — open-GOP leading B-frames ride with the
	// keyframe that follows them in presentation but precedes them in decode.
	windows := make([][]segSample, len(p.tracks))
	started := make([]bool, len(p.tracks))
	nextPts := make([]int64, len(p.tracks)) // first PTS at/past segEnd, -1 until seen
	for i := range nextPts {
		started[i] = n == 0
		nextPts[i] = -1
	}
	idx := map[*planTrack]int{}
	for i, pt := range p.tracks {
		idx[pt] = i
	}
	err := p.walkBlocks(ctx, p.offsets[n], func(b mkv.Block, pt *planTrack) (bool, error) {
		i := idx[pt]
		switch {
		case nextPts[i] >= 0:
			return true, nil // this track's window is closed; drain the others
		case !started[i]:
			if b.Timecode < segStart {
				return true, nil // interleaved leftovers of the previous window
			}
			started[i] = true
			fallthrough
		default:
			if b.Timecode >= segEnd {
				nextPts[i] = b.Timecode
				// Keep reading until every track has crossed the boundary.
				for _, np := range nextPts {
					if np < 0 {
						return true, nil
					}
				}
				return false, nil
			}
			data := pt.ft.outTrack.mkv.RestoreHeader(b.Data)
			windows[i] = append(windows[i], segSample{
				fragSample: fragSample{size: uint32(len(data)), ptsMs: b.Timecode, sync: b.Keyframe},
				data:       data,
			})
			return true, nil
		}
	})
	if err != nil {
		return nil, err
	}

	// Fragment timing per window — fillFragTiming's DTS derivation applied to
	// the window, anchored on the track's global first PTS, with the boundary
	// lookahead supplying the window's final sample duration.
	var segs []trackSegment
	var order []int
	for i, pt := range p.tracks {
		w := windows[i]
		if len(w) == 0 {
			continue
		}
		scale := tsScale(pt.ft.timescale)
		base := scale(pt.firstPtsMs)
		dts := make([]int64, len(w))
		for x := range w {
			dts[x] = scale(w[x].ptsMs)
		}
		sort.Slice(dts, func(a, b int) bool { return dts[a] < dts[b] })
		for x := range w {
			w[x].dtsTS = dts[x] - base
			w[x].ctsTS = int32(scale(w[x].ptsMs) - dts[x])
		}
		for x := 0; x < len(w)-1; x++ {
			w[x].durTS = dts[x+1] - dts[x]
		}
		switch {
		case nextPts[i] >= 0:
			w[len(w)-1].durTS = scale(nextPts[i]) - dts[len(w)-1]
		default: // the track's final sample (fillFragTiming's rule)
			w[len(w)-1].durTS = pt.lastDurTS
		}

		samples := make([]fragSample, len(w))
		var dataLen int64
		for x := range w {
			samples[x] = w[x].fragSample
			dataLen += int64(w[x].size)
		}
		segs = append(segs, trackSegment{
			trackID:      pt.ft.outTrack.mp4ID,
			baseDecodeTS: dts[0] - base,
			samples:      samples,
			hasCTS:       windowHasCTS(samples),
			dataLen:      dataLen,
		})
		order = append(order, i)
	}
	if len(segs) == 0 {
		return nil, errf("segment %d holds no samples", n)
	}

	moof := buildMoof(uint32(n+1), segs)
	out := make([]byte, 0, int64(len(moof))+segsDataLen(segs)+32)
	out = append(out, buildStyp()...)
	out = append(out, moof...)
	var mdatData int64
	for i := range segs {
		mdatData += segs[i].dataLen
	}
	out = append(out, mdatHeader(mdatData)...)
	for _, i := range order {
		for x := range windows[i] {
			out = append(out, windows[i][x].data...)
		}
	}
	return out, nil
}

func segsDataLen(segs []trackSegment) int64 {
	var n int64
	for i := range segs {
		n += segs[i].dataLen
	}
	return n
}
