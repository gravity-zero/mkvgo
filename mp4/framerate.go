package mp4

import (
	"encoding/binary"
	"math"
)

// headerFrameRate derives the nominal (constant) frame rate from the first stts
// entry without expanding the sample table: timescale / sample_delta. This is
// the conventional r_frame_rate for CFR video and is readable head-only. Returns 0 when
// the stts is absent/short or the delta is zero.
func headerFrameRate(stblBoxes []memBox, timescale uint32) float64 {
	if timescale == 0 {
		return 0
	}
	stts, ok := findMemBox(stblBoxes, "stts")
	if !ok || len(stts.payload) < 16 {
		return 0
	}
	// stts: version+flags(4), entry_count(4), then [sample_count(4) sample_delta(4)]…
	delta := binary.BigEndian.Uint32(stts.payload[12:16])
	if delta == 0 {
		return 0
	}
	// A constant rate that does not divide the timescale is stored as deltas
	// one tick apart (24 fps on a millisecond timescale: 42, 41, 42, 42...).
	// The first one alone reads 23.81 fps; the rate is the samples over the
	// time they cover.
	if samples, ticks, ok := sttsRoundedConstant(stts.payload); ok {
		if num, den, ok := standardFrameRate(samples, ticks, timescale); ok {
			return float64(num) / float64(den)
		}
		return float64(timescale) * float64(samples) / float64(ticks)
	}
	// Uneven deltas that swing around a standard rate (672 and 656 ticks for
	// 667.33) average to it; a table that averages to none is a variable rate.
	if samples, ticks := sttsTotals(stts.payload); samples > 1 && ticks > 0 {
		if fps, ok := snapStandardRate(float64(timescale) * float64(samples) / float64(ticks)); ok {
			return fps
		}
	}
	// A variable rate plays at the delta most of its frames have (the shorter on a tie), not its first: that one
	// may be a frame held half a second, which read as 2 fps. Unless that delta covers under a tenth of the time:
	// reordered frames packed one tick apart are most of the deltas and none of the duration, and the average holds.
	if d, covered := dominantDelta(stts.payload); d > 0 {
		if samples, ticks := sttsTotals(stts.payload); samples > 1 && 10*covered < ticks {
			return float64(timescale) * float64(samples) / float64(ticks)
		}
		delta = d
	}
	return float64(timescale) / float64(delta)
}

// dominantDelta returns the stts delta covering the most samples, the shorter on a tie, and the ticks those samples
// cover; 0 when the table is short.
func dominantDelta(payload []byte) (uint32, uint64) {
	if len(payload) < 8 {
		return 0, 0
	}
	counts := map[uint32]uint64{}
	n := binary.BigEndian.Uint32(payload[4:8])
	for i := uint32(0); i < n && 16+int(i)*8 <= len(payload); i++ {
		if d := binary.BigEndian.Uint32(payload[12+i*8:]); d > 0 {
			counts[d] += uint64(binary.BigEndian.Uint32(payload[8+i*8:]))
		}
	}
	var best uint32
	for d, c := range counts {
		if best == 0 || c > counts[best] || (c == counts[best] && d < best) {
			best = d
		}
	}
	return best, counts[best] * uint64(best)
}

// sttsTotals returns the samples an stts describes and the ticks they cover, 0 and 0 when it is short.
func sttsTotals(payload []byte) (samples, ticks uint64) {
	entries := binary.BigEndian.Uint32(payload[4:8])
	if entries < 2 || uint64(len(payload)) < 8+8*uint64(entries) {
		return 0, 0
	}
	for i := uint32(0); i < entries; i++ {
		count := uint64(binary.BigEndian.Uint32(payload[8+8*i:]))
		samples += count
		ticks += count * uint64(binary.BigEndian.Uint32(payload[12+8*i:]))
	}
	return samples, ticks
}

// snapStandardRate returns the standard frame rate within 2e-4 of fps, and whether there is one.
func snapStandardRate(fps float64) (float64, bool) {
	for _, r := range [][2]float64{{24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {48, 1}, {50, 1}, {60000, 1001}, {60, 1}, {100, 1}, {120000, 1001}, {120, 1}} {
		if std := r[0] / r[1]; math.Abs(fps-std) <= 2e-4*std {
			return std, true
		}
	}
	return 0, false
}

// standardFrameRate names the standard frame rate a rounded constant stts
// stands for, when the measurement allows exactly one. The samples cover ticks
// give or take one tick of rounding, so the true frame duration lies in
// [(ticks-1)/samples, (ticks+1)/samples] ticks: a rate is returned only when
// it is the single standard one (an integer, or an NTSC n*1000/1001) inside
// that interval. A long track pins it down - 23.976 fps is told from 24 - and
// then the exact fraction is used instead of an average a few nanoseconds off,
// from which a demuxer derives another fraction than the source's (9998/417
// for 24000/1001). A short track leaves several candidates: no answer.
func standardFrameRate(samples, ticks uint64, timescale uint32) (num, den uint64, ok bool) {
	if samples == 0 || ticks < 2 || timescale == 0 {
		return 0, 0, false
	}
	lo := float64(ticks-1) / float64(samples) / float64(timescale) // seconds per frame
	hi := float64(ticks+1) / float64(samples) / float64(timescale)
	within := func(n, d uint64) bool {
		dur := float64(d) / float64(n)
		return dur >= lo && dur <= hi
	}
	found := 0
	for n := uint64(1); n <= 240; n++ {
		if within(n, 1) {
			num, den = n, 1
			found++
		}
	}
	for _, n := range []uint64{24, 30, 48, 60, 120} {
		if within(n*1000, 1001) {
			num, den = n*1000, 1001
			found++
		}
	}
	return num, den, found == 1
}

// sttsRoundedConstant reports whether an stts describes a constant frame
// duration rounded to the timescale - several entries whose deltas are all
// within one tick of each other - and returns the sample count and the ticks
// they cover. A single-entry table is constant by construction and is not
// reported here (its one delta is exact); deltas further apart are a variable
// rate, which no average describes.
func sttsRoundedConstant(payload []byte) (samples, ticks uint64, ok bool) {
	entries := binary.BigEndian.Uint32(payload[4:8])
	if entries < 2 || uint64(len(payload)) < 8+8*uint64(entries) {
		return 0, 0, false
	}
	var lo, hi uint32
	for i := uint32(0); i < entries; i++ {
		count := binary.BigEndian.Uint32(payload[8+8*i:])
		delta := binary.BigEndian.Uint32(payload[12+8*i:])
		if i == 0 || delta < lo {
			lo = delta
		}
		if delta > hi {
			hi = delta
		}
		samples += uint64(count)
		ticks += uint64(count) * uint64(delta)
	}
	if lo == 0 || hi-lo > 1 || samples == 0 {
		return 0, 0, false
	}
	return samples, ticks, true
}

// headerVideoFrameDurNs returns a video track's constant frame duration in
// nanoseconds: the single stts delta, or the average of a rounded constant one
// (see sttsRoundedConstant). 0 when the rate is variable or unknown.
func headerVideoFrameDurNs(stblBoxes []memBox, timescale uint32) int64 {
	if ns := headerConstantFrameDurNs(stblBoxes, timescale); ns > 0 {
		return ns
	}
	stts, found := findMemBox(stblBoxes, "stts")
	if timescale == 0 || !found || len(stts.payload) < 16 {
		return 0
	}
	samples, ticks, ok := sttsRoundedConstant(stts.payload)
	if !ok {
		return 0
	}
	if num, den, ok := standardFrameRate(samples, ticks, timescale); ok {
		return int64(den * 1_000_000_000 / num) // the DefaultDuration muxers write for that rate
	}
	return int64((float64(ticks)*1e9/float64(timescale))/float64(samples) + 0.5)
}

// headerConstantFrameDurNs returns the constant per-frame duration in
// nanoseconds when the stts declares exactly one entry (every sample the same
// delta), 0 otherwise. Head-only, like headerFrameRate.
func headerConstantFrameDurNs(stblBoxes []memBox, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	stts, ok := findMemBox(stblBoxes, "stts")
	if !ok || len(stts.payload) < 16 {
		return 0
	}
	if binary.BigEndian.Uint32(stts.payload[4:8]) != 1 { // entry_count
		return 0
	}
	delta := binary.BigEndian.Uint32(stts.payload[12:16])
	if delta == 0 {
		return 0
	}
	return (int64(delta)*1_000_000_000 + int64(timescale)/2) / int64(timescale)
}

// sttsDeltaRange returns the smallest and largest sample duration the stts
// declares, leaving out a final entry of one sample when others precede it
// (the last sample's duration is whatever closes the track). 0, 0 when the
// table is absent or empty.
func sttsDeltaRange(stblBoxes []memBox) (lo, hi uint32) {
	stts, ok := findMemBox(stblBoxes, "stts")
	if !ok || len(stts.payload) < 8 {
		return 0, 0
	}
	entries := binary.BigEndian.Uint32(stts.payload[4:8])
	if entries == 0 || uint64(len(stts.payload)) < 8+8*uint64(entries) {
		return 0, 0
	}
	for i := uint32(0); i < entries; i++ {
		count := binary.BigEndian.Uint32(stts.payload[8+8*i:])
		delta := binary.BigEndian.Uint32(stts.payload[12+8*i:])
		if count == 0 || (i > 0 && i == entries-1 && count == 1) {
			continue
		}
		if lo == 0 || delta < lo {
			lo = delta
		}
		if delta > hi {
			hi = delta
		}
	}
	return lo, hi
}

// headerFrameCount returns the sample count (= frame count for a video track)
// from the stsz/stz2 box, read head-only from the header. 0 when absent.
func headerFrameCount(stblBoxes []memBox) int64 {
	if stsz, ok := findMemBox(stblBoxes, "stsz"); ok && len(stsz.payload) >= 12 {
		// stsz: version+flags(4), sample_size(4), sample_count(4).
		return int64(binary.BigEndian.Uint32(stsz.payload[8:12]))
	}
	if stz2, ok := findMemBox(stblBoxes, "stz2"); ok && len(stz2.payload) >= 12 {
		// stz2: version+flags(4), reserved(3)+field_size(1), sample_count(4).
		return int64(binary.BigEndian.Uint32(stz2.payload[8:12]))
	}
	return 0
}
