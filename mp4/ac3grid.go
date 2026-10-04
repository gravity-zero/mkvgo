package mp4

// ac3grid.go - the exact duration of an AC-3 / E-AC-3 packet, read from its
// frames. An MP4 source times its audio through the sample table, and a real
// muxer's table strays: a tick short here, a tick long there, around a frame
// that always holds the same number of samples. Carried through the
// millisecond timeline each stray tick becomes a whole millisecond. The frame
// header says how many blocks the frame holds and at what rate, which is the
// duration the grid needs (see audioGridTS).

import "github.com/gravity-zero/mkvgo/mkv"

// ac3Bitrates is the nominal bit rate (kbit/s) behind frmsizecod>>1.
var ac3Bitrates = [19]uint32{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}

// ac3PacketSamples returns how many samples per channel a packet - whole
// syncframes laid end to end, as a container stores them - covers on the time
// line, and their sample rate. Only the frames that move the time line count:
// every AC-3 frame, and of an enhanced stream independent substream 0 (a
// dependent substream, or another programme, covers the same span). ok is
// false for anything that is not exactly whole frames: the caller then keeps
// the container's timing rather than a guessed one.
func ac3PacketSamples(b []byte) (samples, rate uint32, ok bool) {
	for len(b) > 0 {
		if len(b) < 6 || b[0] != 0x0B || b[1] != 0x77 {
			return 0, 0, false
		}
		var size, n, sr uint32
		advances := true
		switch bsid := b[5] >> 3; {
		case bsid <= 10: // AC-3: six blocks, the size from the rate code
			fscod, code := b[4]>>6, b[4]&0x3F
			if fscod == 3 || code >= 38 {
				return 0, 0, false
			}
			kbps := ac3Bitrates[code>>1]
			switch fscod {
			case 0:
				size = kbps * 4
			case 1:
				size = (kbps*96000/44100 + uint32(code&1)) * 2
			case 2:
				size = kbps * 6
			}
			n, sr = 1536, ac3SampleRates[fscod]
		case bsid <= 16: // E-AC-3
			strmtyp, substreamid := b[2]>>6, b[2]>>3&7
			size = ((uint32(b[2]&7)<<8 | uint32(b[3])) + 1) * 2
			if fscod := b[4] >> 6; fscod == 3 {
				fscod2 := b[4] >> 4 & 3
				if fscod2 == 3 {
					return 0, 0, false
				}
				n, sr = 1536, ac3SampleRates[fscod2]/2
			} else {
				n, sr = eac3Blocks[b[4]>>4&3]*256, ac3SampleRates[fscod]
			}
			advances = strmtyp != 1 && substreamid == 0
		default:
			return 0, 0, false
		}
		if uint32(len(b)) < size {
			return 0, 0, false
		}
		if advances {
			samples += n
			if rate == 0 {
				rate = sr
			}
		}
		b = b[size:]
	}
	return samples, rate, rate != 0
}

// mp4FrameGridTS is the grid stride (in the mts timescale) of an MP4 source's
// AC-3 / E-AC-3 track whose sample table is not strictly constant, or 0 to
// keep the table's timing. The stride is the first packet's own duration, and
// it is used only when every duration the table declares lies within a tick
// of it: the table then describes that one frame size, rounded, and the frame
// is the exact figure. A table further off describes something the frame does
// not explain (packets of several sizes, a real gap) and is left alone.
func mp4FrameGridTS(ps *packagingSource, t *outTrack, mts uint32) int64 {
	if t.mkv.Type != mkv.AudioTrack || (t.mkv.Codec != "ac3" && t.mkv.Codec != "eac3") {
		return 0
	}
	ti := int(t.mkv.ID) - 1
	if ti < 0 || ti >= len(ps.mv.tracks) {
		return 0
	}
	tk := &ps.mv.tracks[ti]
	if tk.sttsLo == 0 || len(tk.samples) == 0 {
		return 0
	}
	data, err := readSample(ps.src, tk.samples[0].offset, tk.samples[0].size)
	if err != nil {
		return 0
	}
	samples, rate, ok := ac3PacketSamples(data)
	if !ok {
		return 0
	}
	// The frame in the table's own ticks, rounded down and up.
	ticks := uint64(samples) * uint64(tk.timescale)
	down, up := ticks/uint64(rate), (ticks+uint64(rate)-1)/uint64(rate)
	if uint64(tk.sttsLo)+1 < down || uint64(tk.sttsHi) > up+1 {
		return 0
	}
	stride := uint64(samples) * uint64(mts)
	if stride%uint64(rate) != 0 {
		return 0
	}
	return int64(stride / uint64(rate))
}
