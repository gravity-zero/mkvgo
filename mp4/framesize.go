package mp4

import (
	"encoding/binary"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// frameSamplesFromHeader is the samples one frame covers at the track's sample
// rate, as the track's configuration states it: AAC from its
// AudioSpecificConfig (1024 or 960 core samples, doubled when the track runs
// at the SBR output rate), MP3 from its sampling family, TrueHD by
// specification (40 samples per access unit at 48 kHz), FLAC from a fixed
// STREAMINFO block size. 0 when the header does not say.
func frameSamplesFromHeader(t *mkv.Track) int64 {
	if t.Type != mkv.AudioTrack || t.SampleRate == nil || *t.SampleRate <= 0 {
		return 0
	}
	rate := *t.SampleRate
	switch t.Codec {
	case "aac":
		if len(t.CodecPrivate) == 0 {
			return 1024
		}
		c := reader.ParseAACConfig(t.CodecPrivate)
		n := float64(c.FrameLength)
		if c.SBR && c.OutputRate > 0 && c.SampleRate > 0 && rate == c.OutputRate { // the track runs at the SBR output rate
			n *= c.OutputRate / c.SampleRate
		}
		return int64(n + 0.5)
	case "mp3":
		if rate >= 32000 {
			return 1152
		}
		return 576
	case "truehd":
		return int64(40*rate/48000 + 0.5)
	case "flac":
		p := t.CodecPrivate
		if len(p) >= 4 && string(p[:4]) == "fLaC" {
			p = p[4:]
		}
		if len(p) >= 8 && p[0]&0x7F == 0 { // STREAMINFO: min and max block size
			if min, max := binary.BigEndian.Uint16(p[4:6]), binary.BigEndian.Uint16(p[6:8]); min == max && min > 0 {
				return int64(min)
			}
		}
	}
	return 0
}

// frameSamplesFromPayload reads the frame size from the first frame's own
// header, for codecs whose configuration lives in the bitstream: AC-3 and
// E-AC-3 (256 to 1536 samples a syncframe), DTS core (32 samples a block),
// MP3 (its version and layer). 0 when the payload does not say.
func frameSamplesFromPayload(codec string, data []byte) int64 {
	switch codec {
	case "ac3", "eac3":
		if n, _, ok := ac3PacketSamples(data); ok {
			return int64(n)
		}
	case "dts":
		if len(data) >= 8 && binary.BigEndian.Uint32(data[:4]) == 0x7FFE8001 {
			nblks := (binary.BigEndian.Uint16(data[4:6]) >> 2) & 0x7F // FTYPE(1) SHORT(5) CPF(1) NBLKS(7)
			if nblks >= 5 {
				return int64(nblks+1) * 32
			}
		}
	case "mp3":
		if len(data) >= 4 && data[0] == 0xFF && data[1]&0xE0 == 0xE0 {
			version, layer := data[1]>>3&3, data[1]>>1&3
			switch layer {
			case 3: // layer I
				return 384
			case 2: // layer II
				return 1152
			case 1: // layer III
				if version == 3 {
					return 1152
				}
				return 576
			}
		}
	}
	return 0
}

// learnFrame records the track's frame size from its first payload when the header left it unknown.
func (t *outTrack) learnFrame(data []byte) {
	if t.frameSamples == 0 && !t.frameLearned {
		t.frameSamples = frameSamplesFromPayload(t.mkv.Codec, data)
	}
	t.frameLearned = true
}

// codecFrameSamples lists the samples a frame of the codec may hold at a fixed rate, the usual size first.
func codecFrameSamples(codec string) []int64 {
	switch codec {
	case "aac":
		return []int64{1024, 960, 2048, 512, 480}
	case "mp3":
		return []int64{1152, 576, 384}
	case "ac3", "eac3":
		return []int64{1536, 768, 512, 256}
	case "dts":
		return []int64{512, 1024, 2048, 256, 4096}
	}
	return nil
}

// snapGridTS replaces a stride measured from millisecond block timecodes by
// the frame duration the stream states, else by a frame size the codec is
// known to use when one lies within a percent of it: the measurement carries
// the rounding of two timecodes, up to a tick a frame, and a tick a frame is
// eleven seconds over a two-hour film; a frame shorter than a millisecond
// (TrueHD) cannot be measured from timecodes at all.
func snapGridTS(measured int64, t *outTrack, mts uint32) int64 {
	if measured <= 0 || t == nil || t.mkv.SampleRate == nil || *t.mkv.SampleRate <= 0 {
		return measured
	}
	ticks := func(n int64) int64 { return int64(float64(n)*float64(mts)/(*t.mkv.SampleRate) + 0.5) }
	if t.frameSamples > 0 {
		return ticks(t.frameSamples)
	}
	for _, n := range codecFrameSamples(t.mkv.Codec) {
		if exact := ticks(n); within(exact, measured, 1) {
			return exact
		}
	}
	return measured
}

// within reports whether b lies within pct percent of a.
func within(a, b, pct int64) bool {
	d := a - b
	return d*100 <= a*pct && -d*100 <= a*pct
}

// longStrideTS measures the frame stride over the whole track - the span from
// the first block to the last one over the frames between them, the count a
// trusted NUMBER_OF_FRAMES statistic gives - for a codec whose frame size no
// header or table knows: the rounding of two timecodes weighs nothing over
// hours. 0 without a trusted count or a span.
func longStrideTS(c *mkv.Container, t *outTrack, mts uint32, firstTC, lastTC, lastFrames int64) int64 {
	if c == nil || t == nil {
		return 0
	}
	st, ok := mkv.TrustedTrackStatistics(c)[t.mkv.ID]
	slots := st.Frames - lastFrames
	if !ok || slots <= 0 || lastTC <= firstTC {
		return 0
	}
	span := tsScale(mts)(lastTC) - tsScale(mts)(firstTC)
	return (span + slots/2) / slots
}

// gridSpread is the grid stride of a laced track whose frame size nothing
// confirms: its frames are spread over each block's span instead of riding a
// measured stride that drifts (see spreadLaced).
const gridSpread = -2

// strideConfirmed reports whether the stream or a codec table vouches for the stride.
func strideConfirmed(stride int64, t *outTrack, mts uint32) bool {
	if t == nil || t.mkv.SampleRate == nil || *t.mkv.SampleRate <= 0 {
		return false
	}
	if t.frameSamples > 0 {
		return true
	}
	for _, n := range codecFrameSamples(t.mkv.Codec) {
		if within(int64(float64(n)*float64(mts)/(*t.mkv.SampleRate)+0.5), stride, 1) {
			return true
		}
	}
	return false
}

// settleStride turns a measured lace stride into the one the timing runs on:
// confirmed by the stream or a codec table it stands; else the whole-track
// measurement within a percent of it; else gridSpread, the bounded fallback.
// 0 (no collapsed lace) passes through.
func settleStride(measured int64, c *mkv.Container, t *outTrack, mts uint32, firstTC, lastTC, lastFrames int64) int64 {
	if measured <= 0 || strideConfirmed(measured, t, mts) {
		return measured
	}
	if long := longStrideTS(c, t, mts, firstTC, lastTC, lastFrames); long > 0 && within(measured, long, 1) {
		return long
	}
	return gridSpread
}

// spreadLaced times collapsed laces without a confirmed stride: a block's
// frames share evenly the span from its timecode to the next block's, so no
// error can build up past one block; the last block runs to nextMs when the
// caller knows the block that follows, else to lastSpan(frames), the span the
// caller gives a final lace of that many frames (lastBlockSpan at the track's
// first pace, the rule the plan's init duration applies). Decode times are
// strictly increasing by construction. set receives each frame's decode time
// (ticks past the first block) and duration.
func spreadLaced(n int, blockPts func(int) int64, scale func(int64) int64, nextMs int64, lastSpan func(frames int64) int64, set func(i int, dts, dur int64)) {
	base := scale(blockPts(0))
	for a := 0; a < n; {
		b := a + 1
		for b < n && blockPts(b) == blockPts(a) {
			b++
		}
		m := int64(b - a)
		start := scale(blockPts(a)) - base
		var end int64
		switch {
		case b < n:
			end = scale(blockPts(b)) - base
		case nextMs >= 0:
			end = scale(nextMs) - base
		default:
			end = start + lastSpan(m)
		}
		if end < start+m {
			end = start + m
		}
		for i := int64(0); i < m; i++ {
			dts := start + i*(end-start)/m
			next := start + (i+1)*(end-start)/m
			set(a+int(i), dts, next-dts)
		}
		a = b
	}
}

// lastBlockSpan is the span a track's final lace takes when nothing follows it: its frames at the first block's pace.
func lastBlockSpan(firstSpan, firstFrames, frames int64) int64 {
	if firstFrames <= 0 || firstSpan <= 0 {
		return frames
	}
	return firstSpan * frames / firstFrames
}

// firstLace describes a track's first collapsed lace: the span to the second block and the frames the first block holds.
func firstLace(n int, blockPts func(int) int64, scale func(int64) int64) (span, frames int64) {
	m := 1
	for m < n && blockPts(m) == blockPts(0) {
		m++
	}
	if m >= n {
		return 0, int64(m)
	}
	return scale(blockPts(m)) - scale(blockPts(0)), int64(m)
}

// AudioFrameSamples is the samples one frame of an audio track covers at its
// sample rate, as its configuration or its first frame states it; 0 when
// neither says (a diagnosis then knows the frame size is unknown).
func AudioFrameSamples(t *mkv.Track, firstFrame []byte) int64 {
	if n := frameSamplesFromHeader(t); n > 0 {
		return n
	}
	return frameSamplesFromPayload(t.Codec, firstFrame)
}

// laceGridTS is deriveGridTS snapped to the track's frame size.
func laceGridTS(t *outTrack, count int, blockPtsAt func(int) int64, mts uint32) int64 {
	return snapGridTS(deriveGridTS(count, blockPtsAt, mts), t, mts)
}
