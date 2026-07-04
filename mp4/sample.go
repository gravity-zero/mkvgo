package mp4

import (
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// sample.go — per-track sample bookkeeping and the Sample Table (stbl) child
// boxes derived from it. The muxer streams sample *data* straight to mdat; this
// file only ever holds the small per-sample metadata (size, timestamp, sync
// flag) and the chunk index, so memory stays O(number of samples), not O(bytes).

// sample is the metadata mkvgo retains for one MP4 sample (one decoded frame).
type sample struct {
	size uint32
	pts  int64 // composition time in the track timescale (milliseconds)
	// blockPts is the stored timecode of the source block (mkv.Block
	// .BlockTimecode; equal to pts for unlaced sources): grid timing derives
	// each audio frame's index from it, so the millisecond rounding of the
	// container timeline never reaches the sample table.
	blockPts int64
	sync     bool  // keyframe / sync sample
	dur      int64 // explicit duration (text tracks); 0 = derive from PTS deltas
}

// chunk records one run of samples written contiguously into mdat.
type chunk struct {
	offset uint64 // absolute file offset of the chunk's first byte
	count  int    // number of samples in the chunk
}

// trackSamples accumulates the sample table for a single output track.
type trackSamples struct {
	samples []sample
	chunks  []chunk
}

// addDur appends one sample's metadata in decode (file) order. dur is the
// sample's explicit duration (timed-text/chapter tracks); pass 0 for media
// tracks, whose durations are derived from PTS deltas. blockPts is the source
// block's stored timecode (pts itself for unlaced/synthetic samples).
func (ts *trackSamples) addDur(size uint32, pts, blockPts, dur int64, sync bool) {
	ts.samples = append(ts.samples, sample{size: size, pts: pts, blockPts: blockPts, sync: sync, dur: dur})
}

// addChunk records a flushed chunk of count samples starting at offset.
func (ts *trackSamples) addChunk(offset uint64, count int) {
	if count > 0 {
		ts.chunks = append(ts.chunks, chunk{offset: offset, count: count})
	}
}

// audioGridTS returns the sample-exact frame stride of a constant-rate audio
// track in its media timescale — round(DefaultDuration × mts / 1e9), e.g.
// exactly 1024 for AAC-LC at 48 kHz — or 0 when the track is not grid-timed
// (video/text, or no declared frame duration). Grid-timed tracks ignore the
// millisecond rounding of the container timeline: anchoring each frame on the
// stored (ms-quantised) block timecodes would jitter the durations by ±1 ms,
// duplicate a DTS whenever a lace's last frame rounds onto the next block's
// timecode, and drift one frame short per segment.
func audioGridTS(t *outTrack, mts uint32) int64 {
	if t.mkv.Type != mkv.AudioTrack || t.mkv.DefaultDurationNs <= 0 {
		return 0
	}
	return (t.mkv.DefaultDurationNs*int64(mts) + 500_000_000) / 1_000_000_000
}

// gridIndex maps a scaled block timestamp (relative to the track's first
// block) to its frame index on the grid. True frame positions are exact
// multiples of gridTS and the stored timecodes carry sub-millisecond rounding
// only — far less than half a frame — so the nearest slot is unambiguous.
// A real gap in the audio lands on the slot nearest its true position, which
// preserves the gap (the decode timeline jumps with it).
func gridIndex(rel, gridTS int64) int64 {
	if rel <= 0 {
		return 0
	}
	return (rel + gridTS/2) / gridTS
}

// deriveGridTS recovers a grid stride for laced audio that collapsed onto its
// block timecodes: when the source declares no DefaultDuration, the reader has
// no stride to spread a lace's frames and gives every frame of the block the
// same play time (so a bare PTS→DTS mapping would hand consecutive frames an
// identical, non-monotonic decode time — rejected by players). The stride is the
// media-scaled gap between the first two distinct block timecodes divided by the
// number of frames the first block held; constant-rate audio is uniform, so it
// is the per-frame duration and applies to every frame. blockPtsAt reads the
// stored (Block, not per-frame) timecode of sample i. It returns 0 — leaving the
// caller on its ordinary timing path — unless the samples actually show a
// collapsed lace (a run of two or more identical block timecodes then a larger
// one), which video and unlaced audio never produce. AC-3/E-AC-3 (whole-ms
// frames) recover exactly; a fractional-ms codec (AAC) lands within a tick,
// which the grid's forced per-frame increment keeps strictly monotonic.
func deriveGridTS(count int, blockPtsAt func(int) int64, mts uint32) int64 {
	if count < 2 {
		return 0
	}
	first := blockPtsAt(0)
	n := 0
	for n < count && blockPtsAt(n) == first {
		n++
	}
	if n < 2 || n >= count {
		return 0 // no lace collapse, or only one block to measure the stride
	}
	scale := tsScale(mts)
	span := scale(blockPtsAt(n)) - scale(first)
	if span <= 0 {
		return 0
	}
	return (span + int64(n)/2) / int64(n)
}

// timing holds the decode-time/composition-time information derived from the
// samples' presentation timestamps.
type timing struct {
	durations []int64 // per-sample decode duration (stts source)
	ctts      []int32 // per-sample composition offset = PTS - DTS
	hasCTTS   bool    // true when any composition offset is non-zero
	total     int64   // total track duration (sum of durations)
}

// textTiming builds timing for a timed-text track straight from each sample's
// explicit duration (no decode reordering, so no ctts).
func textTiming(samples []sample) timing {
	t := timing{durations: make([]int64, len(samples)), ctts: make([]int32, len(samples))}
	for i, s := range samples {
		d := s.dur
		if d <= 0 {
			d = 1
		}
		t.durations[i] = d
		t.total += d
	}
	return t
}

// reconstructTiming derives decode timestamps from presentation timestamps.
//
// Matroska stores only a presentation timestamp per block, in decode (storage)
// order. MP4 needs decode timestamps (DTS, via stts) plus composition offsets
// (CTS-DTS, via ctts). We assign DTS as the sorted PTS values in decode order:
// this yields a strictly non-decreasing decode clock whose durations match the
// real cadence (so it is correct for variable frame rate too), and composition
// offsets CTS-DTS that may be negative — which is why the ctts box is emitted as
// version 1 (signed). With no B-frames every offset is zero and ctts is omitted.
//
// lastDurMs is the duration assigned to the final sample (which has no following
// sample to diff against); pass a frame duration derived from the track when
// known, else 0 to reuse the previous sample's duration.
//
// mts is the target media timescale. Sample pts are in the movie timescale (ms);
// they are scaled to mts so the table is expressed in the track's own units. Audio
// tracks pass their sample rate, which makes the durations — and the edit list
// derived from CodecDelay — sample-exact. For mts == movieTimescale this is the
// identity (video/text are unaffected). Scaling the cumulative pts and then diffing
// keeps the rounding error bounded (it does not accumulate across samples).
func reconstructTiming(samples []sample, lastDurMs int64, mts uint32, gridTS int64) timing {
	n := len(samples)
	if n == 0 {
		return timing{}
	}
	scale := func(ptsMs int64) int64 {
		if mts == movieTimescale {
			return ptsMs
		}
		return ptsMs * int64(mts) / int64(movieTimescale)
	}
	if gridTS <= 0 { // laced audio with no DefaultDuration: recover the stride
		gridTS = deriveGridTS(n, func(i int) int64 { return samples[i].blockPts }, mts)
	}

	// Constant-rate audio rides the sample-exact grid (see audioGridTS): frame
	// k decodes at k×gridTS, k re-derived from each block's stored timecode
	// (then +1 within a lace), so millisecond rounding never reaches the table
	// while a real gap still moves k. No reordering → no ctts.
	if gridTS > 0 {
		t := timing{durations: make([]int64, n), ctts: make([]int32, n)}
		anchor := scale(samples[0].blockPts)
		k := int64(0)
		prev := int64(0)
		for i := range samples {
			if i > 0 {
				if samples[i].blockPts != samples[i-1].blockPts {
					nk := gridIndex(scale(samples[i].blockPts)-anchor, gridTS)
					if nk <= k {
						nk = k + 1
					}
					k = nk
				} else {
					k++
				}
				t.durations[i-1] = k*gridTS - prev
			}
			prev = k * gridTS
		}
		t.durations[n-1] = gridTS
		for i := range t.durations {
			t.total += t.durations[i]
		}
		return t
	}

	dts := make([]int64, n)
	for i := range samples {
		dts[i] = scale(samples[i].pts)
	}
	sort.Slice(dts, func(i, j int) bool { return dts[i] < dts[j] })

	t := timing{
		durations: make([]int64, n),
		ctts:      make([]int32, n),
	}
	for i := 0; i < n-1; i++ {
		t.durations[i] = dts[i+1] - dts[i]
	}
	switch {
	case lastDurMs > 0:
		t.durations[n-1] = scale(lastDurMs)
	case n > 1:
		t.durations[n-1] = t.durations[n-2]
	default:
		t.durations[n-1] = 1
	}
	for i := 0; i < n; i++ {
		off := scale(samples[i].pts) - dts[i]
		t.ctts[i] = int32(off)
		if off != 0 {
			t.hasCTTS = true
		}
		t.total += t.durations[i]
	}
	return t
}

// buildSTTS emits the decoding time-to-sample box, run-length encoded.
func buildSTTS(durations []int64) []byte {
	type run struct {
		count uint32
		delta uint32
	}
	var runs []run
	for _, d := range durations {
		dv := uint32(d)
		if n := len(runs); n > 0 && runs[n-1].delta == dv {
			runs[n-1].count++
			continue
		}
		runs = append(runs, run{count: 1, delta: dv})
	}
	return fullBox("stts", 0, 0, func(w *bw) {
		w.u32(uint32(len(runs)))
		for _, r := range runs {
			w.u32(r.count)
			w.u32(r.delta)
		}
	})
}

// buildSTSS emits the sync sample box from 1-based sample numbers. It returns
// nil when every sample is a sync sample (the absence of stss already means
// "all samples are sync"), keeping the table compact for all-intra streams.
func buildSTSS(samples []sample) []byte {
	var nums []uint32
	for i, s := range samples {
		if s.sync {
			nums = append(nums, uint32(i+1))
		}
	}
	if len(nums) == len(samples) {
		return nil
	}
	return fullBox("stss", 0, 0, func(w *bw) {
		w.u32(uint32(len(nums)))
		for _, n := range nums {
			w.u32(n)
		}
	})
}

// buildCTTS emits the composition offset box (version 1, signed offsets),
// run-length encoded. Returns nil when no offsets are needed.
func buildCTTS(t timing) []byte {
	if !t.hasCTTS {
		return nil
	}
	type run struct {
		count  uint32
		offset int32
	}
	var runs []run
	for _, off := range t.ctts {
		if n := len(runs); n > 0 && runs[n-1].offset == off {
			runs[n-1].count++
			continue
		}
		runs = append(runs, run{count: 1, offset: off})
	}
	return fullBox("ctts", 1, 0, func(w *bw) {
		w.u32(uint32(len(runs)))
		for _, r := range runs {
			w.u32(r.count)
			w.i32(r.offset)
		}
	})
}

// buildSTSC emits the sample-to-chunk box, run-length encoded over chunks that
// share the same samples-per-chunk count.
func buildSTSC(chunks []chunk) []byte {
	type entry struct {
		firstChunk     uint32
		samplesPerChnk uint32
	}
	var entries []entry
	for i, c := range chunks {
		spc := uint32(c.count)
		if n := len(entries); n > 0 && entries[n-1].samplesPerChnk == spc {
			continue
		}
		entries = append(entries, entry{firstChunk: uint32(i + 1), samplesPerChnk: spc})
	}
	return fullBox("stsc", 0, 0, func(w *bw) {
		w.u32(uint32(len(entries)))
		for _, e := range entries {
			w.u32(e.firstChunk)
			w.u32(e.samplesPerChnk)
			w.u32(1) // sample_description_index
		}
	})
}

// buildSTSZ emits the sample size box (one entry per sample).
func buildSTSZ(samples []sample) []byte {
	return fullBox("stsz", 0, 0, func(w *bw) {
		w.u32(0) // sample_size = 0 → sizes listed individually
		w.u32(uint32(len(samples)))
		for _, s := range samples {
			w.u32(s.size)
		}
	})
}

// buildChunkOffsets emits the chunk offset table. Each stored offset is relative
// to the start of the mdat payload; base is the absolute file position of that
// payload, added here. co64 selects 64-bit offsets (chosen up front from the
// total size so the moov size is stable regardless of base, which matters for
// the fast-start two-build layout).
func buildChunkOffsets(chunks []chunk, base int64, co64 bool) []byte {
	if co64 {
		return fullBox("co64", 0, 0, func(w *bw) {
			w.u32(uint32(len(chunks)))
			for _, c := range chunks {
				w.u64(uint64(base) + c.offset)
			}
		})
	}
	return fullBox("stco", 0, 0, func(w *bw) {
		w.u32(uint32(len(chunks)))
		for _, c := range chunks {
			w.u32(uint32(uint64(base) + c.offset))
		}
	})
}
