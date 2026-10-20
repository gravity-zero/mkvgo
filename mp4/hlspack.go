package mp4

// hlspack.go - MP4/MOV sources for the CMAF packager. The packaging pipeline
// (RemuxToHLS / RemuxToABR / PlanHLS) is source-agnostic past its collect
// phase; this file supplies the MP4 side: the source is sniffed from its
// first bytes, its moov gives the complete sample table (head-only - offsets,
// sizes, sync flags, timing), and samples are read straight from the mdat.
// An MP4 source therefore needs no Cues equivalent: the sample table IS the
// index, which makes the on-demand plan exact by construction.

import (
	"context"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// packagingSource is an opened packaging input: Matroska (c only) or MP4
// (c + mv + the open reader for sample reads).
type packagingSource struct {
	c    *mkv.Container
	mv   *movie
	src  mkv.ReadSeekCloser // open MP4 reader; nil for Matroska
	size int64
}

func (ps *packagingSource) Close() {
	if ps.src != nil {
		ps.src.Close()
	}
}

// openPackagingSource sniffs and opens srcPath for packaging.
func openPackagingSource(ctx context.Context, srcPath string, fs *mkv.FS, moovLimit int64) (*packagingSource, error) {
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		f.Close()
		return nil, errf("read head: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		f.Close() // Matroska: the existing reader owns its own handle
		// Attachment payloads stay on disk: the packager carries only the
		// cover art, read on its own by loadCoverArt.
		c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData(), reader.WithInBandColourFallback())
		if err != nil {
			return nil, err
		}
		return &packagingSource{c: c}, nil
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	mv, err := parseMP4Limit(f, st.Size(), sampleFull, moovLimit)
	if err != nil {
		f.Close()
		return nil, err
	}
	return newPackagingSource(f, mv, st.Size()), nil
}

// collectFromMP4 is the MP4 counterpart of collectFragSamples: every media
// sample's bytes go to its track's temp file (read in rough file order via
// the DTS merge) and subtitle samples become WebVTT cues.
func collectFromMP4(ctx context.Context, ps *packagingSource,
	routing map[uint64]*fragTrack, subs []hlsSubTrack, progress mkv.ProgressFunc) error {
	subRouting := make(map[uint64]*hlsSubTrack, len(subs))
	for i := range subs {
		subRouting[subs[i].track.ID] = &subs[i]
	}
	var processed int64
	for _, ref := range mergeByDTS(ps.mv) {
		if err := ctx.Err(); err != nil {
			return err
		}
		tk := &ps.mv.tracks[ref.track]
		s := &tk.samples[ref.idx]
		id := uint64(ref.track + 1)

		if st, ok := subRouting[id]; ok {
			data, err := readSampleMax(ps.src, s.offset, s.size, maxSubtitleSampleBytes, "subtitle")
			if err != nil {
				return errf("read subtitle sample: %w", err)
			}
			if text, ok := decodeSubtitleSample(tk.codec, data); ok {
				cue := subtitle.Cue{StartMs: s.ctsMs, Text: string(text)}
				if s.durMs > 0 {
					cue.EndMs = s.ctsMs + s.durMs
				}
				st.cues = append(st.cues, cue)
			}
			continue
		}
		ft, ok := routing[id]
		if !ok {
			continue
		}
		if ft.offs != nil {
			// Read from the source when the segments are written: the table says where.
			if ft.outTrack.sampleEntry == nil {
				data, err := readSample(ps.src, s.offset, s.size)
				if err != nil {
					return errf("read sample: %w", err)
				}
				if ft.outTrack.sampleEntry, err = ft.outTrack.spec.sampleEntry(&ft.outTrack.mkv, data); err != nil {
					return err
				}
			}
			ft.offs = append(ft.offs, s.offset)
			ft.samples = append(ft.samples, fragSample{size: s.size, ptsMs: s.ctsMs, blockPtsMs: s.ctsMs, sync: s.sync})
			if ft.ptsTS != nil {
				ft.ptsTS = append(ft.ptsTS, s.ctsTicks)
			}
			processed += int64(s.size)
			if progress != nil {
				progress(processed, ps.size)
			}
			continue
		}
		data, err := readSample(ps.src, s.offset, s.size)
		if err != nil {
			return errf("read sample: %w", err)
		}
		data, err = ft.outTrack.convertFrame(data)
		if err != nil {
			return errf("convert frame: %w", err)
		}
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
		// size is the written (possibly converted) byte count; processed tracks
		// progress against the source, so it keeps the source sample size.
		ft.samples = append(ft.samples, fragSample{size: uint32(len(data)), ptsMs: s.ctsMs, blockPtsMs: s.ctsMs, sync: s.sync})
		if ft.ptsTS != nil {
			ft.ptsTS = append(ft.ptsTS, s.ctsTicks)
		}
		processed += int64(s.size)
		if progress != nil {
			progress(processed, ps.size)
		}
	}
	return nil
}

// mp4NativeTimescale is the timescale an MP4 source's video track keeps in the
// fragments - its own - or 0 to time the track on the millisecond timeline
// like any other (a Matroska source, a non-video track, a table that spans
// more ticks than a 32-bit media duration holds). The source states each
// frame's time in its own ticks; taking them through milliseconds and back
// turns a constant 1001/24000 s frame into a run of 41 and 42 ms ones.
func mp4NativeTimescale(ps *packagingSource, t *outTrack) uint32 {
	if ps.mv == nil || !t.spec.video {
		return 0
	}
	ti := int(t.mkv.ID) - 1
	if ti < 0 || ti >= len(ps.mv.tracks) {
		return 0
	}
	tk := &ps.mv.tracks[ti]
	if tk.timescale == 0 || len(tk.samples) == 0 {
		return 0
	}
	lo, hi := tk.samples[0].ctsTicks, tk.samples[0].ctsTicks
	for i := range tk.samples {
		lo, hi = min(lo, tk.samples[i].ctsTicks), max(hi, tk.samples[i].ctsTicks)
	}
	if hi-lo >= 1<<30 {
		return 0
	}
	return tk.timescale
}

// planCtxEvery is how many samples a plan loop handles between two checks of its context.
const planCtxEvery = 4096

// mp4PlanTracks builds the on-demand plan state for an MP4 source: each
// track's fragment samples fully timed from the sample table (head-only) plus
// the parallel file offsets Segment reads through. Returns the fragTracks in
// planTracks order and the per-track sample offsets.
func mp4PlanSamples(ctx context.Context, ps *packagingSource, media []*outTrack) (fts []*fragTrack, offsets [][]int64, err error) {
	for _, t := range media {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ti := int(t.mkv.ID) - 1
		if ti < 0 || ti >= len(ps.mv.tracks) {
			return nil, nil, errf("track %d out of range", t.mkv.ID)
		}
		samples := ps.mv.tracks[ti].samples
		ft := &fragTrack{outTrack: t, timescale: mediaTimescale(t)}
		var ptsTS []int64
		if ts := mp4NativeTimescale(ps, t); ts != 0 {
			ft.timescale, ptsTS = ts, make([]int64, len(samples))
		}
		offs := make([]int64, len(samples))
		ft.samples = make([]fragSample, len(samples))
		for i := range samples {
			if i%planCtxEvery == 0 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}
			ft.samples[i] = fragSample{size: samples[i].size, ptsMs: samples[i].ctsMs,
				blockPtsMs: samples[i].ctsMs, sync: samples[i].sync}
			offs[i] = samples[i].offset
			if ptsTS != nil {
				ptsTS[i] = samples[i].ctsTicks
			}
		}
		if len(ft.samples) == 0 {
			return nil, nil, errf("track %d produced no samples", t.mp4ID)
		}
		// The sample entry: from CodecPrivate, or lazily from the first frame.
		if t.sampleEntry == nil {
			data, rerr := readSampleMax(ps.src, samples[0].offset, samples[0].size, maxFrameSampleBytes, "first")
			if rerr != nil {
				return nil, nil, rerr
			}
			entry, eerr := t.spec.sampleEntry(&t.mkv, data)
			if eerr != nil {
				return nil, nil, eerr
			}
			t.sampleEntry = entry
		}
		grid := audioGridTS(t, ft.timescale)
		if grid == 0 {
			grid = laceGridTS(t, len(ft.samples), func(i int) int64 { return ft.samples[i].blockPtsMs }, ft.timescale)
			last := ft.samples[len(ft.samples)-1].blockPtsMs
			lastFrames := int64(0)
			for i := len(ft.samples) - 1; i >= 0 && ft.samples[i].blockPtsMs == last; i-- {
				lastFrames++
			}
			grid = settleStride(grid, ps.c, t, ft.timescale, ft.samples[0].blockPtsMs, last, lastFrames)
		}
		if grid == 0 {
			grid = mp4FrameGridTS(ps, t, ft.timescale)
		}
		off, hasCTS, totalTS, ctsShift := fillFragTimingTS(ft.samples, ptsTS, t.frameDurMs, ft.timescale, grid)
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ft.offsetMs, ft.hasCTS, ft.durMediaTS = off, hasCTS, totalTS
		ft.ctsShiftTS = ctsShift
		ft.durMovieMs = totalTS
		if ft.timescale != movieTimescale {
			ft.durMovieMs = totalTS * int64(movieTimescale) / int64(ft.timescale)
		}
		ft.presentMs = off + ft.durMovieMs
		fts = append(fts, ft)
		offsets = append(offsets, offs)
	}
	return fts, offsets, nil
}

// mp4SubTable is an MP4 text track's samples as its sample table states them; the text is read on first use (mp4SubWindow).
type mp4SubTable struct {
	codec   string
	samples []fragSample // size only, the shape readSampleRuns reads
	offs    []int64
	startMs []int64
	durMs   []int64
	state   []uint8 // subUnread, subEmpty or subText
	text    []string
	later   []int // per sample, the first text sample after it that starts after it (-1 none), once known (laterText)
}

const (
	subUnread uint8 = iota
	subEmpty
	subText
)

// mp4SubTables lists each text track's samples from the sample table alone; a sample too short to hold text is left out unread.
func mp4SubTables(ps *packagingSource, subs []hlsSubTrack) []*mp4SubTable {
	out := make([]*mp4SubTable, len(subs))
	for i := range subs {
		t := &mp4SubTable{}
		out[i] = t
		ti := int(subs[i].track.ID) - 1
		if ti < 0 || ti >= len(ps.mv.tracks) {
			continue
		}
		tk := &ps.mv.tracks[ti]
		t.codec = tk.codec
		for si := range tk.samples {
			s := &tk.samples[si]
			if s.size <= minSubSampleSize(tk.codec) {
				continue
			}
			t.samples = append(t.samples, fragSample{size: s.size})
			t.offs = append(t.offs, s.offset)
			t.startMs = append(t.startMs, s.ctsMs)
			t.durMs = append(t.durMs, s.durMs)
		}
		t.state = make([]uint8, len(t.samples))
		t.text = make([]string, len(t.samples))
	}
	return out
}

// minSubSampleSize is the largest sample that decodes to no text: a tx3g length prefix alone, a wvtt vttc+payl without payload.
func minSubSampleSize(codec string) uint32 {
	if codec == "webvtt" {
		return 16
	}
	return 2
}

// load reads and decodes the samples at idx that are still unread, in file order.
func (t *mp4SubTable) load(ctx context.Context, open func(context.Context) (mkv.ReadSeekCloser, error), idx []int) error {
	var todo []int
	for _, j := range idx {
		if t.state[j] == subUnread {
			todo = append(todo, j)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	sort.Slice(todo, func(a, b int) bool { return t.offs[todo[a]] < t.offs[todo[b]] })
	samples := make([]fragSample, len(todo))
	offs := make([]int64, len(todo))
	var total int64
	for k, j := range todo {
		samples[k], offs[k] = t.samples[j], t.offs[j]
		total += int64(t.samples[j].size)
	}
	src, err := open(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := segmentFits(total); err != nil {
		return err
	}
	buf := make([]byte, total)
	if err := readSampleRuns(ctx, src, samples, offs, buf); err != nil {
		return errf("read subtitle sample: %w", err)
	}
	for k, j := range todo {
		data := buf[:samples[k].size]
		buf = buf[samples[k].size:]
		t.state[j] = subEmpty
		if text, ok := decodeSubtitleSample(t.codec, data); ok {
			t.state[j], t.text[j] = subText, string(text)
		}
	}
	return nil
}

// window returns the cues meeting [from, to), each ended as subtitle.SettleCueEnds ends it over the whole track, reading only the text that decides them.
func (t *mp4SubTable) window(ctx context.Context, open func(context.Context) (mkv.ReadSeekCloser, error), from, to int64) ([]subtitle.Cue, error) {
	var idx []int
	for j := range t.samples {
		if t.startMs[j] < to && (t.durMs[j] <= 0 || t.startMs[j]+t.durMs[j] > from) {
			idx = append(idx, j)
		}
	}
	if err := t.load(ctx, open, idx); err != nil {
		return nil, err
	}
	var cues []subtitle.Cue
	for _, j := range idx {
		if t.state[j] != subText {
			continue
		}
		end := t.startMs[j] + t.durMs[j]
		if t.durMs[j] <= 0 {
			next, err := t.laterText(ctx, open, j)
			if err != nil {
				return nil, err
			}
			nextMs := int64(0)
			if next >= 0 {
				nextMs = t.startMs[next]
			}
			end = subtitle.CueEndMs(t.startMs[j], t.text[j], nextMs, next >= 0, 0)
		}
		if end > from {
			cues = append(cues, subtitle.Cue{StartMs: t.startMs[j], EndMs: end, Text: t.text[j]})
		}
	}
	return cues, nil
}

// laterText is the first text sample after j that starts after it (-1 for none), subtitle.SettleCueEnds' next cue:
// each sample's answer is kept, and one starting no later than j hands over its own, so the whole track costs one pass.
func (t *mp4SubTable) laterText(ctx context.Context, open func(context.Context) (mkv.ReadSeekCloser, error), j int) (int, error) {
	const unknown = -2
	if t.later == nil {
		t.later = make([]int, len(t.samples))
		for i := range t.later {
			t.later[i] = unknown
		}
	}
	if t.later[j] != unknown {
		return t.later[j], nil
	}
	stack := []int{j} // samples waiting for their answer, each hanging on the one above it
	cand := map[int]int{}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		k, ok := cand[top]
		if !ok {
			var err error
			if k, err = t.nextText(ctx, open, top); err != nil {
				return -1, err
			}
		}
		for k >= 0 && t.startMs[k] <= t.startMs[top] && t.later[k] != unknown {
			k = t.later[k]
		}
		if k >= 0 && t.startMs[k] <= t.startMs[top] {
			cand[top] = k
			stack = append(stack, k) // k's answer first
			continue
		}
		t.later[top] = k
		delete(cand, top)
		stack = stack[:len(stack)-1]
	}
	return t.later[j], nil
}

// nextText is the first sample after j that holds text (-1 for none), reading ahead as far as that takes.
func (t *mp4SubTable) nextText(ctx context.Context, open func(context.Context) (mkv.ReadSeekCloser, error), j int) (int, error) {
	for k := j + 1; k < len(t.samples); k++ {
		if t.state[k] == subUnread {
			if err := t.load(ctx, open, []int{k}); err != nil {
				return -1, err
			}
		}
		if t.state[k] == subText {
			return k, nil
		}
	}
	return -1, nil
}
