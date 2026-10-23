package mp4

import (
	"bufio"
	"context"
	"io"
	"sync"

	"github.com/gravity-zero/mkvgo/mkv"
)

// sniffMP4ForPlan opens srcPath as an MP4 packaging source when it is one,
// nil when it is Matroska (the caller then takes the Cues path).
func sniffMP4ForPlan(ctx context.Context, srcPath string, fs *mkv.FS, moovLimit int64) (*packagingSource, error) {
	f, err := fs.DoOpenContext(ctx, srcPath)
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
	mv, err := parseMP4Limit(ctxReadSeeker{f, ctx}, st.Size(), sampleFull, moovLimit)
	if cerr := ctx.Err(); cerr != nil {
		f.Close()
		return nil, cerr
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return newPackagingSource(f, mv, st.Size()), nil
}

// newPackagingSource wraps a parsed MP4 for the packager. The track description
// is completed from the first sample where the sample entry left it short
// (colour behind a bare hvcC, HDR10 static metadata kept in-band only), so the
// init written carries the same colr/mdcv/clli a Matroska source would yield.
func newPackagingSource(f mkv.ReadSeekCloser, mv *movie, size int64) *packagingSource {
	c := containerFromMovie(mv)
	fillInBandColour(f, mv, c)
	return &packagingSource{c: c, mv: mv, src: f, size: size}
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
	if err := cencPreflight(o, media); err != nil {
		return nil, err
	}

	p := &HLSPlan{srcPath: srcPath, fs: fs, opts: *o, mp4src: true}
	if !o.VideoOnly {
		p.subs = filterSubTracks(planSubTracks(c, *o), keep)
	}
	p.subMu = make([]sync.Mutex, len(p.subs))
	p.subScan = make([]subScanState, len(p.subs))
	p.mp4subs = mp4SubTables(ps, p.subs)

	fts, offs, err := mp4PlanSamples(ctx, ps, media)
	if err != nil {
		return nil, err
	}
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
	starts := make([][]int32, len(fts)) // per track, per segment: the window's first sample
	segs := make([]segInfo, 0, p.segCount)
	var iframes []iframeRef
	p.durs = make([]float64, p.segCount)
	for k := 0; k < p.segCount; k++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		segStart := p.bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < p.segCount {
			segEnd = p.bounds[k+1]
		}
		var segBytes, audioBytes int64
		for i, ft := range fts {
			starts[i] = append(starts[i], int32(cursors[i]))
			seg := segmentWindow(ft, &cursors[i], segStart, segEnd)
			head := segmentFileLen(seg)
			if ft == video && !o.NoIFrames && len(seg.samples) > 0 && seg.samples[0].sync {
				iframes = append(iframes, iframeRef{seg: k, length: segmentFileLen(iframeSegment(seg)) + int64(seg.samples[0].size)})
			}
			segBytes += head + seg.dataLen
			if !ft.outTrack.spec.video {
				audioBytes += head + seg.dataLen
			}
		}
		p.durs[k] = float64(segEndOrLast(p.bounds, k, fts)-segStart) / 1000
		segs = append(segs, segInfo{durSec: p.durs[k], bytes: segBytes, audioBytes: audioBytes})
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
	// Everything above worked on the full sample arrays; the plan itself keeps
	// only the compact tables (it is cached for as long as a viewer may come
	// back), plus the audio bandwidth the arrays measured, for a master built
	// from this plan later.
	p.mp4tabs = make([]*mp4PlanTable, len(fts))
	for i, ft := range fts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mp4tabs[i] = newMP4PlanTable(ft.samples, offs[i], starts[i])
		ft.sampleBps, ft.sampleBpsKnown = sampleBandwidth(ft.samples)
		ft.samples = nil
	}
	p.stampSource()
	return p, nil
}

// mp4SegmentTrack builds one rendition segment from the sample-table plan:
// the window comes out of the compact table with the values the full pass
// computed (its cursor semantics: each window starts where the previous one
// stopped, at the first sample in decode order at/past its boundary) and the
// bytes are ranged reads at the recorded offsets.
func (p *HLSPlan) mp4SegmentTrack(ctx context.Context, ti, n int) ([]byte, error) {
	ft := p.tracks[ti].ft
	seg, offs := p.mp4Segment(ti, n)
	samples := seg.samples

	// The samples go straight into the buffer that is served: the head does
	// not depend on their bytes, so it is built first and the media read in
	// behind it. Only CENC needs the plaintext apart, to derive the auxiliary
	// data the head carries.
	read := func(dst []byte) error {
		if len(samples) == 0 {
			return nil
		}
		src, err := p.openSource(ctx)
		if err != nil {
			return err
		}
		defer src.Close()
		return readSampleRuns(ctx, countedSource{src, &p.copyRead}, samples, offs, dst)
	}

	if err := segmentFits(seg.dataLen); err != nil {
		return nil, err
	}
	var out []byte
	if p.opts.CENC != nil {
		plain := make([]byte, seg.dataLen)
		if err := read(plain); err != nil {
			return nil, err
		}
		// Same requirement as the full pass and the Matroska on-demand path:
		// CENC needs the plaintext bytes before the head is built.
		td, cipher, err := prepareCENCSegment(p.opts.CENC, ft.outTrack.spec.video, ft.outTrack.mkv.Codec, ft.outTrack.mkv.CodecPrivate, ft.outTrack.mkv.ID, seg.samples, plain)
		if err != nil {
			return nil, err
		}
		seg.cenc = td
		head := buildSegmentFile(uint32(n+1), seg)
		out = make([]byte, 0, int64(len(head))+int64(len(cipher)))
		out = append(out, head...)
		out = append(out, cipher...)
	} else {
		head := buildSegmentFile(uint32(n+1), seg)
		out = make([]byte, int64(len(head))+seg.dataLen)
		copy(out, head)
		if err := read(out[len(head):]); err != nil {
			return nil, err
		}
	}

	if p.opts.Encrypt != nil {
		return p.opts.Encrypt.encryptSegment(out, uint32(n))
	}
	return out, nil
}

// mp4Segment is track ti's segment n of an MP4 source, timed from the sample table, and each sample's file offset.
func (p *HLSPlan) mp4Segment(ti, n int) (trackSegment, []int64) {
	ft := p.tracks[ti].ft
	samples, offs := p.mp4tabs[ti].window(n)
	seg := trackSegment{trackID: ft.outTrack.mp4ID, baseDecodeTS: ft.durMediaTS}
	if len(samples) > 0 {
		seg.samples = samples
		seg.baseDecodeTS = samples[0].dtsTS
		seg.hasCTS = windowHasCTS(seg.samples)
		for x := range samples {
			seg.dataLen += int64(samples[x].size)
		}
	}
	return seg, offs
}

// Reading a segment's samples out of an MP4 source. The source stores some
// samples back to back and others apart - another track's samples lie between
// them - and says where each one is.
const (
	// sampleGapReadThrough is the largest gap between two runs of samples that
	// is read through instead of seeked over: the trade the Matroska reader
	// makes (seekSkipMin). A gap is read through only when it is also no
	// longer than the run behind it - see readSampleRuns.
	sampleGapReadThrough = 64 << 10
	// sampleSpanBuf is the read-ahead over such a stream.
	sampleSpanBuf = 256 << 10
)

// readSampleRuns reads the samples' bytes into dst, which holds exactly their
// total size. A run of samples stored back to back is one read, straight into
// dst. Runs are read as one buffered stream, through the gap between them,
// when that gap is small AND no longer than the run it leads to: the stream
// then reads at most twice the bytes it serves. That is the video of a source
// interleaved sample by sample (a 28 KiB frame after a 1.5 KiB audio sample).
// The audio of the same source is the opposite case - 1.5 KiB of it behind
// 28 KiB of video - and is read run by run: small forward reads, which a
// disk's read-ahead serves, rather than every video byte in between (6 times
// the audio, measured, on a disk that is the bottleneck).
func readSampleRuns(ctx context.Context, src io.ReadSeeker, samples []fragSample, offs []int64, dst []byte) error {
	// run returns the run of back-to-back samples starting at sample x: its
	// file offset, its length, and the sample after it.
	run := func(x int) (off, n int64, next int) {
		off, n = offs[x], int64(samples[x].size)
		for x++; x < len(samples) && offs[x] == off+n; x++ {
			n += int64(samples[x].size)
		}
		return off, n, x
	}
	near := func(end int64, x int) bool {
		if x >= len(samples) || offs[x] < end {
			return false
		}
		_, n, _ := run(x)
		return offs[x]-end <= min(sampleGapReadThrough, n)
	}
	fail := func(err error) error {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return errf("read sample: %w", err)
	}

	var buffered *bufio.Reader
	at := int64(0)
	for x := 0; x < len(samples); {
		if err := ctx.Err(); err != nil {
			return err
		}
		off, n, next := run(x)
		if _, err := src.Seek(off, io.SeekStart); err != nil {
			return fail(err)
		}
		if !near(off+n, next) {
			if _, err := io.ReadFull(src, dst[at:at+n]); err != nil {
				return fail(err)
			}
			at, x = at+n, next
			continue
		}
		// A span of runs with small gaps: where it ends, then one stream over
		// it.
		end := off + n
		for y := next; near(end, y); {
			o, m, z := run(y)
			end, y = o+m, z
		}
		limited := &io.LimitedReader{R: src, N: end - off}
		if buffered == nil {
			buffered = bufio.NewReaderSize(limited, sampleSpanBuf)
		} else {
			buffered.Reset(limited)
		}
		for {
			if _, err := io.ReadFull(buffered, dst[at:at+n]); err != nil {
				return fail(err)
			}
			at, x = at+n, next
			pos := off + n
			if pos == end {
				break
			}
			off, n, next = run(x)
			if _, err := buffered.Discard(int(off - pos)); err != nil {
				return fail(err)
			}
		}
	}
	return nil
}
