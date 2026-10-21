package mp4

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// boundSegmentSpans makes the cue-offset estimate of each segment's bytes
// robust to the source's layout. The span between two video keyframe clusters
// is the segment's media only when the file interleaves its tracks; on a
// block-ordered source the span that straddles a block boundary swallows the
// other tracks' whole blocks - minutes of audio - and read as a bitrate it
// declared a 1.5 Mb/s rung at 24 Mb/s (measured: 12x), a rung a player then
// never picks.
//
// Statistics alone cannot tell such a span from a real peak: measured on real
// releases, one 6 s segment in a film runs at 4-5x the median byte rate, and a
// swallowed block of one minute adds only ~3x. So a span past three times the
// median rate is only a CANDIDATE; confirm(k) then looks at the file - the
// cluster headers inside the span, a cluster whose timestamp steps back from
// the previous one's being the foreign block itself - and only a confirmed
// span is replaced by what the segment's duration is worth at the median
// rate, the bytes taken out being handed back to every segment at their
// average rate (they are the other tracks' media, which the spans of such a
// file otherwise lack). Real peaks stay whatever their size. At most
// maxForeignChecks candidates are examined, largest first, to bound the
// reads on a remote source; blocks short enough to stay under the trigger
// over-declare by less than 3x and are left alone.
func boundSegmentSpans(segs []segInfo, confirm func(k int) bool) []segInfo {
	if confirm == nil {
		return segs
	}
	candidates, median := spanCandidates(segs)
	if len(candidates) == 0 {
		return segs
	}
	out := make([]segInfo, len(segs))
	copy(out, segs)
	var removed float64
	for _, k := range candidates {
		if confirm(k) {
			out[k].bytes = int64(median * segs[k].durSec)
			removed += float64(segs[k].bytes - out[k].bytes)
		}
	}
	return handBack(segs, out, removed)
}

// spanCandidates returns the spans past three times the median byte rate, largest rate first (at most maxForeignChecks), and that median.
func spanCandidates(segs []segInfo) ([]int, float64) {
	rates := make([]float64, 0, len(segs))
	for _, s := range segs {
		if s.durSec > 0 && s.bytes > 0 {
			rates = append(rates, float64(s.bytes)/s.durSec)
		}
	}
	if len(rates) < 3 {
		return nil, 0
	}
	sort.Float64s(rates)
	median := rates[len(rates)/2]
	if median <= 0 {
		return nil, 0
	}
	var candidates []int
	for k, s := range segs {
		if s.durSec > 0 && float64(s.bytes) > 3*median*s.durSec {
			candidates = append(candidates, k)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := segs[candidates[i]], segs[candidates[j]]
		return float64(a.bytes)/a.durSec > float64(b.bytes)/b.durSec
	})
	if len(candidates) > maxForeignChecks {
		candidates = candidates[:maxForeignChecks]
	}
	return candidates, median
}

// handBack spreads the bytes taken out of the confirmed spans over every segment.
func handBack(segs, out []segInfo, removed float64) []segInfo {
	if removed <= 0 {
		return out
	}
	var totalDur float64
	// The bytes taken out of the confirmed spans are the other tracks' media
	// (their blocks), which every span of a block-ordered file lacks - the
	// spans there measure the video alone. Hand them back to every segment at
	// their average rate, so the peak counts audio and video as it does on an
	// interleaved file.
	for _, s := range segs {
		totalDur += s.durSec
	}
	if totalDur > 0 {
		perSec := removed / totalDur
		for i := range out {
			out[i].bytes += int64(perSec * out[i].durSec)
		}
	}
	return out
}

// maxForeignChecks bounds how many candidate spans foreignBlockChecker reads
// into (a couple of dozen tiny reads each at most); a real release has one to
// four spans past the trigger, a block-ordered film one per block boundary.
const maxForeignChecks = 32

// maxForeignClusterHeaders bounds the cluster headers read inside one
// candidate span. A foreign block starts right after the segment's own
// clusters (two or three for a 6 s segment at 2 s clusters, a dozen for
// sub-second clusters), so its first cluster - the one whose timestamp steps
// back - is met within the first headers or not at all.
const maxForeignClusterHeaders = 16

// foreignBlockChecker returns the confirm callback boundSegmentSpans uses on
// this plan's source (opened on first use; the returned close releases it)
// and a no-op close when nothing was opened.
func (p *HLSPlan) foreignBlockChecker(ctx context.Context, srcPath string, fs *mkv.FS, segs []segInfo) (func(k int) bool, func()) {
	if hintsRanges(fs, srcPath) { // remote: every header read is a round trip, read them all at once
		cands, _ := spanCandidates(segs)
		verdicts := p.foreignVerdicts(ctx, srcPath, fs, cands)
		return func(k int) bool { return verdicts[k] }, func() {}
	}
	var (
		src  mkv.ReadSeekCloser
		size int64
		bad  bool
	)
	confirm := func(k int) bool {
		if bad || ctx.Err() != nil {
			return false
		}
		if src == nil {
			f, err := fs.DoOpenContext(ctx, srcPath)
			if err != nil {
				bad = true
				return false
			}
			st, err := fs.DoStat(srcPath)
			if err != nil {
				f.Close()
				bad = true
				return false
			}
			src, size = f, st.Size()
		}
		end := size
		if k+1 < len(p.offsets) {
			end = p.offsets[k+1]
		}
		return spanHoldsForeignBlock(ctx, countedSource{src, &p.walkRead}, p.offsets[k], end)
	}
	return confirm, func() {
		if src != nil {
			src.Close()
		}
	}
}

// foreignHeaderBytes is what one cluster header read takes: ID, size and Timestamp.
const foreignHeaderBytes = 32

// foreignReadParallel bounds the header reads in flight on a remote source.
const foreignReadParallel = 2

// foreignVerdicts is spanHoldsForeignBlock over each candidate span of a
// remote source, with the round trips taken at once: the headers at each
// span's start and its cued clusters are read together first (exact ranges,
// contiguous ones in one request), then each span walks its clusters, the
// spans side by side, reading only the headers no cue pointed at.
func (p *HLSPlan) foreignVerdicts(ctx context.Context, srcPath string, fs *mkv.FS, cands []int) map[int]bool {
	verdicts := make(map[int]bool, len(cands))
	if len(cands) == 0 {
		return verdicts
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		return verdicts
	}
	ends := make(map[int]int64, len(cands))
	var offs []int64
	for _, k := range cands {
		end := st.Size()
		if k+1 < len(p.offsets) {
			end = p.offsets[k+1]
		}
		ends[k] = end
		offs = append(offs, p.offsets[k])
		i := sort.Search(len(p.clusters), func(j int) bool { return p.clusters[j] > p.offsets[k] })
		for n := 1; i < len(p.clusters) && p.clusters[i] < end && n < maxForeignClusterHeaders; i, n = i+1, n+1 {
			offs = append(offs, p.clusters[i])
		}
	}
	known := p.readHeadersAt(ctx, srcPath, fs, offs)
	var mu sync.Mutex
	sem := make(chan struct{}, foreignReadParallel)
	var wg sync.WaitGroup
	for _, k := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var src mkv.ReadSeekCloser
			defer func() {
				if src != nil {
					src.Close()
				}
			}()
			read := func(off int64, buf []byte) (int, error) {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				if h, ok := known[off]; ok {
					n := copy(buf, h)
					if n < len(buf) {
						return n, io.ErrUnexpectedEOF
					}
					return n, nil
				}
				sem <- struct{}{}
				defer func() { <-sem }()
				if src == nil {
					f, err := fs.DoOpenContext(ctx, srcPath)
					if err != nil {
						return 0, err
					}
					src = f
				}
				return readFullAt(src, buf, off)
			}
			v := spanHoldsForeignBlockAt(read, p.offsets[k], ends[k])
			mu.Lock()
			verdicts[k] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	return verdicts
}

// readHeadersAt reads foreignHeaderBytes at each of offs (exact ranges, the
// ones a few KiB apart merged into one), foreignReadParallel at a time; a
// header that failed to read is left out.
func (p *HLSPlan) readHeadersAt(ctx context.Context, srcPath string, fs *mkv.FS, offs []int64) map[int64][]byte {
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	type run struct{ off, end int64 }
	var runs []run
	for _, o := range offs {
		if n := len(runs); n > 0 && o <= runs[n-1].end+foreignMergeGap {
			runs[n-1].end = max(runs[n-1].end, o+foreignHeaderBytes)
			continue
		}
		runs = append(runs, run{o, o + foreignHeaderBytes})
	}
	known := make(map[int64][]byte, len(offs))
	var mu sync.Mutex
	sem := make(chan struct{}, foreignReadParallel)
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			src, err := fs.DoOpenContext(ctx, srcPath)
			if err != nil {
				return
			}
			defer src.Close()
			buf := make([]byte, r.end-r.off)
			n, _ := readFullAt(src, buf, r.off)
			mu.Lock()
			defer mu.Unlock()
			for _, o := range offs {
				if o >= r.off && o < r.end && o-r.off < int64(n) {
					known[o] = buf[o-r.off : min(int64(n), o-r.off+foreignHeaderBytes)]
				}
			}
		}()
	}
	wg.Wait()
	return known
}

// foreignMergeGap is the most bytes between two headers read in one request rather than two.
const foreignMergeGap = 4 << 10

// spanHoldsForeignBlock walks the cluster headers of [from, end) - the ID,
// size and Timestamp of each cluster, one small read per cluster, no block
// read - and reports whether a cluster's timestamp steps BACK from the
// previous cluster's: the segment's span then contains another track's
// block, stored out of time order (an interleaving muxer writes cluster
// timestamps forward, always). The comparison is between consecutive
// clusters, not against the segment's start: a segment's own first cluster
// legitimately starts before its keyframe. An unknown-size cluster or
// anything that is not a cluster ends the walk undecided (false: the span is
// left as measured).
func spanHoldsForeignBlock(ctx context.Context, src io.ReadSeeker, from, end int64) bool {
	return spanHoldsForeignBlockAt(func(off int64, buf []byte) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if _, err := src.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		return io.ReadFull(src, buf)
	}, from, end)
}

// spanHoldsForeignBlockAt is spanHoldsForeignBlock reading each cluster header through read (io.ReadFull's contract).
func spanHoldsForeignBlockAt(read func(off int64, buf []byte) (int, error), from, end int64) bool {
	var buf [foreignHeaderBytes]byte
	off := from
	var prev int64 = -1
	for n := 0; n < maxForeignClusterHeaders && off+8 < end; n++ {
		got, err := read(off, buf[:])
		if err != nil && got < 12 {
			return false
		}
		r := bytes.NewReader(buf[:got])
		h, hdrLen, err := ebml.ReadElementHeader(r)
		if err != nil || h.Size < 0 {
			return false
		}
		if h.ID != mkv.IDCluster {
			off += int64(hdrLen) + h.Size // a stray top-level element (Cues, Tags): step over it
			continue
		}
		ch, _, err := ebml.ReadElementHeader(r)
		if err != nil || ch.ID != mkv.IDTimestamp || ch.Size <= 0 || ch.Size > 8 {
			return false
		}
		ts, err := ebml.ReadUint(r, ch.Size)
		if err != nil {
			return false
		}
		if prev >= 0 && int64(ts) < prev {
			return true
		}
		prev = int64(ts)
		off += int64(hdrLen) + h.Size
	}
	return false
}
