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
// write). Title, global tags and cover art ride in the init segment as usual.
// Text subtitle tracks are declared in the master playlist and served as a
// WebVTT rendition each — whole-presentation (subN.vtt) or windowed
// (subN_%05d.vtt). Text blocks have no cue index, so the cues come from a
// sequential pass over the source; the pass is incremental (subCursor): each
// windowed request advances it just far enough, in playback order that is one
// bounded prefix read per segment, and the whole pass is paid at most once.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
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
	inits    [][]byte // one init segment per track (video first, per fts order)
	medias   [][]byte // one media playlist per track
	master   []byte
	mpd      []byte
	segCount int
	opts     Options       // Encrypt / RewriteURL ride along for Resource builds
	subs     []hlsSubTrack // declared renditions; cues fetched lazily, then cached
	subMu    []sync.Mutex  // serialises each track's cue cursor
	subCur   []subCursor   // per-track incremental cue scan state
	mp4src   bool          // MP4 source: windows sliced from the (fully timed) sample arrays
	mp4offs  [][]int64     // per track, per sample: absolute file offset (MP4 source)
	iframe   []byte        // trick-play playlist (MP4 plans; exact byte ranges)
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

	// Source sniff: an MP4's moov IS the index (sample offsets/sizes/sync,
	// head-only), so its plan is exact by construction; a Matroska source
	// plans through its Cues.
	if mp4ps, sniffErr := sniffMP4ForPlan(ctx, srcPath, fs); sniffErr != nil {
		return nil, sniffErr
	} else if mp4ps != nil {
		defer mp4ps.Close()
		return planHLSFromMP4(ctx, mp4ps, srcPath, fs, &o, segMs)
	}

	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs,
		reader.WithCues(), reader.WithTags(), reader.WithAttachments())
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
		if t.isChapter || t.spec.text {
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

	if o.Encrypt != nil {
		if err := o.Encrypt.validate(); err != nil {
			return nil, err
		}
	}
	p := &HLSPlan{srcPath: srcPath, fs: fs, tcScale: c.Info.TimecodeScale, opts: o}
	p.subs = planSubTracks(c, o)
	p.subMu = make([]sync.Mutex, len(p.subs))
	p.subCur = make([]subCursor, len(p.subs))
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
	p.master = buildMasterPlaylist(&o, fts, p.subs, segs, nil)
	if o.Encrypt == nil {
		p.mpd = buildDASHManifest(&o, fts, p.subs, p.durs, peakBandwidth(segs))
	}
	meta := movieMeta{title: c.Info.Title, tags: globalTags(c), cover: pickCoverArt(c.Attachments)}
	for i, ft := range fts {
		m := movieMeta{}
		if ft.outTrack.spec.video {
			m = meta
		}
		p.inits = append(p.inits, buildInitSegment([]*fragTrack{ft}, m))
		i := i
		p.medias = append(p.medias, buildMediaPlaylist(&o, p.durs, renditionInit(fts, i),
			func(k int) string { return renditionSegment(fts, i, k) }))
	}
	return p, nil
}

// fts returns the plan's fragTracks in order (video first).
func (p *HLSPlan) fts() []*fragTrack {
	out := make([]*fragTrack, len(p.tracks))
	for i, pt := range p.tracks {
		out[i] = pt.ft
	}
	return out
}

// videoIndex returns the index of the video track in p.tracks.
func (p *HLSPlan) videoIndex() int {
	for i, pt := range p.tracks {
		if pt.ft.outTrack.spec.video {
			return i
		}
	}
	return 0
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
// cluster sizes), including the declared subtitle renditions.
func (p *HLSPlan) MasterPlaylist() []byte { return p.master }

// MediaPlaylist returns playlist.m3u8 — the video rendition's playlist
// (audio renditions have their own, audioK.m3u8, served via Resource).
func (p *HLSPlan) MediaPlaylist() []byte { return p.medias[p.videoIndex()] }

// InitSegment returns init.mp4 — the video rendition's init segment, which
// also carries the movie metadata (title, tags, cover art).
func (p *HLSPlan) InitSegment() []byte { return p.inits[p.videoIndex()] }

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

// Segment builds the n-th (0-based) VIDEO media segment (segNNNNN.m4s) —
// audio renditions' segments are served through Resource by name. The bytes
// equal the corresponding segment RemuxToHLS writes.
func (p *HLSPlan) Segment(ctx context.Context, n int) ([]byte, error) {
	return p.segmentTrack(ctx, p.videoIndex(), n)
}

// segmentTrack builds the n-th segment of the ti-th track — styp + moof +
// mdat — reading only that segment's window from the source: it seeks to the
// window's cluster through the Cues and stops once the track crosses the
// window's end.
func (p *HLSPlan) segmentTrack(ctx context.Context, ti, n int) ([]byte, error) {
	if n < 0 || n >= p.segCount {
		return nil, errf("segment %d out of range (0..%d)", n, p.segCount-1)
	}
	if p.mp4src {
		return p.mp4SegmentTrack(ctx, ti, n)
	}
	pt := p.tracks[ti]
	segStart := p.bounds[n]
	var segEnd int64 = 1<<63 - 1
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}

	// Cursor semantics, exactly like the full pass: the track's window starts
	// at its first sample (decode order) with PTS >= segStart and ends just
	// before its first sample with PTS >= segEnd. Everything in between
	// belongs to the window whatever its PTS — open-GOP leading B-frames ride
	// with the keyframe that follows them in presentation but precedes them
	// in decode.
	var window []segSample
	started := n == 0
	nextPts := int64(-1)
	err := p.walkBlocks(ctx, p.offsets[n], func(b mkv.Block, wpt *planTrack) (bool, error) {
		if wpt != pt {
			return true, nil
		}
		if !started {
			if b.Timecode < segStart {
				return true, nil // interleaved leftovers of the previous window
			}
			started = true
		}
		if b.Timecode >= segEnd {
			nextPts = b.Timecode
			return false, nil
		}
		data := pt.ft.outTrack.mkv.RestoreHeader(b.Data)
		window = append(window, segSample{
			fragSample: fragSample{size: uint32(len(data)), ptsMs: b.Timecode, sync: b.Keyframe},
			data:       data,
		})
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	// Fragment timing — fillFragTiming's DTS derivation applied to the window,
	// anchored on the track's global first PTS, with the boundary lookahead
	// supplying the window's final sample duration. An empty window (the track
	// ended before the presentation) becomes an empty trun with the decode
	// time parked at the stream end, keeping the rendition's segments aligned.
	seg := trackSegment{trackID: pt.ft.outTrack.mp4ID, baseDecodeTS: pt.ft.durMediaTS}
	if len(window) > 0 {
		scale := tsScale(pt.ft.timescale)
		base := scale(pt.firstPtsMs)
		dts := make([]int64, len(window))
		for x := range window {
			dts[x] = scale(window[x].ptsMs)
		}
		sort.Slice(dts, func(a, b int) bool { return dts[a] < dts[b] })
		for x := range window {
			window[x].dtsTS = dts[x] - base
			window[x].ctsTS = int32(scale(window[x].ptsMs) - dts[x])
		}
		for x := 0; x < len(window)-1; x++ {
			window[x].durTS = dts[x+1] - dts[x]
		}
		if nextPts >= 0 {
			window[len(window)-1].durTS = scale(nextPts) - dts[len(window)-1]
		} else { // the track's final sample (fillFragTiming's rule)
			window[len(window)-1].durTS = pt.lastDurTS
		}

		samples := make([]fragSample, len(window))
		for x := range window {
			samples[x] = window[x].fragSample
			seg.dataLen += int64(window[x].size)
		}
		seg.baseDecodeTS = dts[0] - base
		seg.samples = samples
		seg.hasCTS = windowHasCTS(samples)
	}

	head := buildSegmentFile(uint32(n+1), seg)
	out := make([]byte, 0, int64(len(head))+seg.dataLen)
	out = append(out, head...)
	for x := range window {
		out = append(out, window[x].data...)
	}
	if p.opts.Encrypt != nil {
		return p.opts.Encrypt.encryptSegment(out, uint32(n))
	}
	return out, nil
}

// Resources returns every resource name the plan serves — the HLS master, the
// DASH manifest (same CMAF segments, second manifest), then per rendition its
// media playlist, init segment and media segments (video first, then each
// audio track), and each subtitle rendition (its HLS playlist and its WebVTT).
// Serving a presentation is: answer any of these names through Resource.
func (p *HLSPlan) Resources() []string {
	names := []string{"master.m3u8"}
	if p.iframe != nil {
		names = append(names, "iframe.m3u8")
	}
	if p.mpd != nil {
		names = append(names, "manifest.mpd")
	}
	fts := p.fts()
	for i := range fts {
		names = append(names, renditionPlaylist(fts, i), renditionInit(fts, i))
		for n := 0; n < p.segCount; n++ {
			names = append(names, renditionSegment(fts, i, n))
		}
	}
	for i := range p.subs {
		names = append(names, fmt.Sprintf("sub%d.m3u8", i+1), fmt.Sprintf("sub%d.vtt", i+1))
		for n := 0; n < p.segCount; n++ {
			names = append(names, fmt.Sprintf("sub%d_%05d.vtt", i+1, n+1))
		}
	}
	return names
}

// Resource builds the named resource and returns its bytes and Content-Type.
// The name is exactly the URI a player requests relative to the playlist —
// "master.m3u8", "playlist.m3u8", "init.mp4", "seg00042.m4s", "sub1.m3u8",
// "sub1.vtt" — so an HTTP handler is a one-liner around this call. Playlists
// and the init segment are precomputed; media segments read their window;
// a subtitle .vtt runs one sequential pass over the source (lazily — cache it
// if requested often).
func (p *HLSPlan) Resource(ctx context.Context, name string) ([]byte, string, error) {
	const (
		mimeM3U8 = "application/vnd.apple.mpegurl"
		mimeMP4  = "video/mp4"
		mimeSeg  = "video/iso.segment"
		mimeVTT  = "text/vtt"
	)
	switch name {
	case "master.m3u8":
		return p.master, mimeM3U8, nil
	case "iframe.m3u8":
		if p.iframe == nil {
			return nil, "", errf("no I-frame playlist for this plan (Matroska plans and encrypted presentations do not expose one)")
		}
		return p.iframe, mimeM3U8, nil
	case "manifest.mpd":
		if p.mpd == nil {
			return nil, "", errf("no DASH manifest for an AES-128-encrypted presentation (HLS only)")
		}
		return p.mpd, "application/dash+xml", nil
	}
	fts := p.fts()
	for i := range fts {
		switch name {
		case renditionPlaylist(fts, i):
			return p.medias[i], mimeM3U8, nil
		case renditionInit(fts, i):
			return p.inits[i], mimeMP4, nil
		}
	}
	var n int
	if _, err := fmt.Sscanf(name, "seg%d.m4s", &n); err == nil && name == fmt.Sprintf("seg%05d.m4s", n) {
		data, err := p.segmentTrack(ctx, p.videoIndex(), n-1)
		return data, mimeSeg, err
	}
	var a int
	if _, err := fmt.Sscanf(name, "seg_a%d_%d.m4s", &a, &n); err == nil && a >= 1 {
		for i := range fts {
			if name == renditionSegment(fts, i, n-1) {
				data, err := p.segmentTrack(ctx, i, n-1)
				return data, mimeSeg, err
			}
		}
	}
	var i int
	if _, err := fmt.Sscanf(name, "sub%d.m3u8", &i); err == nil && name == fmt.Sprintf("sub%d.m3u8", i) {
		if i < 1 || i > len(p.subs) {
			return nil, "", errf("no subtitle rendition %d (have %d)", i, len(p.subs))
		}
		// Windowed like the full pass (byte-identical playlist); the cues are
		// loaded once, so each window is served from the cache.
		pl := buildMediaPlaylist(&p.opts, p.durs, "",
			func(k int) string { return fmt.Sprintf("sub%d_%05d.vtt", i, k+1) })
		return pl, mimeM3U8, nil
	}
	if _, err := fmt.Sscanf(name, "sub%d_%d.vtt", &i, &n); err == nil && name == fmt.Sprintf("sub%d_%05d.vtt", i, n) {
		data, err := p.subtitleSegment(ctx, i-1, n-1)
		return data, mimeVTT, err
	}
	if _, err := fmt.Sscanf(name, "sub%d.vtt", &i); err == nil && name == fmt.Sprintf("sub%d.vtt", i) {
		data, err := p.Subtitle(ctx, i-1)
		return data, mimeVTT, err
	}
	return nil, "", errf("unknown HLS resource %q (see Resources())", name)
}

// subCursor is one subtitle track's incremental cue scan: the pass over the
// clusters advances only as far as each request needs and resumes where it
// stopped — serving windowed segments in playback order costs one bounded
// prefix read each, and the whole pass is paid at most once in total. Text
// blocks carry no cue index, which is why a pass exists at all.
type subCursor struct {
	cues    []subtitle.Cue // raw cues in block order (ends resolved per request)
	next    int64          // cluster offset to resume from; 0 = not started
	scanned int64          // clusters with timestamps up to this ms are consumed
	done    bool           // scanned to EOF
	err     error          // permanent (non-context) scan error, replayed
}

// subScanStrideMs is one scan increment: progress commits and the caller's
// ctx is honoured at each stride, so a client disconnect keeps the clusters
// already walked and a later request resumes instead of restarting.
const subScanStrideMs = 60_000

// subCuesThrough returns the i-th track's cues, complete and end-resolved for
// every cue starting before uptoMs (math.MaxInt64 = the whole track),
// advancing the cursor as far as that requires and no further.
func (p *HLSPlan) subCuesThrough(ctx context.Context, i int, uptoMs int64) ([]subtitle.Cue, error) {
	if p.mp4src {
		return p.subs[i].cues, nil // decoded at plan time from the sample table
	}
	p.subMu[i].Lock()
	err := p.extendSubScan(ctx, i, uptoMs)
	var cues []subtitle.Cue
	if err == nil {
		cues = append([]subtitle.Cue(nil), p.subCur[i].cues...)
	}
	p.subMu[i].Unlock()
	if err != nil {
		return nil, err
	}
	// Resolving on the prefix equals resolving on the full track for every
	// cue starting before uptoMs: extendSubScan guarantees their successors
	// (which duration-less ends resolve on) are in the prefix.
	subtitle.ResolveCueEnds(cues, 2000)
	return cues, nil
}

// extendSubScan advances the i-th cursor until it holds every cue starting
// before uptoMs with resolvable ends. Two bounds drive the walk: blocks with
// timecode T live in clusters stamped within the relative-timecode range of T
// (±32767 raw ticks), and a duration-less cue's end is the NEXT cue's start —
// so the scan runs to uptoMs plus that margin, then keeps going while the
// last known cue before uptoMs still awaits a successor. Cancellation between
// strides keeps the committed progress; only permanent errors are cached.
func (p *HLSPlan) extendSubScan(ctx context.Context, i int, uptoMs int64) error {
	cur := &p.subCur[i]
	if cur.err != nil {
		return cur.err
	}
	relMaxMs := int64(32767)
	if p.tcScale > 0 && p.tcScale < math.MaxInt64/32767 {
		relMaxMs = 32767 * p.tcScale / 1_000_000
	}
	target := uptoMs
	if target < math.MaxInt64-relMaxMs {
		target += relMaxMs
	}
	if cur.done || (cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs)) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	st := &p.subs[i]
	src, err := p.fs.DoOpen(p.srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	start := cur.next
	if start == 0 {
		start = p.offsets[0]
	}
	br, err := reader.NewBlockReaderAt(src, p.tcScale, start)
	if err != nil {
		cur.err = err
		return err
	}
	br.KeepTracks(st.track.ID)
	var local []subtitle.Cue
	for {
		stopAt := cur.scanned + subScanStrideMs
		if stopAt < cur.scanned {
			stopAt = math.MaxInt64
		}
		br.StopBeforeClusterMs(stopAt)
		for {
			b, err := br.Next()
			if errors.Is(err, reader.ErrClusterLimit) {
				cur.cues = append(cur.cues, local...)
				local = local[:0]
				cur.next = br.ResumeOffset()
				cur.scanned = stopAt
				break
			}
			if errors.Is(err, io.EOF) {
				cur.cues = append(cur.cues, local...)
				cur.done = true
				return nil
			}
			if err != nil {
				cur.err = errf("read block: %w", err)
				return cur.err
			}
			if b.TrackNumber != st.track.ID {
				continue
			}
			if cue, ok := subCueFromBlock(st.track.Codec, b); ok {
				local = append(local, cue)
			}
		}
		if cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// subNeedsNextCue reports whether serving cues starting before uptoMs still
// needs a later cue: a duration-less cue resolves its end on the NEXT cue's
// start, however far away, so the scan must reach that successor (or EOF).
// Only the trailing run of latest-start cues can lack one.
func subNeedsNextCue(cues []subtitle.Cue, uptoMs int64) bool {
	if len(cues) == 0 {
		return false
	}
	last := cues[len(cues)-1].StartMs
	for i := len(cues) - 1; i >= 0 && cues[i].StartMs == last; i-- {
		if cues[i].StartMs < uptoMs && cues[i].EndMs <= cues[i].StartMs {
			return true
		}
	}
	return false
}

// Subtitle builds the i-th (0-based) subtitle rendition's WebVTT — the whole
// track as one file. It drives the cue cursor to the end of the source; the
// cues collected stay cached for the windowed segments (and vice versa).
func (p *HLSPlan) Subtitle(ctx context.Context, i int) ([]byte, error) {
	if i < 0 || i >= len(p.subs) {
		return nil, errf("subtitle rendition %d out of range (0..%d)", i, len(p.subs)-1)
	}
	cues, err := p.subCuesThrough(ctx, i, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, cues); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// subtitleSegment builds the i-th rendition's n-th windowed WebVTT segment
// (subN_%05d.vtt) — the same windows the full pass writes, byte-identical.
// Only the prefix of the source the window can draw cues from is scanned, so
// serving segments in playback order reads the source once, incrementally —
// the first hit costs one bounded window, like a video segment.
func (p *HLSPlan) subtitleSegment(ctx context.Context, i, n int) ([]byte, error) {
	if i < 0 || i >= len(p.subs) {
		return nil, errf("subtitle rendition %d out of range (0..%d)", i, len(p.subs)-1)
	}
	if n < 0 || n >= p.segCount {
		return nil, errf("subtitle segment %d out of range (0..%d)", n, p.segCount-1)
	}
	segStart := p.bounds[n]
	var segEnd int64 = math.MaxInt64
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}
	cues, err := p.subCuesThrough(ctx, i, segEnd)
	if err != nil {
		return nil, err
	}
	var window []subtitle.Cue
	for _, cue := range cues {
		if cue.EndMs > segStart && cue.StartMs < segEnd {
			window = append(window, cue)
		}
	}
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, window); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// sniffMP4ForPlan opens srcPath as an MP4 packaging source when it is one,
// nil when it is Matroska (the caller then takes the Cues path).
func sniffMP4ForPlan(ctx context.Context, srcPath string, fs *mkv.FS) (*packagingSource, error) {
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		f.Close()
		return nil, errf("read head: %w", err)
	}
	if head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		f.Close()
		return nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	mv, err := parseMP4(f, st.Size(), sampleFull)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &packagingSource{c: containerFromMovie(mv), mv: mv, src: f, size: st.Size()}, nil
}

// planHLSFromMP4 builds the on-demand plan from an MP4 source's sample table:
// every window, size and duration is known head-only, so the plan's outputs —
// including the master's BANDWIDTH — equal the full pass exactly.
func planHLSFromMP4(ctx context.Context, ps *packagingSource, srcPath string, fs *mkv.FS, o *Options, segMs int64) (*HLSPlan, error) {
	c := ps.c
	planned, _, err := planTracks(c, *o)
	if err != nil {
		return nil, err
	}
	var media []*outTrack
	var videoSeen bool
	for _, t := range planned {
		if t.isChapter || t.spec.text {
			continue
		}
		if t.spec.video {
			if videoSeen {
				o.report(DroppedTrack{ID: t.mkv.ID, Type: t.mkv.Type, Codec: t.mkv.Codec,
					Reason: "the presentation carries one video track; secondary video tracks are dropped"})
				continue
			}
			videoSeen = true
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return nil, errf("no audio or video track to segment")
	}

	p := &HLSPlan{srcPath: srcPath, fs: fs, opts: *o, mp4src: true}
	p.subs = planSubTracks(c, *o)
	p.subMu = make([]sync.Mutex, len(p.subs))
	p.subCur = make([]subCursor, len(p.subs))
	if err := mp4SubCues(ps, p.subs); err != nil {
		return nil, err
	}

	fts, offs, err := mp4PlanSamples(ps, media)
	if err != nil {
		return nil, err
	}
	p.mp4offs = offs
	for _, ft := range fts {
		p.tracks = append(p.tracks, &planTrack{ft: ft, firstPtsMs: ft.offsetMs})
	}

	p.bounds = segmentBoundaries(fts[primaryIndex(fts)].samples, segMs)
	p.segCount = len(p.bounds)

	// Exact per-boundary aggregates: window sizes are computable without
	// touching the media (deterministic builders), so BANDWIDTH — and with it
	// the master playlist and the DASH manifest — equal the full pass.
	video := pickVideoFrag(fts)
	cursors := make([]int, len(fts))
	segs := make([]segInfo, 0, p.segCount)
	var iframes []iframeRef
	p.durs = make([]float64, p.segCount)
	for k := 0; k < p.segCount; k++ {
		segStart := p.bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < p.segCount {
			segEnd = p.bounds[k+1]
		}
		var segBytes int64
		for i, ft := range fts {
			seg := segmentWindow(ft, &cursors[i], segEnd)
			head := buildSegmentFile(uint32(k+1), seg)
			if ft == video && len(seg.samples) > 0 && seg.samples[0].sync {
				iframes = append(iframes, iframeRef{seg: k, length: int64(len(head)) + int64(seg.samples[0].size)})
			}
			segBytes += int64(len(head)) + seg.dataLen
		}
		p.durs[k] = float64(segEndOrLast(p.bounds, k, fts)-segStart) / 1000
		segs = append(segs, segInfo{durSec: p.durs[k], bytes: segBytes})
	}
	if video != nil && o.Encrypt == nil && len(iframes) > 0 {
		p.iframe = buildIFramePlaylist(o, fts, p.durs, iframes)
	}

	p.master = buildMasterPlaylist(o, fts, p.subs, segs, iframes)
	if o.Encrypt == nil {
		p.mpd = buildDASHManifest(o, fts, p.subs, p.durs, peakBandwidth(segs))
	}
	meta := movieMeta{title: c.Info.Title, tags: globalTags(c)}
	for i, ft := range fts {
		m := movieMeta{}
		if i == primaryIndex(fts) {
			m = meta
		}
		p.inits = append(p.inits, buildInitSegment([]*fragTrack{ft}, m))
		i := i
		p.medias = append(p.medias, buildMediaPlaylist(o, p.durs, renditionInit(fts, i),
			func(k int) string { return renditionSegment(fts, i, k) }))
	}
	return p, nil
}

// mp4SegmentTrack builds one rendition segment from the sample-table plan:
// the window is a slice of the fully timed sample array (identical values to
// the full pass) and the bytes are ranged reads at the recorded offsets.
func (p *HLSPlan) mp4SegmentTrack(ctx context.Context, ti, n int) ([]byte, error) {
	fts := p.fts()
	ft := fts[ti]
	// Window bounds under the full pass's cursor semantics: each window
	// starts where the previous one stopped — at the first sample (decode
	// order) at/past its boundary.
	start := 0
	for k := 1; k <= n; k++ {
		for start < len(ft.samples) && ft.samples[start].ptsMs < p.bounds[k] {
			start++
		}
	}
	end := start
	if n+1 < p.segCount {
		for end < len(ft.samples) && ft.samples[end].ptsMs < p.bounds[n+1] {
			end++
		}
	} else {
		end = len(ft.samples)
	}

	seg := trackSegment{trackID: ft.outTrack.mp4ID, baseDecodeTS: ft.durMediaTS}
	if end > start {
		seg.samples = ft.samples[start:end]
		seg.baseDecodeTS = ft.samples[start].dtsTS
		seg.hasCTS = windowHasCTS(seg.samples)
		for x := start; x < end; x++ {
			seg.dataLen += int64(ft.samples[x].size)
		}
	}
	head := buildSegmentFile(uint32(n+1), seg)
	out := make([]byte, 0, int64(len(head))+seg.dataLen)
	out = append(out, head...)

	if end > start {
		src, err := p.fs.DoOpen(p.srcPath)
		if err != nil {
			return nil, err
		}
		defer src.Close()
		for x := start; x < end; x++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			data, err := readSample(src, p.mp4offs[ti][x], ft.samples[x].size)
			if err != nil {
				return nil, errf("read sample: %w", err)
			}
			out = append(out, data...)
		}
	}
	if p.opts.Encrypt != nil {
		return p.opts.Encrypt.encryptSegment(out, uint32(n))
	}
	return out, nil
}
