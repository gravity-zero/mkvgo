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
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// HLSPlan is the result of PlanHLS: the playlists, the init segment, and the
// per-segment window index. It is immutable after PlanHLS returns and safe for
// concurrent Segment calls (each opens its own reader).
type HLSPlan struct {
	srcPath       string
	fs            *mkv.FS
	tcScale       int64
	segEnd        int64 // Container.SegmentEnd: where a walk opened at an offset stops, 0 when unknown
	tracks        []*planTrack
	bounds        []int64 // segment start times (ms); bounds[0] == 0
	offsets       []int64 // absolute file offset of the cluster holding bounds[k]
	clusters      []int64 // absolute offsets of every cued cluster, ascending
	clusterOpenMs []int64 // the cue time each cued cluster opens on, parallel to clusters
	// aheadMs is the furthest a non-video block has been seen stored ahead of
	// the cued cluster that follows the one holding it (audio a muxer writes
	// ahead of its video): how far before its boundary a cold walk must open.
	aheadMs  atomic.Int64
	durs     []float64
	inits    [][]byte // one init segment per track (video first, per fts order)
	medias   [][]byte // one media playlist per track
	master   []byte
	mpd      []byte
	segCount int
	// cueDensity measures the video cue points the bounds were cut on (CueDensity).
	cueDensity CueDensity
	opts       Options         // Encrypt / RewriteURL ride along for Resource builds
	subs       []hlsSubTrack   // declared renditions; cues fetched lazily, then cached
	subMu      []sync.Mutex    // serialises each track's cue scans
	subScan    []subScanState  // per-track incremental cue scan state
	segs       []segInfo       // per-segment duration/bytes (BANDWIDTH; ABR master reuse)
	mp4src     bool            // MP4 source: windows cut from the sample table, exact by construction
	share      mp4Share        // a remote MP4 window's audio, kept by its video read
	mp4tabs    []*mp4PlanTable // per track: the compact sample table the windows are built from (MP4 source)
	mp4subs    []*mp4SubTable  // per subtitle rendition: its samples, text read on first use (MP4 source)
	iframe     []byte          // trick-play playlist: MP4 plans build it eagerly (free, head-only);
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
	// segPos holds, per segment, the exact block its window opens on - the
	// Cues can only name the enclosing CLUSTER. It matters only where a
	// cluster is long enough to hold the window (some muxers write multi-second
	// clusters): the walk then skips the cluster prefix ahead of the window
	// instead of reading it. Real releases cluster every ~2s, so a 6s segment
	// spans several clusters and there is no prefix to skip - the entry is
	// simply never a win there, and never a cost either. Each walk stops on the
	// block the NEXT window opens on, so playback order learns them for free.
	// Entries are pure hints: a miss walks from the cluster, so a seek anywhere,
	// a fresh plan or a concurrent request stays correct.
	segMu  sync.Mutex
	segPos []reader.BlockPos
	// trackPos holds, per segment, per track, the block each TRACK's window
	// opens on - learned by the walks exactly like segPos. It is what serves a
	// source laid out in single-track BLOCKS (a two-input remux with a large
	// interleave delta writes minutes of one track, then minutes of the next):
	// there the audio of an instant lies megabytes past its video, and one
	// linear walk from the video's cluster reads the whole video block before
	// it reaches the audio. Once every track's opening block is known and they
	// lie far apart (scatteredWindow), the window is read as one short walk
	// per track from its own block instead. The bytes are the same either way;
	// only the first window after a seek pays the traversal.
	trackPos [][]reader.BlockPos
	// trackCues: each track's own cue points by time (absolute cluster offsets); trackCueGapMs its widest cue spacing;
	// trackEndMs (segMu): where a walk that met the end of the source last saw it. A walk does not wait for a track
	// past its end, nor for one whose next block lies far past the window's bytes (a block-ordered tail).
	medianSpan    int64 // what a window usually spans (medianSpanOf); spanCap, the most windowSpan reports
	spanCap       int64
	trackCues     [][]mkv.CuePoint
	trackCueGapMs []int64
	trackEndMs    []int64
	// uncuedRegions (segMu, found once: uncuedKnown) are the stretches of clusters the Cues never name, between or after
	// the cued ones: where a track stored apart from the video lives. tailMarks (segMu) are their clusters walked so far.
	uncuedRegions []tailRegion
	uncuedKnown   bool
	tailMarks     []tailMark
	// stamp identifies the source without reading it (size and mtime, else the
	// plan's birth): what a streamed segment's ETag carries. srcInfo is the
	// stat behind it, for the file's identity (checkSource).
	stamp   string
	srcInfo os.FileInfo
	srcID   string // the source's identity beyond its stamp: an ETag or version id the FS declares, else the OS file id
	osStat  bool   // srcInfo is the OS's own stat: os.SameFile and fstat apply
	pinned  bool   // every read verifies the source itself (fstat on an OS file, a PinnedReader elsewhere): no stat before reading
	// srcStatAt is when the path was last stat'ed clean (monotonic ns since clockBase, 0 never), srcChanged
	// that the source was once found changed: final for the plan.
	srcStatAt  atomic.Int64
	srcChanged atomic.Bool
	// sourceID is SourceID, computed once.
	sourceIDOnce sync.Once
	sourceID     string
	// tables are the window tables Open streams from (hlsstream.go), budgeted
	// by tableCacheBytes per plan and tableTotalBudget per process, one build
	// at a time per window (tabFlight).
	tabMu     sync.Mutex
	netSrc    atomic.Int32 // networkSrc's answer: 0 not known yet, 1 local, 2 network
	tabAcct   *tableAccount
	tables    map[int]*windowTable
	tabOrder  []int
	tabBytes  int64
	tabFlight map[int]*tableFlight
	// gridStart holds, per segment, per grid-timed audio track, the frame index
	// its window opens on (-1 unknown): learned by the window before it, as the
	// full pass's running clock has it, or wound up from the window before (warmGridStart).
	gridStart [][]int64

	// windows caches, per segment index, the media of EVERY rendition - built
	// by the one walk that had to read all of it. That walk is the unit of I/O:
	// a source interleaves its tracks block by block and every payload in a real
	// file is too small for the reader to seek past, so reading a window for one
	// rendition reads it for all of them. Serving video and audio through
	// separate walks therefore read the source TWICE to serve it once; this is
	// what makes it once. A bundle is dropped as soon as every rendition has been
	// collected (seconds), and winBudget is only a net against a client that
	// never collects the rest.
	winMu     sync.Mutex
	windows   map[int]*windowBundle
	winFlight map[int]*windowFlight
	winOrder  []int // insertion order, for the byte-budget trim
	winBytes  int64
	winBudget int64 // 0 = derive from the source (see budget); negative = sharing off
	// otherRate is the lightest bytes per ms a window's non-video renditions
	// have shown (half the tracks' BPS tags before any walk, 0 unknown): what
	// newWindowInPlace takes off the span, which counts them too. A lower
	// bound, so the arena stays large enough; a quieter window than any seen
	// falls back to assembly by copy (ArenaFallbacks) and lowers it.
	otherRate float64
	// winPeak is the largest window this plan has met - the budget is twice it.
	// It is seeded from the Cues (the source bytes between two segment boundaries
	// bound the window they hold) so that the FIRST window is already covered,
	// then refined by the bundles actually built - which is what a growing plan,
	// whose segments do not exist yet at construction, relies on entirely.
	winPeak int64
	// stats counts what the windows cost (see HLSPlanStats); winBuilt has one
	// bit per segment index, set once that window has been built. Both are
	// guarded by winMu.
	stats    HLSPlanStats
	winBuilt []uint64
	walkRead atomic.Int64 // HLSPlanStats.WalkReadBytes
	copyRead atomic.Int64 // HLSPlanStats.CopyReadBytes
	streamed atomic.Int64 // HLSPlanStats.StreamedBytes
	// damaged (segMu, sorted by start) are the unreadable regions the walks met: passed over, or refused.
	damaged []damagedRange
	// subIndex is Options.SubtitleIndex when it matches the source's
	// fingerprint, else the index derived from the source's Cues when they
	// cue its subtitle blocks; nil otherwise, and the subtitle scans walk.
	subIndex SubtitleBlockIndex
}

// planTrack is one media track's plan state: the outTrack (sample entry ready)
// plus the timing anchors the per-segment DTS derivation needs.
type planTrack struct {
	// laceSecondMs and laceFirstFrames describe the first collapsed lace peekHead
	// saw (the second block's timecode and the first block's frames): the pace
	// a spread track's final block takes (lastBlockSpan).
	laceSecondMs    int64
	laceFirstFrames int64
	ft              *fragTrack
	firstPtsMs      int64 // the track's first sample PTS - the global DTS origin
	lastDurTS       int64 // the track's final sample duration (fillFragTiming's rule)
	// gridTS is the sample-exact frame stride of a constant-rate audio track
	// (audioGridTS); windows are then timed on the grid, exactly like
	// fillFragTiming's grid branch, instead of on the ms-rounded timecodes.
	gridTS int64
}

// PlanHLS reads the source's metadata, Cues, first and last clusters - a few
// bounded reads, head-only in spirit - and returns a plan that serves an HLS
// presentation segment by segment. The source must carry a Cues index (every
// real muxer writes one; `mkvgo reindex` adds one). See HLSPlan.Segment.
//
// The source is stat'ed before the first read and again once the plan is
// built: a source replaced while the plan read it is ErrSourceChanged, never
// a plan of one file stamped with another.
//
// A source whose FS declares a version for it and pins its readers (a remote
// one) is stat'ed once: every read of the planning is pinned to that version,
// so a source replaced meanwhile fails a read rather than mix in, and the
// planning's readers share what they fetch (mkv.ShareReads).
func PlanHLS(ctx context.Context, srcPath string, opts ...Options) (*HLSPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o := optionsFrom(opts)
	fs := o.FS
	share := &mkv.ReadShare{}
	fi, shared, statErr := statShared(ctx, fs, srcPath, share)
	if !shared {
		fi, statErr = fs.DoStat(srcPath)
	}
	known := statErr == nil
	var before, id string
	osStat := false
	if known {
		id, osStat = statIdentity(fi)
		before = statStamp(fi) + "|" + id
	}
	if known && !osStat && id != "" && (shared || pinsReaders(fs, srcPath)) {
		po := o
		po.FS = pinnedPlanningFS(share, fs, srcPath, fi, id)
		p, err := planHLS(ctx, srcPath, po)
		if err != nil {
			return nil, err
		}
		p.fs, p.opts.FS = fs, fs // serving stats and opens the source itself
		p.walkRead.Store(0)      // SourceReadBytes counts the requests, not the build
		p.copyRead.Store(0)
		return p, nil
	}
	p, err := planHLS(ctx, srcPath, opts...)
	if p != nil {
		p.walkRead.Store(0)
		p.copyRead.Store(0)
	}
	if !known {
		return p, err
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if after, _ := sourceSig(fs, srcPath); after != before {
			return nil, fmt.Errorf("%w (while planning: %v)", ErrSourceChanged, err)
		}
		return nil, err
	}
	if p.stamp+"|"+p.srcID != before {
		return nil, ErrSourceChanged
	}
	return p, nil
}

func planHLS(ctx context.Context, srcPath string, opts ...Options) (*HLSPlan, error) {
	o := optionsFrom(opts)
	fs := o.FS
	segMs := o.SegmentMs
	if segMs <= 0 {
		segMs = defaultSegmentMs
	}
	if err := refuseFrameConverterOnPlan(o.FrameConverter); err != nil {
		return nil, err
	}
	if o.Encrypt != nil {
		if err := o.Encrypt.validate(); err != nil { // before the routing: the MP4 path checked nothing
			return nil, err
		}
	}

	// Source sniff: an MP4's moov IS the index (sample offsets/sizes/sync,
	// head-only), so its plan is exact by construction; a Matroska source
	// plans through its Cues.
	if mp4ps, sniffErr := sniffMP4ForPlan(ctx, srcPath, fs, o.moovLimit()); sniffErr != nil {
		return nil, sniffErr
	} else if mp4ps != nil {
		defer mp4ps.Close()
		return planHLSFromMP4(ctx, mp4ps, srcPath, fs, &o, segMs)
	}

	// WithBitrate: each track's BPS statistic (the Tags are read anyway) -
	// the DASH manifest's audio bandwidth, which read 0 without it.
	// WithInBandColourFallback: the init's colr/mdcv/clli come from the track
	// description, and an HDR10 mux often keeps the static metadata only
	// in-band; one bounded read of the first sample, for such a track only.
	metaOpts := []reader.ReadOption{reader.WithCues(), reader.WithTags(), reader.WithAttachments(), reader.WithoutAttachmentData(), reader.WithBitrate(), reader.WithInBandColourFallback()}
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
		return nil, errf("%s: no Cues index - on-demand HLS seeks through the Cues (run `mkvgo reindex` first, or plan with Options.SynthesizeIndex)", shown.Path(srcPath))
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

	if err := cencPreflight(&o, media); err != nil {
		return nil, err
	}
	p := &HLSPlan{srcPath: srcPath, fs: fs, tcScale: c.Info.TimecodeScale, opts: o,
		trackDurs: reader.TrackDefaultDurations(c.Tracks), hasIframe: o.Encrypt == nil && o.CENC == nil && !o.NoIFrames}
	if o.SubtitleIndex != nil {
		if st, serr := fs.DoStat(srcPath); serr == nil && o.SubtitleIndex.Matches(st.Size(), c.Info.SegmentUID, c.Info.TimecodeScale) {
			p.subIndex = o.SubtitleIndex
		}
	}
	if !o.VideoOnly {
		p.subs = filterSubTracks(planSubTracks(c, o), keep)
	}
	if p.subIndex == nil {
		if ix, verified, unverified := cueSubtitleIndexFrom(c, p.subs); ix != nil {
			p.subIndex = ix
			p.stats.SubtitleCueIndexed, p.stats.SubtitleCueUnverified = verified, unverified
		}
	}
	p.subMu = make([]sync.Mutex, len(p.subs))
	p.subScan = make([]subScanState, len(p.subs))
	for _, t := range media {
		p.tracks = append(p.tracks, &planTrack{
			ft:         &fragTrack{outTrack: t, timescale: mediaTimescale(t)},
			firstPtsMs: -1,
		})
	}
	p.segEnd = c.SegmentEnd
	p.noteTrackCues(c.Cues, c.SegmentStart, segMs)

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
	synthesized := len(cues) == 0
	if synthesized {
		// Options.SynthesizeIndex: serve the unindexed (or misskeyed-index)
		// source anyway - walk the clusters once, structure only, and build
		// the video cue points in memory. The synthesized index replaces
		// nothing on disk; the plan just seeks through it.
		if !o.SynthesizeIndex {
			return nil, errf("%s: the Cues index no video keyframes - on-demand HLS seeks through them (run `mkvgo reindex` first, or plan with Options.SynthesizeIndex)", shown.Path(srcPath))
		}
		cues, err = synthesizeVideoCues(ctx, srcPath, fs, c, vidID, p.trackDurs)
		if err != nil {
			return nil, err
		}
		if len(cues) == 0 {
			return nil, errf("%s: no video keyframes found to synthesize an index from", shown.Path(srcPath))
		}
	}
	p.cueDensity = cueDensityOf(cues, synthesized)
	p.bounds = []int64{0}
	p.offsets = []int64{c.SegmentStart + cues[0].ClusterPos}
	last := int64(0)
	for _, cue := range cues {
		p.noteCluster(c.SegmentStart+cue.ClusterPos, cue.TimeMs)
		if cue.TimeMs >= last+segMs {
			p.bounds = append(p.bounds, cue.TimeMs)
			p.offsets = append(p.offsets, c.SegmentStart+cue.ClusterPos)
			last = cue.TimeMs
		}
	}
	p.segCount = len(p.bounds)
	p.medianSpan = medianSpanOf(p.offsets)
	p.spanCap = 8 * p.medianSpan

	// Bounded peeks: the first cluster fixes each track's first PTS and builds
	// the sample entries lazy codecs derive from their first frame; the last
	// cued cluster fixes each track's final PTS pair (exact init durations).
	// The two ends share nothing: over a remote source (one whose readers
	// take range hints, where each read is a round trip) the tail is read
	// while the head is.
	var (
		lastPts, prevPts, lastFrames []int64
		tailErr                      error
		tailDone                     = make(chan struct{})
	)
	peekTail := func() {
		defer close(tailDone)
		lastPts, prevPts, lastFrames, tailErr = p.peekTail(ctx, c.SegmentStart+cues[len(cues)-1].ClusterPos)
	}
	parallel := hintsRanges(fs, srcPath)
	if parallel {
		go peekTail()
	}
	headErr := p.peekHead(ctx)
	if headErr == nil {
		headErr = p.learnAheadHead(ctx)
	}
	if !parallel {
		peekTail()
	}
	<-tailDone
	if headErr != nil {
		return nil, headErr
	}
	if tailErr != nil {
		return nil, tailErr
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
		} else if ft.outTrack.mkv.DefaultDurationNs <= 0 {
			pt.gridTS = settleStride(pt.gridTS, c, ft.outTrack, ft.timescale, pt.firstPtsMs, lastPts[i], lastFrames[i])
			if pt.gridTS == gridSpread {
				p.stats.SpreadLacedTracks++
			}
		}
		switch {
		case pt.gridTS > 0:
			// Grid-timed audio: the final frame's index recovers its exact
			// slot from the ms timecode (fillFragTiming's grid rule).
			pt.lastDurTS = pt.gridTS
		case pt.gridTS == gridSpread:
			pt.lastDurTS = lastBlockSpan(scale(pt.laceSecondMs)-scale(pt.firstPtsMs), pt.laceFirstFrames, lastFrames[i]) // spreadLaced's final lace
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
	if p.segEnd > 0 && p.segEnd < srcSize {
		srcSize = p.segEnd // what follows the Segment is no segment's
	}
	for k := 0; k < p.segCount; k++ {
		end := srcSize
		if k+1 < p.segCount {
			end = p.offsets[k+1]
		}
		segs = append(segs, segInfo{durSec: p.durs[k], bytes: end - p.offsets[k]})
	}
	confirm, closeConfirm := p.foreignBlockChecker(ctx, srcPath, fs, segs)
	segs = boundSegmentSpans(segs, confirm)
	closeConfirm()
	if err := ctx.Err(); err != nil { // a span left unchecked must not reach the master
		return nil, err
	}
	p.segs = segs
	p.winBudget = o.WindowCacheBytes
	for _, pt := range p.tracks { // half the tagged bit rates: a lower bound until a walk measures
		if mk := &pt.ft.outTrack.mkv; !pt.ft.outTrack.spec.video && mk.Bitrate != nil {
			p.otherRate += float64(*mk.Bitrate) / 8000 / 2
		}
	}
	for _, s := range segs {
		if s.bytes > p.winPeak {
			p.winPeak = s.bytes
		}
	}
	fts := make([]*fragTrack, len(p.tracks))
	for i, pt := range p.tracks {
		fts[i] = pt.ft
	}
	// Options.AudioPresentationShift: re-base the shifted audio tracks'
	// edit-list offsets before the init segments are built. The fragment
	// decode times were derived above from the UNSHIFTED first PTS, exactly
	// like the full pass - the shift lives in the init alone (parity kept).
	// The video's composition shift, measured header-only from the opening frames:
	// the trun's offsets are pushed up by it (a fragmented file cannot carry a
	// negative one - see compositionShiftTS) and the init's edit list takes it back
	// out. It must be known BEFORE the init is built.
	if err := p.probeCompositionShift(ctx); err != nil {
		return nil, err
	}
	applyAudioPresentationShift(&o, fts)
	// Options.ChapterMarkers: video.ft.presentMs is derived exactly as the
	// full pass derives fts[primaryIndex(fts)].presentMs, so the same source
	// and option yield the same chapters (byte parity full-pass <-> plan).
	chapters := chapterMarkers(&o, c.Chapters, video.ft.presentMs)
	var est []iframeRef // the peak segment bounds the trick-play bitrate: each I-frame lies inside its segment, over the same duration
	if p.hasIframe {
		est = peakSegmentIframe(segs)
	}
	p.master = buildMasterPlaylist(&o, fts, p.subs, segs, est)
	p.stampSource()
	p.tabAcct = newTableAccount(p)
	if o.Encrypt == nil {
		p.mpd = buildDASHManifest(&o, fts, p.subs, p.durs, peakBandwidth(segs), chapters)
	}
	if err := ctx.Err(); err != nil { // the cover read does not see ctx
		return nil, err
	}
	cover, err := loadCoverArt(fs, c.Attachments)
	if err != nil {
		return nil, err
	}
	meta := movieMeta{title: c.Info.Title, tags: globalTags(c), cover: cover}
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

// Resources returns every resource name the plan serves - the HLS master, the
// DASH manifest (same CMAF segments, second manifest), then per rendition its
// media playlist, init segment and media segments (video first, then each
// audio track), and each subtitle rendition (its HLS playlist and its WebVTT).
// The I-frame fragments iframe.m3u8 names are listed beside it, a Matroska
// plan's once that playlist was built.
// Serving a presentation is: answer any of these names through Resource.
func (p *HLSPlan) Resources() []string {
	names := []string{"master.m3u8"}
	if p.iframe != nil || p.hasIframe {
		names = append(names, "iframe.m3u8")
		p.iframeMu.Lock()
		for _, ifr := range p.iframes { // a Matroska plan's, once iframe.m3u8 was built
			names = append(names, iframeName(ifr.seg))
		}
		p.iframeMu.Unlock()
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
			return nil, "", noResource("no DASH manifest for an AES-128-encrypted presentation (HLS only)")
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
	if strings.HasPrefix(name, "iframe") {
		data, err := p.iframeFragment(ctx, name)
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
			return nil, "", noResource("no subtitle rendition %d (have %d)", i, len(p.subs))
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
	return nil, "", noResource("unknown HLS resource %q (see Resources())", name)
}

// blockReaderAt opens a block walk at off, bounded to the Segment's end: what lies past it is no part of the media.
func (p *HLSPlan) blockReaderAt(src io.ReadSeeker, off int64) (*reader.BlockReader, error) {
	br, err := reader.NewBlockReaderAt(src, p.tcScale, off)
	if err == nil {
		br.SetSegmentEnd(p.segEnd)
	}
	return br, err
}

// blockReaderFrom is blockReaderAt opened on a block's position.
func (p *HLSPlan) blockReaderFrom(src io.ReadSeeker, pos reader.BlockPos) (*reader.BlockReader, error) {
	br, err := reader.NewBlockReaderFrom(src, p.tcScale, pos)
	if err == nil {
		br.SetSegmentEnd(p.segEnd)
	}
	return br, err
}
