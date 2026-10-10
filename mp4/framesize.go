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

// refineStride settles a measured stride the stream could not confirm: a
// header or table match stands; otherwise the whole-track measurement, when
// it lies within a percent of the local one, replaces it.
func refineStride(measured int64, c *mkv.Container, t *outTrack, mts uint32, firstTC, lastTC, lastFrames int64) int64 {
	if measured <= 0 || snapGridTS(measured, t, mts) != measured || t.frameSamples > 0 {
		return measured
	}
	if long := longStrideTS(c, t, mts, firstTC, lastTC, lastFrames); long > 0 && within(measured, long, 1) {
		return long
	}
	return measured
}

// laceGridTS is deriveGridTS snapped to the track's frame size.
func laceGridTS(t *outTrack, count int, blockPtsAt func(int) int64, mts uint32) int64 {
	return snapGridTS(deriveGridTS(count, blockPtsAt, mts), t, mts)
}
