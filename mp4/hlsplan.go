package mp4

// hlsplan.go - on-demand HLS: PlanHLS derives everything a server needs to
// answer playlist/init requests from three bounded reads (the metadata head
// with its Cues, the first cluster, the last cued cluster), and Segment(n)
// then builds any single media segment by reading only that segment's window  -
// seeking straight to its cluster through the Cues. Nothing is pre-generated:
// first-play latency is one window read, storage cost is zero, and a remote
// source (httpfs) transfers only the ranges a viewer actually watches.
//
// The fragments are built by the same code as RemuxToHLS, with the same DTS
// assignment, so Segment(n) is byte-identical to the n-th segment of the full
// pass (given a source whose Cues index every video keyframe, as real muxers
// write). Title, global tags and cover art ride in the init segment as usual.
// Text subtitle tracks are declared in the master playlist and served as a
// WebVTT rendition each - whole-presentation (subN.vtt) or windowed
// (subN_%05d.vtt). Text blocks have no cue index, so the cues come from
// scanning the clusters; the scans are incremental and bounded (subScanState):
// sequential playback advances an exact prefix cursor stride by stride, and a
// far seek jumps through the segment index to a sliding bounded island  -
// every request costs O(window), like a video segment. Sources whose subtitle
// blocks lack explicit durations (or carry over-long cues) drop to the
// always-exact prefix path.

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
	opts     Options        // Encrypt / RewriteURL ride along for Resource builds
	subs     []hlsSubTrack  // declared renditions; cues fetched lazily, then cached
	subMu    []sync.Mutex   // serialises each track's cue scans
	subScan  []subScanState // per-track incremental cue scan state
	segs     []segInfo      // per-segment duration/bytes (BANDWIDTH; ABR master reuse)
	mp4src   bool           // MP4 source: windows sliced from the (fully timed) sample arrays
	mp4offs  [][]int64      // per track, per sample: absolute file offset (MP4 source)
	iframe   []byte         // trick-play playlist: MP4 plans build it eagerly (free, head-only);
	// Matroska plans build it lazily (see hasIframe/iframeMu/iframeErr) - the
	// exact byte ranges need every segment's sample count/flags, which only a
	// full pass over the video track's block headers can give.
	iframes []iframeRef // the I-frames behind p.iframe, for the ABR master's trick-play stream
	// hasIframe is decided cheaply at construction (a video track, not
	// encrypted): it says whether iframe.m3u8 is a valid resource name for a
	// Matroska plan, independent of whether the lazy pass has run yet.
	hasIframe bool
	iframeMu  sync.Mutex
	// iframeErr caches a PERMANENT build error only (never a context
	// cancellation - see the subtitle-cue scan's poisoning fix): a transient
	// error must not stick around and fail every later request.
	iframeErr error
	// trackDurs feeds every mid-file BlockReader the tracks' DefaultDurations
	// (laced frames share one stored timecode; the stride times them) - a
	// reader seeked to a cluster never sees the Tracks element on its own.
	trackDurs map[uint64]int64
}

// planTrack is one media track's plan state: the outTrack (sample entry ready)
// plus the timing anchors the per-segment DTS derivation needs.
type planTrack struct {
	ft         *fragTrack
	firstPtsMs int64 // the track's first sample PTS - the global DTS origin
	lastDurTS  int64 // the track's final sample duration (fillFragTiming's rule)
	// gridTS is the sample-exact frame stride of a constant-rate audio track
	// (audioGridTS); windows are then timed on the grid, exactly like
	// fillFragTiming's grid branch, instead of on the ms-rounded timecodes.
	gridTS int64
}

// PlanHLS reads the source's metadata, Cues, first and last clusters - a few
// bounded reads, head-only in spirit - and returns a plan that serves an HLS
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

	metaOpts := []reader.ReadOption{reader.WithCues(), reader.WithTags(), reader.WithAttachments()}
	if o.ChapterMarkers {
		// Only fetched when the opt-in is set: an extra bounded SeekHead ->
		// Chapters read a plan otherwise has no use for.
		metaOpts = append(metaOpts, reader.WithChapters())
	}
	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs, metaOpts...)
	if err != nil {
		return nil, err
	}
	if len(c.Cues) == 0 && !o.SynthesizeIndex {
		return nil, errf("%s: no Cues index - on-demand HLS seeks through the Cues (run `mkvgo reindex` first, or plan with Options.SynthesizeIndex)", srcPath)
	}
	planned, _, err := planTracks(c, o)
	if err != nil {
		return nil, err
	}
	// Track selection mirrors remuxToHLSInto exactly so a variant's plan stays
	// byte-identical to its full pass: one video rendition (secondary video
	// dropped), each audio, and - unless VideoOnly (an ABR variant carries only
	// its video) - the subtitle renditions.
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
			continue
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return nil, errf("no audio or video track to segment")
	}
	if !videoSeen {
		return nil, errf("HLS output requires a video track")
	}

	if o.Encrypt != nil {
		if err := o.Encrypt.validate(); err != nil {
			return nil, err
		}
	}
	if err := cencPreflight(&o, videoCodecOf(media)); err != nil {
		return nil, err
	}
	p := &HLSPlan{srcPath: srcPath, fs: fs, tcScale: c.Info.TimecodeScale, opts: o,
		trackDurs: reader.TrackDefaultDurations(c.Tracks), hasIframe: o.Encrypt == nil}
	if !o.VideoOnly {
		p.subs = filterSubTracks(planSubTracks(c, o), keep)
	}
	p.subMu = make([]sync.Mutex, len(p.subs))
	p.subScan = make([]subScanState, len(p.subs))
	for _, t := range media {
		p.tracks = append(p.tracks, &planTrack{
			ft:         &fragTrack{outTrack: t, timescale: mediaTimescale(t)},
			firstPtsMs: -1,
		})
	}

	// Segment boundaries from the cue times (the video keyframes), like
	// segmentBoundaries does from the sample flags. Only the VIDEO track's cue
	// points qualify: real files cue subtitle/audio tracks too, and a boundary
	// on another track's cue time would start a video segment mid-GOP (broken
	// random access) and diverge from the full pass's keyframe cuts.
	var vidID uint64
	for _, t := range media {
		if t.spec.video {
			vidID = t.mkv.ID
			break
		}
	}
	cues := make([]mkv.CuePoint, 0, len(c.Cues))
	for _, cue := range c.Cues {
		if cue.Track == vidID {
			cues = append(cues, cue)
		}
	}
	if len(cues) == 0 {
		// Options.SynthesizeIndex: serve the unindexed (or misskeyed-index)
		// source anyway - walk the clusters once, structure only, and build
		// the video cue points in memory. The synthesized index replaces
		// nothing on disk; the plan just seeks through it.
		if !o.SynthesizeIndex {
			return nil, errf("%s: the Cues index no video keyframes - on-demand HLS seeks through them (run `mkvgo reindex` first, or plan with Options.SynthesizeIndex)", srcPath)
		}
		cues, err = synthesizeVideoCues(ctx, srcPath, fs, c, vidID, p.trackDurs)
		if err != nil {
			return nil, err
		}
		if len(cues) == 0 {
			return nil, errf("%s: no video keyframes found to synthesize an index from", srcPath)
		}
	}
	p.bounds = []int64{0}
	p.offsets = []int64{c.SegmentStart + cues[0].ClusterPos}
	last := int64(0)
	for _, cue := range cues {
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
	lastPts, prevPts, lastFrames, err := p.peekTail(ctx, c.SegmentStart+cues[len(cues)-1].ClusterPos)
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
		if pt.gridTS <= 0 { // peekHead may already have recovered a no-DefaultDuration stride
			pt.gridTS = audioGridTS(ft.outTrack, ft.timescale)
		}
		switch {
		case pt.gridTS > 0:
			// Grid-timed audio: the final frame's index recovers its exact
			// slot from the ms timecode (fillFragTiming's grid rule).
			pt.lastDurTS = pt.gridTS
		case ft.outTrack.frameDurMs > 0:
			pt.lastDurTS = scale(ft.outTrack.frameDurMs)
		case prevPts[i] >= 0 && lastPts[i] > prevPts[i]:
			pt.lastDurTS = scale(lastPts[i]) - scale(prevPts[i])
		default:
			pt.lastDurTS = 1
		}
		ft.offsetMs = pt.firstPtsMs
		if pt.gridTS > 0 {
			kLast := gridIndex(scale(lastPts[i])-scale(pt.firstPtsMs), pt.gridTS)
			// A collapsed no-DefaultDuration lace reports the final block's
			// timecode for every frame it holds, so lastPts lands on the block,
			// not its last frame: advance kLast across the block's frames, as
			// the full pass counts them.
			if ft.outTrack.mkv.DefaultDurationNs <= 0 && lastFrames[i] > 1 {
				kLast += lastFrames[i] - 1
			}
			ft.durMediaTS = (kLast + 1) * pt.gridTS
		} else {
			ft.durMediaTS = scale(lastPts[i]) - scale(pt.firstPtsMs) + pt.lastDurTS
		}
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

	// Master playlist with BANDWIDTH estimated from the cue cluster offsets  -
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
	p.segs = segs
	fts := make([]*fragTrack, len(p.tracks))
	for i, pt := range p.tracks {
		fts[i] = pt.ft
	}
	// Options.AudioPresentationShift: re-base the shifted audio tracks'
	// edit-list offsets before the init segments are built. The fragment
	// decode times were derived above from the UNSHIFTED first PTS, exactly
	// like the full pass - the shift lives in the init alone (parity kept).
	applyAudioPresentationShift(&o, fts)
	// Options.ChapterMarkers: video.ft.presentMs is derived exactly as the
	// full pass derives fts[primaryIndex(fts)].presentMs, so the same source
	// and option yield the same chapters (byte parity full-pass <-> plan).
	chapters := chapterMarkers(&o, c.Chapters, video.ft.presentMs)
	p.master = buildMasterPlaylist(&o, fts, p.subs, segs, nil)
	if o.Encrypt == nil {
		p.mpd = buildDASHManifest(&o, fts, p.subs, p.durs, peakBandwidth(segs), chapters)
	}
	meta := movieMeta{title: c.Info.Title, tags: globalTags(c), cover: pickCoverArt(c.Attachments)}
	for i, ft := range fts {
		m := movieMeta{}
		if ft.outTrack.spec.video {
			m = meta
		}
		p.inits = append(p.inits, buildInitSegment([]*fragTrack{ft}, m, o.CENC))
		i := i
		var chs []mkv.Chapter
		if ft.outTrack.spec.video {
			chs = chapters
		}
		p.medias = append(p.medias, buildMediaPlaylist(&o, p.durs, renditionInit(fts, i),
			func(k int) string { return renditionSegment(fts, i, k) }, chs))
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

// hlsResult exposes the plan's packaging facts in the shape RemuxToHLS returns,
// so the ABR master builder treats a plan and a full pass identically.
func (p *HLSPlan) hlsResult() *hlsResult {
	return &hlsResult{fts: p.fts(), subs: p.subs, segs: p.segs, durs: p.durs, bounds: p.bounds, iframes: p.iframes}
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

// MediaPlaylist returns playlist.m3u8 - the video rendition's playlist
// (audio renditions have their own, audioK.m3u8, served via Resource).
func (p *HLSPlan) MediaPlaylist() []byte { return p.medias[p.videoIndex()] }

// InitSegment returns init.mp4 - the video rendition's init segment, which
// also carries the movie metadata (title, tags, cover art).
func (p *HLSPlan) InitSegment() []byte { return p.inits[p.videoIndex()] }

// peekHead reads from the first cued cluster until every track has its first
// sample: the first PTS anchors the DTS timeline, and the sample entry is
// built for codecs that derive it from the first frame.
func (p *HLSPlan) peekHead(ctx context.Context) error {
	// Grid probe for laced audio with no DefaultDuration: the reader collapses a
	// lace onto its block timecode, so the plan recovers the stride the same way
	// the full pass does - from the first two distinct block timecodes and the
	// first block's frame count - and the init's total duration matches.
	type gridProbe struct {
		firstTC, frames, secondTC int64
		haveFirst, haveSecond     bool
	}
	probes := map[*planTrack]*gridProbe{}
	needFirst := len(p.tracks)
	needGrid := 0
	for _, pt := range p.tracks {
		mk := &pt.ft.outTrack.mkv
		if mk.Type == mkv.AudioTrack && mk.DefaultDurationNs <= 0 {
			probes[pt] = &gridProbe{}
			needGrid++
		}
	}
	err := p.walkBlocks(ctx, p.offsets[0], func(b mkv.Block, pt *planTrack) (bool, error) {
		if pt.firstPtsMs < 0 {
			data := pt.ft.outTrack.mkv.RestoreHeader(b.Data)
			if pt.ft.outTrack.sampleEntry == nil {
				entry, err := pt.ft.outTrack.spec.sampleEntry(&pt.ft.outTrack.mkv, data)
				if err != nil {
					return false, err
				}
				pt.ft.outTrack.sampleEntry = entry
			}
			pt.firstPtsMs = b.Timecode
			needFirst--
		}
		if pr := probes[pt]; pr != nil && !pr.haveSecond {
			switch {
			case !pr.haveFirst:
				pr.firstTC, pr.frames, pr.haveFirst = b.BlockTimecode, 1, true
			case b.BlockTimecode == pr.firstTC:
				pr.frames++
			default:
				pr.secondTC, pr.haveSecond = b.BlockTimecode, true
				needGrid--
			}
		}
		return needFirst > 0 || needGrid > 0, nil
	})
	if err != nil {
		return err
	}
	for pt, pr := range probes {
		if !pr.haveSecond {
			continue
		}
		pt.gridTS = deriveGridTS(int(pr.frames)+1, func(i int) int64 {
			if int64(i) < pr.frames {
				return pr.firstTC
			}
			return pr.secondTC
		}, pt.ft.timescale)
	}
	return nil
}

// peekTail reads from the last cued cluster to EOF and returns each track's
// two largest PTS (the final sample and its predecessor), -1 when unseen, plus
// the frame count of the final Block (frames sharing the largest block
// timecode). lastFrames lets the grid duration correct a collapsed no-
// DefaultDuration lace, whose frames all report the block timecode as their PTS.
func (p *HLSPlan) peekTail(ctx context.Context, off int64) (lastPts, prevPts, lastFrames []int64, err error) {
	lastPts = make([]int64, len(p.tracks))
	prevPts = make([]int64, len(p.tracks))
	lastFrames = make([]int64, len(p.tracks))
	lastBlockTC := make([]int64, len(p.tracks))
	for i := range lastPts {
		lastPts[i], prevPts[i], lastBlockTC[i] = -1, -1, -1
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
		switch {
		case b.BlockTimecode > lastBlockTC[i]:
			lastBlockTC[i], lastFrames[i] = b.BlockTimecode, 1
		case b.BlockTimecode == lastBlockTC[i]:
			lastFrames[i]++
		}
		return true, nil
	})
	return lastPts, prevPts, lastFrames, err
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
	br.SetTrackDefaultDurations(p.trackDurs)
	routing := make(map[uint64]*planTrack, len(p.tracks))
	for _, pt := range p.tracks {
		routing[pt.ft.outTrack.mkv.ID] = pt
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if isBlockWalkEnd(err) { // clean end, incl. a truncated/over-declared tail
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

// walkVideoHeadersOnly walks the video track's blocks from the presentation
// start to EOF, structure only: SetHeaderOnly skips the payload of every
// (real-world universal) unlaced block instead of reading it, and KeepTracks
// seek-skips every other track's blocks entirely - so the cost is bounded by
// the video track's block-header count, never by any payload bytes.
func (p *HLSPlan) walkVideoHeadersOnly(ctx context.Context, fn func(mkv.Block) (bool, error)) error {
	vid := p.tracks[p.videoIndex()].ft.outTrack.mkv.ID
	src, err := p.fs.DoOpen(p.srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	br, err := reader.NewBlockReaderAt(src, p.tcScale, p.offsets[0])
	if err != nil {
		return err
	}
	br.SetTrackDefaultDurations(p.trackDurs)
	br.KeepTracks(vid)
	br.SetHeaderOnly(true)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if isBlockWalkEnd(err) { // clean end, incl. a truncated/over-declared tail
			return nil
		}
		if err != nil {
			return errf("read block: %w", err)
		}
		more, err := fn(b)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// synthesizeVideoCues is Options.SynthesizeIndex's one-time walk: the cue
// points a healthy index would carry - one per cluster holding a video
// keyframe, at the keyframe's stored timecode - built in memory from a
// structure-only pass (KeepTracks + SetHeaderOnly: block headers, never
// payload bytes). The cost is the walk a persistent repair would pay anyway,
// but nothing is written: the only road to seekable on-demand playback for a
// source on a read-only mount. A structurally broken cluster stream still
// refuses (repair or salvage first).
func synthesizeVideoCues(ctx context.Context, srcPath string, fs *mkv.FS, c *mkv.Container, vidID uint64, trackDurs map[uint64]int64) ([]mkv.CuePoint, error) {
	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
	if err != nil {
		return nil, errf("synthesize index: %w", err)
	}
	br.SetTrackDefaultDurations(trackDurs)
	br.KeepTracks(vidID)
	br.SetHeaderOnly(true)
	var cues []mkv.CuePoint
	lastCluster := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := br.Next()
		if isBlockWalkEnd(err) {
			return cues, nil
		}
		if err != nil {
			return nil, errf("synthesize index: %w (repair the source first: `mkvgo reindex --resync`)", err)
		}
		if !b.Keyframe {
			continue
		}
		off := br.ClusterOffset()
		if off == lastCluster {
			continue // one cue per cluster, like real muxers write
		}
		lastCluster = off
		cues = append(cues, mkv.CuePoint{
			TimeMs:     b.BlockTimecode,
			Track:      vidID,
			ClusterPos: off - c.SegmentStart,
		})
	}
}

// iframePlaylist returns the trick-play playlist, built once and cached. MP4
// plans build it eagerly at PlanHLS time (the sample table makes it free);
// Matroska plans build it lazily, on whichever request needs it first, from
// a structure-only pass over the video track (walkVideoHeadersOnly - no
// sample bytes read for the unlaced case), so PlanHLS's construction stays
// the bounded few reads it always was. Only a PERMANENT error is cached (a
// cancelled first request must not poison every later one - the same fix as
// the subtitle-cue scan's).
func (p *HLSPlan) iframePlaylist(ctx context.Context) ([]byte, error) {
	if p.mp4src {
		if p.iframe == nil {
			return nil, errf("no I-frame playlist for this plan (an encrypted presentation does not expose one)")
		}
		return p.iframe, nil
	}
	if !p.hasIframe {
		return nil, errf("no I-frame playlist for this plan (an encrypted presentation does not expose one)")
	}
	p.iframeMu.Lock()
	defer p.iframeMu.Unlock()
	if p.iframe != nil {
		return p.iframe, nil
	}
	if p.iframeErr != nil {
		return nil, p.iframeErr
	}
	pl, iframes, err := p.buildMatroskaIframePlaylist(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // transient - never cached
		}
		p.iframeErr = err
		return nil, err
	}
	p.iframe = pl
	p.iframes = iframes
	return pl, nil
}

// buildMatroskaIframePlaylist performs the one-time structure-only walk: for
// every segment it collects the video track's samples (size/pts/blockPts/
// sync, never the bytes) and feeds them to timeSegmentWindow - the very
// derivation Segment(n) uses - to build the SAME trun/moof RemuxToHLS and
// Segment(n) would write, byte for byte, since that derivation never touches
// sample content. Each segment's I-frame byte range is then the moof's exact
// length plus its leading (keyframe) sample's exact size.
func (p *HLSPlan) buildMatroskaIframePlaylist(ctx context.Context) ([]byte, []iframeRef, error) {
	vi := p.videoIndex()
	pt := p.tracks[vi]
	windows := make([][]fragSample, p.segCount)

	k := 0
	err := p.walkVideoHeadersOnly(ctx, func(b mkv.Block) (bool, error) {
		for k+1 < p.segCount && b.BlockTimecode >= p.bounds[k+1] {
			k++
		}
		windows[k] = append(windows[k], fragSample{
			size:       uint32(int64(len(pt.ft.outTrack.mkv.HeaderStripping)) + b.Size),
			ptsMs:      b.Timecode,
			blockPtsMs: b.BlockTimecode,
			sync:       b.Keyframe,
		})
		return true, nil
	})
	if err != nil {
		return nil, nil, err
	}

	var iframes []iframeRef
	for seg := range windows {
		window := windows[seg]
		nextPts := int64(-1)
		for next := seg + 1; next < len(windows); next++ {
			if len(windows[next]) > 0 {
				nextPts = windows[next][0].ptsMs
				break
			}
		}
		ts := trackSegment{trackID: pt.ft.outTrack.mp4ID}
		ts.baseDecodeTS, ts.hasCTS = timeSegmentWindow(window, pt, nextPts)
		if len(window) > 0 {
			ts.samples = window
			for _, s := range window {
				ts.dataLen += int64(s.size)
			}
		}
		head := buildSegmentFile(uint32(seg+1), ts)
		if len(window) > 0 && window[0].sync {
			iframes = append(iframes, iframeRef{seg: seg, length: int64(len(head)) + int64(window[0].size)})
		}
	}
	if len(iframes) == 0 {
		return nil, nil, errf("no I-frame available for this presentation")
	}
	fts := p.fts()
	return buildIFramePlaylist(&p.opts, fts, p.durs, iframes), iframes, nil
}

// segSample is one sample of an on-demand window: the fragment metadata plus
// its bytes (a single segment's data is held in memory - a few MB).
type segSample struct {
	fragSample
	data []byte
}

// timeSegmentWindow derives one segment window's per-sample dtsTS/ctsTS/durTS
// (mutating window in place) purely from metadata - size, ptsMs, blockPtsMs,
// sync - never the sample bytes, applying fillFragTiming's rule window-local:
// grid-timed audio re-derives each frame's index from its block's stored
// timecode, otherwise samples are DTS-sorted and CTS is the PTS/DTS gap.
// nextPts is the first sample of the window that follows (-1 for the
// presentation's last window, which closes on pt.lastDurTS instead). It
// returns the window's base decode time and whether any sample carries a
// non-zero CTS. Shared by segmentTrack (the window's bytes ride along in a
// parallel slice) and the structure-only I-frame builder (bytes never read
// at all), so both derive byte-identical segment heads from the same math.
func timeSegmentWindow(window []fragSample, pt *planTrack, nextPts int64) (baseDecodeTS int64, hasCTS bool) {
	if len(window) == 0 {
		return pt.ft.durMediaTS, false
	}
	// gridTS from the DefaultDuration, else recovered from the window's own
	// collapsed laces (the stride is uniform, so every window measures the same
	// value the full pass derives from all samples - parity by construction).
	gridTS := pt.gridTS
	if gridTS <= 0 {
		gridTS = deriveGridTS(len(window), func(i int) int64 { return window[i].blockPtsMs }, pt.ft.timescale)
	}
	if gridTS > 0 {
		// Grid-timed audio: each frame's index re-derived from its block's
		// stored timecode (then +1 within a lace) - fillFragTiming's grid
		// branch applied to the window, window-local by construction.
		scale := tsScale(pt.ft.timescale)
		anchor := scale(pt.firstPtsMs)
		k := int64(0)
		for x := range window {
			switch {
			case x == 0:
				k = gridIndex(scale(window[0].blockPtsMs)-anchor, gridTS)
			case window[x].blockPtsMs != window[x-1].blockPtsMs:
				nk := gridIndex(scale(window[x].blockPtsMs)-anchor, gridTS)
				if nk <= k {
					nk = k + 1
				}
				k = nk
			default:
				k++
			}
			window[x].dtsTS = k * gridTS
			window[x].ctsTS = 0
			if x > 0 {
				window[x-1].durTS = window[x].dtsTS - window[x-1].dtsTS
			}
		}
		if nextPts >= 0 {
			nk := gridIndex(scale(nextPts)-anchor, gridTS)
			if nk <= k {
				nk = k + 1
			}
			window[len(window)-1].durTS = nk*gridTS - k*gridTS
		} else { // the track's final sample (fillFragTiming's grid rule)
			window[len(window)-1].durTS = gridTS
		}
		return window[0].dtsTS, false
	}
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
	return dts[0] - base, windowHasCTS(window)
}

// Segment builds the n-th (0-based) VIDEO media segment (segNNNNN.m4s)  -
// audio renditions' segments are served through Resource by name. The bytes
// equal the corresponding segment RemuxToHLS writes.
func (p *HLSPlan) Segment(ctx context.Context, n int) ([]byte, error) {
	return p.segmentTrack(ctx, p.videoIndex(), n)
}

// segmentTrack builds the n-th segment of the ti-th track - styp + moof +
// mdat - reading only that segment's window from the source: it seeks to the
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
	// belongs to the window whatever its PTS - open-GOP leading B-frames ride
	// with the keyframe that follows them in presentation but precedes them
	// in decode.
	var window []segSample
	started := n == 0
	nextPts := int64(-1)
	err := p.walkBlocks(ctx, p.offsets[n], func(b mkv.Block, wpt *planTrack) (bool, error) {
		if wpt != pt {
			return true, nil
		}
		// Window membership keys on the enclosing block's stored timecode -
		// exactly like the full pass's cursor - so a lace is never split.
		if !started {
			if b.BlockTimecode < segStart {
				return true, nil // interleaved leftovers of the previous window
			}
			started = true
		}
		if b.BlockTimecode >= segEnd {
			nextPts = b.Timecode
			return false, nil
		}
		data := pt.ft.outTrack.mkv.RestoreHeader(b.Data)
		window = append(window, segSample{
			fragSample: fragSample{size: uint32(len(data)),
				ptsMs: b.Timecode, blockPtsMs: b.BlockTimecode, sync: b.Keyframe},
			data: data,
		})
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	// Fragment timing - fillFragTiming's DTS derivation applied to the window,
	// anchored on the track's global first PTS, with the boundary lookahead
	// supplying the window's final sample duration. An empty window (the track
	// ended before the presentation) becomes an empty trun with the decode
	// time parked at the stream end, keeping the rendition's segments aligned.
	// timeSegmentWindow derives this from metadata alone (size/pts/blockPts/
	// sync), so it is shared verbatim with the structure-only I-frame builder,
	// which never reads the window's sample bytes at all.
	seg := trackSegment{trackID: pt.ft.outTrack.mp4ID}
	metas := make([]fragSample, len(window))
	for x := range window {
		metas[x] = window[x].fragSample
	}
	seg.baseDecodeTS, seg.hasCTS = timeSegmentWindow(metas, pt, nextPts)
	if len(window) > 0 {
		for x := range window {
			window[x].fragSample = metas[x]
			seg.dataLen += int64(window[x].size)
		}
		seg.samples = metas
	}

	var cipherData []byte
	if p.opts.CENC != nil {
		// Same requirement as the full pass (hls.go's writeSegments): CENC needs
		// the plaintext bytes before the head is built, so the moof's senc/saiz
		// (sized from the samples) are ready when buildSegmentFile frames it.
		plain := make([]byte, 0, seg.dataLen)
		for x := range window {
			plain = append(plain, window[x].data...)
		}
		td, cipher, err := prepareCENCSegment(p.opts.CENC, pt.ft.outTrack.spec.video, pt.ft.outTrack.mkv.Codec, seg.samples, plain)
		if err != nil {
			return nil, err
		}
		seg.cenc = td
		cipherData = cipher
	}
	head := buildSegmentFile(uint32(n+1), seg)
	out := make([]byte, 0, int64(len(head))+seg.dataLen)
	out = append(out, head...)
	if p.opts.CENC != nil {
		out = append(out, cipherData...)
	} else {
		for x := range window {
			out = append(out, window[x].data...)
		}
	}
	if p.opts.Encrypt != nil {
		return p.opts.Encrypt.encryptSegment(out, uint32(n))
	}
	return out, nil
}

// Resources returns every resource name the plan serves - the HLS master, the
// DASH manifest (same CMAF segments, second manifest), then per rendition its
// media playlist, init segment and media segments (video first, then each
// audio track), and each subtitle rendition (its HLS playlist and its WebVTT).
// Serving a presentation is: answer any of these names through Resource.
func (p *HLSPlan) Resources() []string {
	names := []string{"master.m3u8"}
	if p.iframe != nil || p.hasIframe {
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
// The name is exactly the URI a player requests relative to the playlist  -
// "master.m3u8", "playlist.m3u8", "init.mp4", "seg00042.m4s", "sub1.m3u8",
// "sub1.vtt" - so an HTTP handler is a one-liner around this call. Playlists
// and the init segment are precomputed; media segments read their window;
// a subtitle .vtt runs one sequential pass over the source (lazily - cache it
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
		pl, err := p.iframePlaylist(ctx)
		if err != nil {
			return nil, "", err
		}
		return pl, mimeM3U8, nil
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
			func(k int) string { return fmt.Sprintf("sub%d_%05d.vtt", i, k+1) }, nil)
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

// subCursor is one bounded, resumable cue scan over the clusters: it holds
// every cue of its track whose block lives in a cluster stamped within
// [baseMs, scanned]. The prefix cursor (baseMs 0) grows from the track start;
// a fast-path island starts mid-file after a far seek and slides forward.
type subCursor struct {
	baseMs  int64          // cluster-timestamp floor the scan started at (0 = track start)
	cues    []subtitle.Cue // raw cues in block order
	next    int64          // cluster offset to resume from; 0 = not started
	scanned int64          // clusters with timestamps up to this ms are consumed
	done    bool           // scanned to EOF
}

// subScanState is one subtitle track's serving state: the exact prefix scan
// (whole-track .vtt, sequential playback, and the fallback), one sliding
// island for far seeks, and the flag that gates the island path.
type subScanState struct {
	prefix subCursor
	window *subCursor // fast-seek island; replaced when a jump lands before it
	noFast bool       // a duration-less or over-cap cue was observed: prefix only
	err    error      // permanent (non-context) scan error, replayed
}

const (
	// subScanStrideMs is one scan increment: progress commits and the
	// caller's ctx is honoured at each stride, so a client disconnect keeps
	// the clusters already walked and a later request resumes.
	subScanStrideMs = 60_000

	// subFastMaxCueDurMs is the longest cue the fast-seek path accounts for:
	// its backward margin. A cue longer than this observed ANYWHERE (the
	// track-head probe, a prefix scan, an island scan) - or any cue without
	// an explicit duration, whose end resolves on an arbitrarily-far
	// successor - drops the track to the always-exact prefix path. A
	// violating cue that only lives in never-scanned regions can escape this
	// net; real subtitle muxing (explicit BlockDurations, cues of seconds)
	// stays exact.
	subFastMaxCueDurMs = 60_000
)

// relTCMaxMs is how far (ms) a block's timecode can sit from its cluster's
// timestamp: the ±32767-tick relative timecode range at the source's scale.
func (p *HLSPlan) relTCMaxMs() int64 {
	if p.tcScale > 0 && p.tcScale < math.MaxInt64/32767 {
		return 32767 * p.tcScale / 1_000_000
	}
	return 32767
}

// subCuesThrough returns the i-th track's cues, complete and end-resolved for
// every cue starting before uptoMs (math.MaxInt64 = the whole track),
// advancing the prefix cursor as far as that requires and no further.
func (p *HLSPlan) subCuesThrough(ctx context.Context, i int, uptoMs int64) ([]subtitle.Cue, error) {
	if p.mp4src {
		return p.subs[i].cues, nil // decoded at plan time from the sample table
	}
	p.subMu[i].Lock()
	cues, err := p.subPrefixLocked(ctx, i, uptoMs)
	p.subMu[i].Unlock()
	if err != nil {
		return nil, err
	}
	// Resolving on the prefix equals resolving on the full track for every
	// cue starting before uptoMs: the scan guarantees their successors
	// (which duration-less ends resolve on) are in the prefix.
	subtitle.ResolveCueEnds(cues, 2000)
	return cues, nil
}

// subPrefixLocked extends the prefix cursor to cover cues starting before
// uptoMs and returns a copy of it. Caller holds subMu[i].
func (p *HLSPlan) subPrefixLocked(ctx context.Context, i int, uptoMs int64) ([]subtitle.Cue, error) {
	st := &p.subScan[i]
	target := uptoMs
	if relMax := p.relTCMaxMs(); target < math.MaxInt64-relMax {
		target += relMax
	}
	if err := p.extendCursorLocked(ctx, i, &st.prefix, target, uptoMs); err != nil {
		return nil, err
	}
	return append([]subtitle.Cue(nil), st.prefix.cues...), nil
}

// subCuesForWindow returns cues sufficient to window [segStart, segEnd):
// from the prefix when it is the cheaper (or the only exact) vehicle, or
// from a seeked island when the request jumps far past the prefix - the
// direct-play property: a cold seek costs O(window), not O(position). The
// island's backward margin covers cues started before the window
// (subFastMaxCueDurMs) and the relative-timecode spread of their blocks.
func (p *HLSPlan) subCuesForWindow(ctx context.Context, i int, segStart, segEnd int64) ([]subtitle.Cue, error) {
	if p.mp4src {
		return p.subs[i].cues, nil
	}
	relMax := p.relTCMaxMs()
	target := segEnd
	if target < math.MaxInt64-relMax {
		target += relMax
	}
	neededFrom := segStart - subFastMaxCueDurMs - relMax
	if neededFrom < 0 {
		neededFrom = 0
	}

	p.subMu[i].Lock()
	st := &p.subScan[i]
	prefixCovers := st.prefix.done ||
		(st.prefix.scanned >= target && !subNeedsNextCue(st.prefix.cues, segEnd))
	// The prefix serves when it already covers the window, when the window
	// starts within (or near) it, or when extending it reads no more than a
	// fresh island would - sequential playback stays on the prefix.
	usePrefix := prefixCovers || neededFrom <= st.prefix.scanned ||
		target-st.prefix.scanned <= target-neededFrom
	if !usePrefix && !st.noFast && st.prefix.scanned == 0 && !st.prefix.done {
		// Probe the track head once before trusting a jump: the first stride
		// reveals the track's cue style (explicit durations or not).
		if err := p.extendCursorLocked(ctx, i, &st.prefix, subScanStrideMs, 0); err != nil {
			p.subMu[i].Unlock()
			return nil, err
		}
	}
	if !usePrefix && !st.noFast {
		cues, err := p.subWindowLocked(ctx, i, neededFrom, target, segEnd)
		if err != nil {
			p.subMu[i].Unlock()
			return nil, err
		}
		if !st.noFast {
			p.subMu[i].Unlock()
			return cues, nil // island cues all carry explicit ends
		}
		st.window = nil // a violation surfaced mid-island: prefix from now on
	}
	cues, err := p.subPrefixLocked(ctx, i, segEnd)
	p.subMu[i].Unlock()
	if err != nil {
		return nil, err
	}
	subtitle.ResolveCueEnds(cues, 2000)
	return cues, nil
}

// subWindowLocked serves through the sliding island: reuse it while its
// floor covers the needed lookback AND its scan front has reached it  -
// sliding forward is then incremental. A jump landing before the island or
// far past its front seeks a fresh island through the segment index instead
// of dragging the old one across the gap. Caller holds subMu[i].
func (p *HLSPlan) subWindowLocked(ctx context.Context, i int, neededFrom, target, uptoMs int64) ([]subtitle.Cue, error) {
	st := &p.subScan[i]
	if st.window == nil || st.window.baseMs > neededFrom || st.window.scanned < neededFrom {
		k := sort.Search(len(p.bounds), func(k int) bool { return p.bounds[k] > neededFrom }) - 1
		if k < 0 {
			k = 0
		}
		st.window = &subCursor{baseMs: p.bounds[k], next: p.offsets[k], scanned: p.bounds[k]}
	}
	if err := p.extendCursorLocked(ctx, i, st.window, target, uptoMs); err != nil {
		return nil, err
	}
	return append([]subtitle.Cue(nil), st.window.cues...), nil
}

// extendCursorLocked advances cur until it holds every cue starting before
// uptoMs (within its floor) with resolvable ends. Two bounds drive the walk:
// blocks with timecode T live in clusters stamped within the relative-
// timecode range of T, and a duration-less cue's end is the NEXT cue's start
// - the scan keeps going while the last known cue before uptoMs awaits a
// successor. Cancellation between strides keeps the committed progress; only
// permanent errors are cached. Caller holds subMu[i].
func (p *HLSPlan) extendCursorLocked(ctx context.Context, i int, cur *subCursor, target, uptoMs int64) error {
	st := &p.subScan[i]
	if st.err != nil {
		return st.err
	}
	if cur.done || (cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs)) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	track := &p.subs[i]
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
		st.err = err
		return err
	}
	br.SetTrackDefaultDurations(p.trackDurs)
	br.KeepTracks(track.track.ID)
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
			// Clean end (incl. a truncated/over-declared tail); all cues to this
			// point are collected, so finish cleanly.
			if isBlockWalkEnd(err) {
				cur.cues = append(cur.cues, local...)
				cur.done = true
				return nil
			}
			if err != nil {
				st.err = errf("read block: %w", err)
				return st.err
			}
			if b.TrackNumber != track.track.ID {
				continue
			}
			if cue, ok := subCueFromBlock(track.track.Codec, b); ok {
				if cue.EndMs <= cue.StartMs || cue.EndMs-cue.StartMs > subFastMaxCueDurMs {
					st.noFast = true
				}
				local = append(local, cue)
			}
		}
		if st.noFast && cur.baseMs > 0 {
			return nil // the island is doomed; the caller falls back to the prefix
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

// Subtitle builds the i-th (0-based) subtitle rendition's WebVTT - the whole
// track as one file. It drives the cue cursor to the end of the source; the
// cues collected stay cached for the windowed segments (and vice versa).
// Options.SubtitleOffsetMs shifts every cue (see subtitle.ShiftCues) before
// it is written.
func (p *HLSPlan) Subtitle(ctx context.Context, i int) ([]byte, error) {
	if i < 0 || i >= len(p.subs) {
		return nil, errf("subtitle rendition %d out of range (0..%d)", i, len(p.subs)-1)
	}
	cues, err := p.subCuesThrough(ctx, i, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	cues = subtitle.ShiftCues(cues, p.opts.SubtitleOffsetMs)
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, cues); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// subtitleSegment builds the i-th rendition's n-th windowed WebVTT segment
// (subN_%05d.vtt) - the same windows the full pass writes, byte-identical.
// Only the prefix of the source the window can draw cues from is scanned, so
// serving segments in playback order reads the source once, incrementally  -
// the first hit costs one bounded window, like a video segment.
//
// Options.SubtitleOffsetMs shifts every cue before windowing (segment
// boundaries apply AFTER the shift, matching the full pass): the fetch range
// is the shift's pre-image (a cue's SOURCE time that will land in [segStart,
// segEnd) once shifted is [segStart-offset, segEnd-offset)), so the right
// source cues are pulled in for any offset, however large.
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
	off := p.opts.SubtitleOffsetMs
	srcStart, srcEnd := segStart, segEnd
	if off != 0 {
		srcStart = segStart - off
		if srcStart < 0 {
			srcStart = 0 // subCuesForWindow clamps its own lookback to 0 too
		}
		if segEnd != math.MaxInt64 {
			srcEnd = segEnd - off
		}
	}
	cues, err := p.subCuesForWindow(ctx, i, srcStart, srcEnd)
	if err != nil {
		return nil, err
	}
	cues = subtitle.ShiftCues(cues, off)
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
	// sampleFull builds the sample table from the moof fragments for a
	// fragmented/CMAF source, so the packager reads it like a progressive file.
	mv, err := parseMP4(f, st.Size(), sampleFull)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &packagingSource{c: containerFromMovie(mv), mv: mv, src: f, size: st.Size()}, nil
}

// planHLSFromMP4 builds the on-demand plan from an MP4 source's sample table:
// every window, size and duration is known head-only, so the plan's outputs  -
// including the master's BANDWIDTH - equal the full pass exactly.
func planHLSFromMP4(ctx context.Context, ps *packagingSource, srcPath string, fs *mkv.FS, o *Options, segMs int64) (*HLSPlan, error) {
	c := ps.c
	planned, _, err := planTracks(c, *o)
	if err != nil {
		return nil, err
	}
	keep := keepTrackSet(o)
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
			continue
		}
		media = append(media, t)
	}
	if len(media) == 0 {
		return nil, errf("no audio or video track to segment")
	}
	if err := cencPreflight(o, videoCodecOf(media)); err != nil {
		return nil, err
	}

	p := &HLSPlan{srcPath: srcPath, fs: fs, opts: *o, mp4src: true}
	if !o.VideoOnly {
		p.subs = filterSubTracks(planSubTracks(c, *o), keep)
	}
	p.subMu = make([]sync.Mutex, len(p.subs))
	p.subScan = make([]subScanState, len(p.subs))
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
	// Options.AudioPresentationShift, after the planTracks captured the
	// UNSHIFTED first PTS (the decode-time anchor): the shift lives in the
	// init's edit list alone, exactly like the Matroska plan.
	applyAudioPresentationShift(o, fts)

	p.bounds = segmentBoundaries(fts[primaryIndex(fts)].samples, segMs)
	p.segCount = len(p.bounds)

	// Exact per-boundary aggregates: window sizes are computable without
	// touching the media (deterministic builders), so BANDWIDTH - and with it
	// the master playlist and the DASH manifest - equal the full pass.
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
	if video != nil && o.Encrypt == nil && o.CENC == nil && len(iframes) > 0 {
		p.iframe = buildIFramePlaylist(o, fts, p.durs, iframes)
		p.iframes = iframes
	}

	p.segs = segs
	// Options.ChapterMarkers: chapters ride in Container.Chapters already
	// (containerFromMovie carries the MP4 chpl box head-only, same as the
	// full pass's ps.c.Chapters - no extra read needed for an MP4 source).
	chapters := chapterMarkers(o, c.Chapters, fts[primaryIndex(fts)].presentMs)
	p.master = buildMasterPlaylist(o, fts, p.subs, segs, iframes)
	if o.Encrypt == nil {
		p.mpd = buildDASHManifest(o, fts, p.subs, p.durs, peakBandwidth(segs), chapters)
	}
	meta := movieMeta{title: c.Info.Title, tags: globalTags(c)}
	for i, ft := range fts {
		m := movieMeta{}
		if i == primaryIndex(fts) {
			m = meta
		}
		p.inits = append(p.inits, buildInitSegment([]*fragTrack{ft}, m, o.CENC))
		i := i
		var chs []mkv.Chapter
		if i == primaryIndex(fts) && pickVideoFrag(fts) != nil {
			chs = chapters
		}
		p.medias = append(p.medias, buildMediaPlaylist(o, p.durs, renditionInit(fts, i),
			func(k int) string { return renditionSegment(fts, i, k) }, chs))
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
	// starts where the previous one stopped - at the first sample (decode
	// order) at/past its boundary.
	start := 0
	for k := 1; k <= n; k++ {
		for start < len(ft.samples) && ft.samples[start].blockPtsMs < p.bounds[k] {
			start++
		}
	}
	end := start
	if n+1 < p.segCount {
		for end < len(ft.samples) && ft.samples[end].blockPtsMs < p.bounds[n+1] {
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

	var plain []byte
	if end > start {
		plain = make([]byte, 0, seg.dataLen)
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
			plain = append(plain, data...)
		}
	}

	cipherData := plain
	if p.opts.CENC != nil {
		// Same requirement as the full pass and the Matroska on-demand path:
		// CENC needs the plaintext bytes before the head is built.
		td, cipher, err := prepareCENCSegment(p.opts.CENC, ft.outTrack.spec.video, ft.outTrack.mkv.Codec, seg.samples, plain)
		if err != nil {
			return nil, err
		}
		seg.cenc = td
		cipherData = cipher
	}
	head := buildSegmentFile(uint32(n+1), seg)
	out := make([]byte, 0, int64(len(head))+int64(len(cipherData)))
	out = append(out, head...)
	out = append(out, cipherData...)

	if p.opts.Encrypt != nil {
		return p.opts.Encrypt.encryptSegment(out, uint32(n))
	}
	return out, nil
}
