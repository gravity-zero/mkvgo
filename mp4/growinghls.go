package mp4

// growinghls.go - PlanGrowingHLS: HLS over a Matroska file that is still being
// written (a download in progress), served as a live-style playlist that
// lengthens as more data lands, then finalizes to a plain VOD playlist once the
// source is complete. This is VOD-to-live, not live ingest: the source is a
// regular, growing file (e.g. a download landing on disk) - there is no
// chunked transfer and no LL-HLS, just a cursor that resumes over whatever new,
// WHOLE clusters have appeared since the last look.
//
// Cursor and the partial-cluster rule: unlike PlanHLS, a growing source rarely
// carries a Cues index yet, so segment boundaries cannot be read off it. The
// scan instead walks clusters from the front, cutting on video keyframes at
// Options.SegmentMs like PlanHLS does from Cues, but discovering the cuts
// itself. A cluster is "whole" only when its declared (known-size) body fits
// entirely within the file's current length; a cluster whose header claims N
// bytes with only M<N present is a PARTIAL TRAILING CLUSTER and is never
// scanned - the cursor simply stops before it and retries on the next Refresh.
// This is the only correctness-critical rule in this file: once a cluster is
// accepted as whole, every byte it contributes to a segment is guaranteed
// present, so re-deriving that segment later (once the file has grown further)
// reads the exact same bytes and produces the exact same output - segment k's
// byte range and content never change once published (stable numbering).
//
// Byte-identity: segment building is NOT reimplemented here. A GrowingHLSPlan
// keeps an internal *HLSPlan (the same type PlanHLS returns) whose bounds/
// offsets/tracks are extended in place as the cursor advances; Segment/Resource
// delegate straight to HLSPlan.Segment/segmentTrack, the exact function PlanHLS
// uses. Given the same bounds and offsets (which the cursor guarantees are
// byte-for-byte the same once the finished file is scanned by PlanHLS), the
// output is the same function called with the same inputs - byte-identical by
// construction, not by re-derivation.
//
// EVENT vs VOD: while growing, the media playlist carries
// #EXT-X-PLAYLIST-TYPE:EVENT (append-only: a player may keep polling and seek
// back into anything already listed) and NO #EXT-X-ENDLIST. This is
// deliberately NOT a sliding window (a live broadcast's #EXT-X-MEDIA-SEQUENCE
// trick that evicts old segments): a VOD-to-live download retains the whole
// presentation from the start, media sequence 0 forever, since nothing here is
// actually unbounded - the source file will finish. Once Complete() is called,
// or Refresh notices the source has finalized on its own (a Cues index now
// parses, or a known-size Segment element's declared end has been reached with
// its last cluster whole), the playlist switches to VOD and gets ENDLIST.
//
// Duration: mvhd/tkhd/mdhd/mehd cannot know the final duration while growing,
// so the init segment carries 0 (the standard "unknown duration" live-HLS
// convention; players time playback off each fragment's tfdt/trun, never off
// the init). Once finalized, the exact totals are derived the same way PlanHLS
// derives them (peekTail, applied here to the tail the cursor has not yet
// closed) and the init segment is rebuilt - from that point on it is
// byte-identical to a PlanHLS build of the same, now-finished file. Before
// finalization the init is necessarily NOT byte-identical to the finished
// file's init (the total duration is genuinely unknown until then); what IS
// stable from the first stage onward is everything the head alone determines -
// ftyp brands, stsd/sample entry, track handler/name/language.
//
// v1 limits (explicit, not silent): Matroska/WebM sources only (no MP4);
// requires a video track (matches PlanHLS's own Matroska path - an audio-only
// growing plan is refused with a clear error); no subtitle renditions, no I-
// frame trick-play playlist, no DASH manifest; Options.Encrypt/CENC refused;
// only known-size clusters are supported (an unknown-size/streamed cluster is
// reported as an explicit error, never silently mishandled).
//
// Concurrency: Refresh and Resource/Segment may run on different goroutines (a
// server polling Refresh while requests hit Resource). Every exported method
// takes the plan's mutex for its whole body, including the Segment/Resource
// I/O - simple and provably race-free, at the cost of serialising concurrent
// requests against each other and against Refresh. A future version could
// snapshot the immutable-once-published state (bounds/offsets never mutate in
// place, only grow) and release the lock before the read, like HLSPlan.Segment
// does; v1 keeps the simpler, safe form.

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// GrowingHLSPlan is the result of PlanGrowingHLS: an HLS presentation over a
// Matroska source that may still be growing. See the package doc above for the
// cursor/partial-cluster/byte-identity contract. Safe for concurrent use.
type GrowingHLSPlan struct {
	srcPath  string
	fs       *mkv.FS
	opts     Options
	segMs    int64
	meta     movieMeta
	segStart int64 // Container.SegmentStart

	// segDeclaredEnd is the Segment element's declared end offset (segStart +
	// its size), or -1 when the Segment size is unknown (the shape a file
	// still being written typically has: a size only becomes trustworthy once
	// the muxer finished and sealed it). One of the two auto-finalize
	// signals: the other is a non-Cluster trailing element (typically Cues).
	segDeclaredEnd int64

	mu sync.Mutex

	// full is reused verbatim for segment building: PlanHLS's own
	// tracks/bounds/offsets/segCount, extended in place as the cursor scans
	// further. HLSPlan.Segment/segmentTrack/videoIndex/peekTail run unmodified
	// against it, which is what makes a served segment byte-identical to
	// PlanHLS's - the same function, the same inputs.
	full *HLSPlan

	// head resolution: every selected track needs its first sample (sample
	// entry, first PTS) and, for a laced no-DefaultDuration audio track, its
	// grid stride, before any output can be built (mirrors HLSPlan.peekHead,
	// but incremental - extended cluster by cluster until resolved).
	headDone bool
	headNeed int
	// leadPts/leadSync are the video track's opening frames (decode order), from
	// which its composition shift is measured - the same prefix rule the full pass
	// and PlanHLS apply (compositionShiftTS), so the three build the same init. No
	// extra read: these blocks are the ones the growing scan already walks.
	leadPts      []int64
	leadSync     []bool
	leadDone     bool
	headNeedGrid int
	headProbes   map[*planTrack]*growingGridProbe

	// cursor: byte offset to resume the whole-cluster scan from. started
	// distinguishes "not yet found the first cluster" from offset 0 (which
	// never IS a cluster offset - the EBML+Segment headers precede it).
	started bool
	cursor  int64

	// boundary discovery (mirrors PlanHLS's Cues-driven bounds loop, driven by
	// scanned keyframes instead of a pre-built index). kfLast is the last
	// accepted boundary's ms, starting at 0 exactly like PlanHLS's "last".
	haveFirstKF bool
	kfLast      int64

	done     bool  // finalized: VOD + ENDLIST, every duration final
	lastSize int64 // file size as of the last scan (segInfo's tail estimate)

	// published is the number of segments GrowingHLSPlan actually serves:
	// len(p.full.bounds)-1 while growing (the last bounds entry is the START
	// of the still-open segment, held back), len(p.full.bounds) once done.
	// p.full.segCount is kept EQUAL to len(p.full.bounds) at all times instead
	// (not published) - HLSPlan.segmentTrack's own bounds[n+1] gate reads
	// "n+1 < segCount" to decide whether a next bound exists, so segCount must
	// track len(bounds) exactly (PlanHLS's own invariant) for that check to see
	// the already-known bounds entry that closes the last published segment;
	// published is the separate, smaller number GrowingHLSPlan exposes.
	published int

	master []byte
	inits  [][]byte
	medias [][]byte
}

// growingGridProbe recovers a laced, no-DefaultDuration audio track's per-frame
// stride from its first two distinct block timecodes and the first block's
// frame count - the same recovery HLSPlan.peekHead performs, held here across
// cluster scans until it resolves.
type growingGridProbe struct {
	firstTC, frames, secondTC int64
	haveFirst, haveSecond     bool
}

// PlanGrowingHLS opens path as a Matroska/WebM file that may still be growing
// (a download in progress) and returns a plan whose media playlist has no
// EXT-X-ENDLIST until Complete()/auto-detected finalization. The source must
// already carry its EBML+Segment head and a Tracks element (the downloader
// writes those before any player can usefully start); PlanGrowingHLS itself
// performs one scan of whatever whole clusters already exist, exactly like a
// first Refresh.
func PlanGrowingHLS(ctx context.Context, srcPath string, opts ...Options) (*GrowingHLSPlan, error) {
	o := optionsFrom(opts)
	fs := o.FS
	segMs := o.SegmentMs
	if segMs <= 0 {
		segMs = defaultSegmentMs
	}
	if o.Encrypt != nil || o.CENC != nil {
		return nil, errf("%s: a growing (still-downloading) HLS plan does not support Options.Encrypt/CENC in this version", srcPath)
	}

	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	var head [4]byte
	_, rerr := io.ReadFull(f, head[:])
	_ = f.Close()
	if rerr != nil {
		return nil, errf("%s: read head: %w", srcPath, rerr)
	}
	if head[0] != 0x1A || head[1] != 0x45 || head[2] != 0xDF || head[3] != 0xA3 {
		return nil, errf("%s: play-while-downloading is Matroska/WebM-only in this version", srcPath)
	}

	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs, reader.WithTags(), reader.WithAttachments())
	if err != nil {
		return nil, err
	}
	if len(c.Tracks) == 0 {
		return nil, errf("%s: no Tracks element yet (the downloader must write the head before play-while-downloading can start)", srcPath)
	}

	planned, _, err := planTracks(c, o)
	if err != nil {
		return nil, err
	}
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
		return nil, errf("%s: play-while-downloading requires a video track (an audio-only growing plan is not supported in this version, matching PlanHLS)", srcPath)
	}

	p := &GrowingHLSPlan{
		srcPath: srcPath, fs: fs, opts: o, segMs: segMs,
		segStart: c.SegmentStart, segDeclaredEnd: -1,
		meta: movieMeta{title: c.Info.Title, tags: globalTags(c), cover: pickCoverArt(c.Attachments)},
	}
	p.full = &HLSPlan{srcPath: srcPath, fs: fs, tcScale: c.Info.TimecodeScale, opts: o,
		trackDurs: reader.TrackDefaultDurations(c.Tracks)}
	for _, t := range media {
		p.full.tracks = append(p.full.tracks, &planTrack{
			ft:         &fragTrack{outTrack: t, timescale: mediaTimescale(t)},
			firstPtsMs: -1,
		})
	}
	p.headNeed = len(p.full.tracks)
	p.headProbes = map[*planTrack]*growingGridProbe{}
	for _, pt := range p.full.tracks {
		mk := &pt.ft.outTrack.mkv
		if mk.Type == mkv.AudioTrack && mk.DefaultDurationNs <= 0 {
			p.headProbes[pt] = &growingGridProbe{}
			p.headNeedGrid++
		}
	}

	if declEnd, derr := segmentDeclaredEnd(fs, srcPath); derr == nil {
		p.segDeclaredEnd = declEnd
	}

	if _, err := p.Refresh(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Refresh re-stats the file, scans any whole new clusters that appeared since
// the last call, and extends the segment list. It returns the number of newly
// published (closed) segments - 0, not an error, when nothing new has landed.
func (p *GrowingHLSPlan) Refresh(ctx context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return 0, nil
	}
	st, err := p.fs.DoStat(p.srcPath)
	if err != nil {
		return 0, err
	}
	return p.scanLocked(ctx, st.Size())
}

// Complete marks the source finished: the plan scans whatever whole clusters
// remain (like a last Refresh), then closes the final segment using the
// current end of file as its true end regardless of whether the source itself
// ever appends a Cues/trailer element. Complete has no context parameter (a
// fixed part of this API): the scan it performs is a small, bounded, one-shot
// read over data that by the caller's own assertion will not grow further, so
// context.Background() is used internally rather than threading one through.
func (p *GrowingHLSPlan) Complete() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return
	}
	ctx := context.Background()
	if st, err := p.fs.DoStat(p.srcPath); err == nil {
		_, _ = p.scanLocked(ctx, st.Size())
	}
	if !p.done {
		_ = p.finalizeLocked(ctx)
	}
}

// scanLocked resumes the cluster cursor over [cursor, size), folding every
// newly whole cluster into head resolution and boundary discovery, then
// rebuilds the served outputs. Caller holds p.mu.
func (p *GrowingHLSPlan) scanLocked(ctx context.Context, size int64) (int, error) {
	p.lastSize = size
	before := p.published

	src, err := p.fs.DoOpen(p.srcPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	if !p.started {
		off, found, ferr := p.firstClusterOffset(src)
		if ferr != nil {
			return 0, ferr
		}
		if !found {
			return 0, nil // no cluster yet; try again on the next Refresh
		}
		p.cursor = off
		p.started = true
	}

	for !p.done {
		if cerr := ctx.Err(); cerr != nil {
			return p.published - before, cerr
		}
		if _, serr := src.Seek(p.cursor, io.SeekStart); serr != nil {
			return p.published - before, serr
		}
		h, n, herr := ebml.ReadElementHeader(src)
		if herr != nil {
			break // header not (fully) present yet: try again on the next Refresh
		}
		if h.ID != mkv.IDCluster {
			// A non-Cluster top-level element after the last cluster (typically
			// Cues, written once the source finalizes): auto-detected completion.
			if ferr := p.finalizeLocked(ctx); ferr != nil {
				return p.published - before, ferr
			}
			break
		}
		if h.Size < 0 {
			return p.published - before, errf("%s: growing plan requires known-size Matroska clusters (unsupported unknown-size cluster at byte %d)", p.srcPath, p.cursor)
		}
		bodyEnd := p.cursor + int64(n) + h.Size
		if bodyEnd > size {
			break // partial trailing cluster: not yet whole, stop before it
		}
		if serr := p.scanClusterLocked(ctx, src, p.cursor, bodyEnd); serr != nil {
			return p.published - before, serr
		}
		p.cursor = bodyEnd
	}

	if !p.done && p.segDeclaredEnd >= 0 && size >= p.segDeclaredEnd && p.cursor >= p.segDeclaredEnd {
		if ferr := p.finalizeLocked(ctx); ferr != nil {
			return p.published - before, ferr
		}
	}

	if p.headDone && p.leadDone {
		p.rebuildOutputsLocked()
	}
	return p.published - before, nil
}

// scanClusterLocked walks one confirmed-whole cluster's blocks (bounded to its
// declared byte range via boundedReadSeeker, so a concurrently-growing file
// past that point is never touched), folding them into head resolution (while
// unresolved) and video-keyframe boundary discovery. Caller holds p.mu.
func (p *GrowingHLSPlan) scanClusterLocked(ctx context.Context, src mkv.ReadSeekCloser, clusterStart, bodyEnd int64) error {
	bounded := &boundedReadSeeker{r: src, limit: bodyEnd}
	br, err := reader.NewBlockReaderAt(bounded, p.full.tcScale, clusterStart)
	if err != nil {
		return err
	}
	br.SetTrackDefaultDurations(p.full.trackDurs)
	routing := make(map[uint64]*planTrack, len(p.full.tracks))
	ids := make([]uint64, 0, len(p.full.tracks))
	for _, pt := range p.full.tracks {
		routing[pt.ft.outTrack.mkv.ID] = pt
		ids = append(ids, pt.ft.outTrack.mkv.ID)
	}
	br.KeepTracks(ids...)

	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		b, berr := br.Next()
		if isBlockWalkEnd(berr) {
			break
		}
		if berr != nil {
			return errf("read block: %w", berr)
		}
		pt, ok := routing[b.TrackNumber]
		if !ok {
			continue
		}
		if !p.headDone {
			if herr := p.extendHeadLocked(pt, b); herr != nil {
				return herr
			}
		}
		if pt.ft.outTrack.spec.video {
			p.extendLeadLocked(b)
			if b.Keyframe {
				p.recordKeyframeLocked(b.BlockTimecode, clusterStart)
			}
		}
	}
	if !p.headDone && p.headNeed == 0 && p.headNeedGrid == 0 {
		p.resolveHeadLocked()
	}
	p.updateSegCountLocked()
	return nil
}

// extendLeadLocked folds one video block into the composition-shift prefix: the
// opening frames, up to the second keyframe (the first GOP shows the whole reorder
// pattern) or compositionPrefix, whichever comes first. The shift is settled before
// a single output is published, because the edit list it produces lives in the INIT
// - a byte a player fetches once and caches, and which therefore may never change
// under it mid-session.
func (p *GrowingHLSPlan) extendLeadLocked(b mkv.Block) {
	if p.leadDone {
		return
	}
	if (len(p.leadPts) > 0 && b.Keyframe) || len(p.leadPts) >= compositionPrefix {
		p.settleLeadLocked()
		return
	}
	p.leadPts = append(p.leadPts, b.Timecode)
	p.leadSync = append(p.leadSync, b.Keyframe)
}

// settleLeadLocked computes the video track's composition shift from the prefix
// gathered so far and freezes it. Called when the prefix closes, and again when the
// source is declared complete - a source shorter than one GOP still gets its shift,
// measured over every frame it has, exactly as the full pass measures it.
func (p *GrowingHLSPlan) settleLeadLocked() {
	if p.leadDone {
		return
	}
	p.leadDone = true
	if len(p.full.tracks) == 0 {
		return
	}
	pt := p.full.tracks[p.full.videoIndex()]
	scale := tsScale(pt.ft.timescale)
	ts := make([]int64, len(p.leadPts))
	for i, ms := range p.leadPts {
		ts[i] = scale(ms)
	}
	pt.ft.ctsShiftTS = compositionShiftTS(ts, p.leadSync)
}

// extendHeadLocked folds one block into head resolution: the track's first
// sample (building its sample entry when derived from the first frame) and,
// for a laced no-DefaultDuration audio track, the grid probe. Mirrors
// HLSPlan.peekHead's per-block logic exactly, applied incrementally.
func (p *GrowingHLSPlan) extendHeadLocked(pt *planTrack, b mkv.Block) error {
	if pt.firstPtsMs < 0 {
		data := pt.ft.outTrack.mkv.RestoreHeader(b.Data)
		if pt.ft.outTrack.sampleEntry == nil {
			entry, err := pt.ft.outTrack.spec.sampleEntry(&pt.ft.outTrack.mkv, data)
			if err != nil {
				return err
			}
			pt.ft.outTrack.sampleEntry = entry
		}
		pt.firstPtsMs = b.Timecode
		p.headNeed--
	}
	if pr := p.headProbes[pt]; pr != nil && !pr.haveSecond {
		switch {
		case !pr.haveFirst:
			pr.firstTC, pr.frames, pr.haveFirst = b.BlockTimecode, 1, true
		case b.BlockTimecode == pr.firstTC:
			pr.frames++
		default:
			pr.secondTC, pr.haveSecond = b.BlockTimecode, true
			p.headNeedGrid--
		}
	}
	return nil
}

// resolveHeadLocked finalizes every pending grid probe (deriveGridTS, exactly
// as HLSPlan.peekHead does) once every track has its first sample and every
// no-DefaultDuration audio track has resolved its stride.
func (p *GrowingHLSPlan) resolveHeadLocked() {
	for pt, pr := range p.headProbes {
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
	p.headDone = true
}

// recordKeyframeLocked folds one video keyframe into boundary discovery,
// mirroring PlanHLS's Cues-driven bounds loop verbatim: bounds[0] is always 0
// (anchored at the first keyframe's cluster, regardless of its real ms), and
// every keyframe after it (the first one included, exactly like PlanHLS
// re-testing cues[0] against last=0) that reaches segMs past the last accepted
// boundary opens a new one.
func (p *GrowingHLSPlan) recordKeyframeLocked(ms, clusterOffset int64) {
	if !p.haveFirstKF {
		p.full.bounds = []int64{0}
		p.full.offsets = []int64{clusterOffset}
		p.haveFirstKF = true
		p.kfLast = 0
	}
	if ms >= p.kfLast+p.segMs {
		p.full.bounds = append(p.full.bounds, ms)
		p.full.offsets = append(p.full.offsets, clusterOffset)
		p.kfLast = ms
	}
}

// updateSegCountLocked is the single place p.full.segCount and p.published
// are derived. p.full.segCount always equals len(bounds) once the head has
// resolved (0 before that) - see the published field's doc for why it must
// NOT be reduced by the still-open segment the way p.published is.
func (p *GrowingHLSPlan) updateSegCountLocked() {
	if !p.headDone {
		p.full.segCount = 0
		p.published = 0
		return
	}
	p.full.segCount = len(p.full.bounds)
	if p.done {
		p.published = len(p.full.bounds)
		return
	}
	n := len(p.full.bounds) - 1
	if n < 0 {
		n = 0
	}
	p.published = n
}

// finalizeLocked closes the plan: the still-open final segment is given a real
// end and every track's exact total duration is derived from a tail peek
// (HLSPlan.peekTail, applied to the byte range the cursor has not yet closed),
// exactly reproducing PlanHLS's own post-Cues duration derivation. Caller
// holds p.mu.
func (p *GrowingHLSPlan) finalizeLocked(ctx context.Context) error {
	if p.done {
		return nil
	}
	// The source will not grow again: a prefix still open (a file shorter than one
	// GOP) is settled on what it has, so the shift - and the init's edit list - is
	// never left unmeasured.
	p.settleLeadLocked()
	if !p.headDone || !p.haveFirstKF {
		// Degenerate source (no video keyframe ever scanned, or the head never
		// resolved): nothing to close into a real presentation.
		p.done = true
		p.updateSegCountLocked()
		if p.headDone {
			p.rebuildOutputsLocked()
		}
		return nil
	}

	tailOff := p.full.offsets[len(p.full.offsets)-1]
	lastPts, prevPts, lastFrames, err := p.full.peekTail(ctx, tailOff)
	if err != nil {
		return err
	}

	var video *planTrack
	for i, pt := range p.full.tracks {
		ft := pt.ft
		scale := tsScale(ft.timescale)
		switch {
		case pt.gridTS > 0:
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

	vi := p.full.videoIndex()
	endMs := lastPts[vi] + video.lastDurTS*int64(movieTimescale)/int64(video.ft.timescale)
	p.done = true
	p.updateSegCountLocked()
	p.full.durs = make([]float64, p.full.segCount)
	for k := 0; k < p.full.segCount; k++ {
		end := endMs
		if k+1 < p.full.segCount {
			end = p.full.bounds[k+1]
		}
		p.full.durs[k] = float64(end-p.full.bounds[k]) / 1000
	}
	p.rebuildOutputsLocked()
	return nil
}

// rebuildOutputsLocked rebuilds the master/init/media outputs from the current
// bounds/offsets/segCount. Called whenever they change; only ever called once
// the head has resolved (every track has a sample entry). Caller holds p.mu.
func (p *GrowingHLSPlan) rebuildOutputsLocked() {
	// published, not p.full.segCount: the latter equals len(bounds) even while
	// growing (HLSPlan.segmentTrack's own invariant - see the published field's
	// doc), one MORE than the number of segments actually servable.
	segCount := p.published
	fts := p.full.fts()

	var durs []float64
	if p.done {
		durs = p.full.durs
	} else {
		durs = make([]float64, segCount)
		for k := 0; k < segCount; k++ {
			durs[k] = float64(p.full.bounds[k+1]-p.full.bounds[k]) / 1000
		}
		p.full.durs = durs
	}

	segs := make([]segInfo, segCount)
	for k := 0; k < segCount; k++ {
		end := p.lastSize
		if k+1 < len(p.full.offsets) {
			end = p.full.offsets[k+1]
		}
		segs[k] = segInfo{durSec: durs[k], bytes: end - p.full.offsets[k]}
	}

	p.master = buildMasterPlaylist(&p.opts, fts, nil, segs, nil)
	vi := p.full.videoIndex()
	inits := make([][]byte, len(fts))
	medias := make([][]byte, len(fts))
	for i, ft := range fts {
		m := movieMeta{}
		if i == vi {
			m = p.meta
		}
		inits[i] = buildInitSegment([]*fragTrack{ft}, m, p.opts.CENC)
		ii := i
		medias[ii] = buildGrowingMediaPlaylist(&p.opts, durs, renditionInit(fts, ii),
			func(k int) string { return renditionSegment(fts, ii, k) }, p.done)
	}
	p.inits = inits
	p.medias = medias
}

// NumSegments returns the number of media segments currently published - the
// closed ones only; the segment still accumulating past the last boundary is
// not counted until it closes (a new boundary is found, or the plan
// finalizes).
func (p *GrowingHLSPlan) NumSegments() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.published
}

// MasterPlaylist returns master.m3u8, or nil before the head has resolved.
func (p *GrowingHLSPlan) MasterPlaylist() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.master
}

// MediaPlaylist returns the video rendition's playlist: EVENT-typed with no
// ENDLIST while growing, VOD+ENDLIST once finalized. nil before the head has
// resolved.
func (p *GrowingHLSPlan) MediaPlaylist() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.medias) == 0 {
		return nil
	}
	return p.medias[p.full.videoIndex()]
}

// InitSegment returns the video rendition's init segment. Its duration fields
// read 0 (unknown) while growing and the exact totals once finalized - see
// the package doc's byte-identity note. nil before the head has resolved.
func (p *GrowingHLSPlan) InitSegment() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.inits) == 0 {
		return nil
	}
	return p.inits[p.full.videoIndex()]
}

// Resources returns every resource name the plan currently serves - grows as
// segments are published, exactly like HLSPlan.Resources but without the
// subtitle/DASH/trick-play names this version does not carry.
func (p *GrowingHLSPlan) Resources() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := []string{"master.m3u8"}
	fts := p.full.fts()
	for i := range fts {
		names = append(names, renditionPlaylist(fts, i), renditionInit(fts, i))
		for n := 0; n < p.published; n++ {
			names = append(names, renditionSegment(fts, i, n))
		}
	}
	return names
}

// Segment builds the n-th (0-based) video media segment, byte-identical to
// the same segment RemuxToHLS/PlanHLS would produce for the finished file
// (HLSPlan.Segment does the actual work - see the package doc).
func (p *GrowingHLSPlan) Segment(ctx context.Context, n int) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 || n >= p.published {
		return nil, errf("segment %d out of range (0..%d)", n, p.published-1)
	}
	return p.full.Segment(ctx, n)
}

// Resource builds the named resource and returns its bytes and Content-Type,
// mirroring HLSPlan.Resource for the names this version serves (master, per-
// rendition playlist/init/segments - no subtitles, no DASH, no trick-play).
func (p *GrowingHLSPlan) Resource(ctx context.Context, name string) ([]byte, string, error) {
	const (
		mimeM3U8 = "application/vnd.apple.mpegurl"
		mimeMP4  = "video/mp4"
		mimeSeg  = "video/iso.segment"
	)
	p.mu.Lock()
	defer p.mu.Unlock()

	if name == "master.m3u8" {
		if p.master == nil {
			return nil, "", errf("%s: no data yet (call Refresh)", p.srcPath)
		}
		return p.master, mimeM3U8, nil
	}
	fts := p.full.fts()
	for i := range fts {
		switch name {
		case renditionPlaylist(fts, i):
			if i >= len(p.medias) {
				return nil, "", errf("%s: no data yet (call Refresh)", p.srcPath)
			}
			return p.medias[i], mimeM3U8, nil
		case renditionInit(fts, i):
			if i >= len(p.inits) {
				return nil, "", errf("%s: no data yet (call Refresh)", p.srcPath)
			}
			return p.inits[i], mimeMP4, nil
		}
	}
	var n int
	if _, err := fmt.Sscanf(name, "seg%d.m4s", &n); err == nil && name == fmt.Sprintf("seg%05d.m4s", n) {
		if n < 1 || n > p.published {
			return nil, "", errf("segment %d out of range (0..%d)", n-1, p.published-1)
		}
		data, err := p.full.segmentTrack(ctx, p.full.videoIndex(), n-1)
		return data, mimeSeg, err
	}
	var a int
	if _, err := fmt.Sscanf(name, "seg_a%d_%d.m4s", &a, &n); err == nil && a >= 1 {
		for i := range fts {
			if name == renditionSegment(fts, i, n-1) {
				if n < 1 || n > p.published {
					return nil, "", errf("segment %d out of range (0..%d)", n-1, p.published-1)
				}
				data, err := p.full.segmentTrack(ctx, i, n-1)
				return data, mimeSeg, err
			}
		}
	}
	return nil, "", errf("unknown HLS resource %q (see Resources())", name)
}

// buildGrowingMediaPlaylist renders a media playlist EVENT-typed with no
// ENDLIST while growing, VOD+ENDLIST once done - otherwise identical to
// buildMediaPlaylist (no EXT-X-KEY line: Encrypt/CENC are refused for a
// growing plan).
func buildGrowingMediaPlaylist(o *Options, durs []float64, mapURI string, segName func(i int) string, done bool) []byte {
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
	if done {
		b = append(b, "#EXT-X-PLAYLIST-TYPE:VOD\n"...)
	} else {
		// EVENT, not a sliding live window: this is VOD-to-live (the source
		// will finish), so the whole presentation is retained from the start
		// (media sequence 0 forever) - see the package doc.
		b = append(b, "#EXT-X-PLAYLIST-TYPE:EVENT\n"...)
	}
	if mapURI != "" {
		b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q\n", rw(mapURI))...)
	}
	for i, d := range durs {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n%s\n", d, rw(segName(i)))...)
	}
	if done {
		b = append(b, "#EXT-X-ENDLIST\n"...)
	}
	return b
}

// firstClusterOffset walks the segment-level elements from p.segStart, skipping
// everything (Void/SeekHead/Info/Tracks/Tags/…) until the first Cluster header,
// and returns its absolute offset. found is false (not an error) when the walk
// runs out of data before reaching one - the normal state of a download that
// has only written its head so far.
func (p *GrowingHLSPlan) firstClusterOffset(src io.ReadSeeker) (int64, bool, error) {
	pos := p.segStart
	for {
		if _, err := src.Seek(pos, io.SeekStart); err != nil {
			return 0, false, err
		}
		h, n, err := ebml.ReadElementHeader(src)
		if err != nil {
			if isBlockWalkEnd(err) {
				return 0, false, nil
			}
			return 0, false, err
		}
		if h.ID == mkv.IDCluster {
			return pos, true, nil
		}
		if h.Size < 0 {
			return 0, false, errf("%s: unknown-size element before the first cluster (unsupported for a growing plan)", p.srcPath)
		}
		pos += int64(n) + h.Size
	}
}

// segmentDeclaredEnd reads the EBML+Segment headers and returns the Segment
// element's declared end offset, or -1 when its size is unknown - the shape
// a file still being written typically has (a finished muxer seals the size;
// an in-progress download of one carries the final value early, which is why
// reaching the declared end is an auto-finalize signal).
func segmentDeclaredEnd(fs *mkv.FS, path string) (int64, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	h1, _, err := ebml.ReadElementHeader(f) // EBML header
	if err != nil {
		return -1, err
	}
	if h1.Size < 0 {
		return -1, errf("%s: unknown-size EBML header", path)
	}
	if _, err := f.Seek(h1.Size, io.SeekCurrent); err != nil {
		return -1, err
	}
	h2, _, err := ebml.ReadElementHeader(f) // Segment header
	if err != nil {
		return -1, err
	}
	if h2.ID != mkv.IDSegment {
		return -1, errf("%s: expected a Segment element", path)
	}
	if h2.Size < 0 {
		return -1, nil
	}
	segStart, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	return segStart + h2.Size, nil
}

// boundedReadSeeker presents r truncated at limit: Read reports io.EOF at or
// past limit instead of reading further, so a cluster confirmed whole up to a
// known byte offset can be walked without ever touching bytes past it - a
// concurrently-growing file's later, unconfirmed bytes are never read, whether
// or not they have arrived yet. Seek passes through unchanged (countingReader
// only ever seeks to absolute positions within the already-validated range).
type boundedReadSeeker struct {
	r     io.ReadSeeker
	limit int64
}

func (b *boundedReadSeeker) Read(p []byte) (int, error) {
	cur, err := b.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if cur >= b.limit {
		return 0, io.EOF
	}
	if cur+int64(len(p)) > b.limit {
		p = p[:b.limit-cur]
	}
	return b.r.Read(p)
}

func (b *boundedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return b.r.Seek(offset, whence)
}
