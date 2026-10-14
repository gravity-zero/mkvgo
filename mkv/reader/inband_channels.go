package reader

import (
	"context"
	"io"
	"math/bits"

	"github.com/gravity-zero/mkvgo/mkv"
)

// fillStreamChannels sets StreamChannels and ChannelsMismatch on the AC-3, E-AC-3 and AAC tracks from what their stream states.
func fillStreamChannels(ctx context.Context, r io.ReadSeeker, c *mkv.Container) {
	need := map[uint64]*mkv.Track{}
	for i := range c.Tracks {
		t := &c.Tracks[i]
		switch {
		case t.Type != mkv.AudioTrack:
		case isAACCodec(t.Codec) || t.Codec == "aac":
			if ch := ParseAACConfig(t.CodecPrivate).Channels; ch > 0 {
				setStreamChannels(t, ch)
			}
		case t.Codec == "ac3" || t.Codec == "eac3":
			need[t.ID] = t
		}
	}
	if len(need) == 0 {
		return
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return
	}
	br, err := NewBlockReader(r, c.Info.TimecodeScale)
	if err != nil {
		return
	}
	for i := 0; i < maxInBandBlocks && len(need) > 0 && ctx.Err() == nil; i++ {
		blk, err := br.Next()
		if err != nil {
			return
		}
		if t, ok := need[blk.TrackNumber]; ok {
			delete(need, blk.TrackNumber)
			if ch, ok := ac3FrameChannels(blk.Data); ok {
				setStreamChannels(t, ch)
			}
		}
	}
}

func setStreamChannels(t *mkv.Track, ch uint8) {
	t.StreamChannels = &ch
	t.ChannelsMismatch = t.Channels != nil && *t.Channels != ch
}

// ac3ChannelCount is the channels of each AC-3 acmod (1+1 dual mono counts 2), before the LFE.
var ac3ChannelCount = [8]uint8{2, 1, 2, 3, 3, 4, 4, 5}

// ac3FrameChannels reads the channel count of an AC-3 or E-AC-3 frame header: acmod plus lfeon.
func ac3FrameChannels(f []byte) (uint8, bool) {
	if len(f) < 8 || f[0] != 0x0B || f[1] != 0x77 {
		return 0, false
	}
	bsid := f[5] >> 3
	switch {
	case bsid <= 10: // AC-3: acmod opens byte 6, then cmixlev, surmixlev, dsurmod as acmod calls for, then lfeon
		r := &bitReader{data: f[6:]}
		acmod := r.bits(3)
		if acmod&1 != 0 && acmod != 1 {
			r.bits(2)
		}
		if acmod&4 != 0 {
			r.bits(2)
		}
		if acmod == 2 {
			r.bits(2)
		}
		lfe := r.bit()
		if r.err {
			return 0, false
		}
		// An AC-3 frame can be an E-AC-3 track's core, its extra channels in the dependent substreams after it.
		if size := ac3FrameBytes(f); size > 0 && size < len(f) {
			core := eac3AcmodSpeakers[acmod]
			if lfe == 1 {
				core |= spkLFE
			}
			if sp := eac3Speakers(f[size:], false); sp != 0 {
				return speakerChannels(core | sp), true
			}
		}
		return ac3ChannelCount[acmod] + uint8(lfe), true
	case bsid <= 16:
		if sp := eac3Speakers(f, true); sp != 0 {
			return speakerChannels(sp), true
		}
	}
	return 0, false
}

// ac3Bitrates is the bit rate (kbit/s) of each frmsizecod>>1.
var ac3Bitrates = [19]int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}

// ac3FrameBytes is an AC-3 syncframe's size from its fscod and frmsizecod (0 when they are invalid).
func ac3FrameBytes(f []byte) int {
	fscod, code := int(f[4]>>6), int(f[4]&0x3F)
	if code>>1 >= len(ac3Bitrates) {
		return 0
	}
	kbps := ac3Bitrates[code>>1]
	switch fscod {
	case 0: // 48 kHz
		return 4 * kbps
	case 1: // 44.1 kHz: 1536 samples, rounded down to words, odd codes one word more
		return 2 * (kbps*1000*1536/44100/16 + code&1)
	case 2: // 32 kHz
		return 6 * kbps
	}
	return 0
}

// Speaker positions, as bits, for counting an E-AC-3 programme's channels.
const (
	spkL = 1 << iota
	spkC
	spkR
	spkLs
	spkRs
	spkLcRc
	spkLrsRrs
	spkCs
	spkTs
	spkLsdRsd
	spkLwRw
	spkVhlVhr
	spkVhc
	spkLtsRts
	spkLFE2
	spkLFE
	spkDualMono
)

// eac3AcmodSpeakers are each acmod's positions (1+1 dual mono: two channels).
var eac3AcmodSpeakers = [8]uint32{spkL | spkDualMono, spkC, spkL | spkR, spkL | spkC | spkR, spkL | spkR | spkCs, spkL | spkC | spkR | spkCs, spkL | spkR | spkLs | spkRs, spkL | spkC | spkR | spkLs | spkRs}

// eac3ChanmapSpeakers are the positions of each chanmap bit, most significant first (ETSI TS 102 366 table E.1.4).
var eac3ChanmapSpeakers = [16]uint32{spkL, spkC, spkR, spkLs, spkRs, spkLcRc, spkLrsRrs, spkCs, spkTs, spkLsdRsd, spkLwRw, spkVhlVhr, spkVhc, spkLtsRts, spkLFE2, spkLFE}

// eac3PairSpeakers are the positions that stand for two channels.
const eac3PairSpeakers = spkLcRc | spkLrsRrs | spkLsdRsd | spkLwRw | spkVhlVhr | spkLtsRts | spkDualMono

// eac3Speakers gathers the speakers of the E-AC-3 access unit opening f: its
// independent substream's (acmod, lfeon) when independent is set, and those
// the dependent substreams right after it add (a 7.1 is a 5.1 core and a
// dependent with the back pair), up to the next independent substream.
func eac3Speakers(f []byte, independent bool) uint32 {
	var speakers uint32
	for off, first := 0, independent; off+8 <= len(f) && f[off] == 0x0B && f[off+1] == 0x77; first = false {
		r := &bitReader{data: f[off+2:]}
		strmtyp := r.bits(2)
		substream := r.bits(3)
		size := (int(r.bits(11)) + 1) * 2
		if fscod := r.bits(2); fscod == 3 {
			r.bits(2) // fscod2
		} else {
			r.bits(2) // numblkscod
		}
		acmod := r.bits(3)
		lfe := r.bit()
		if bsid := r.bits(5); bsid <= 10 || bsid > 16 || r.err {
			break
		}
		if (strmtyp == 1) == first || substream != 0 {
			break // only the first programme: its independent substream, then its dependents
		}
		unit := eac3AcmodSpeakers[acmod]
		if lfe == 1 {
			unit |= spkLFE
		}
		if strmtyp == 1 { // a dependent substream: its chanmap, when it has one, places its channels
			r.bits(5) // dialnorm
			if r.bit() == 1 {
				r.bits(8) // compr
			}
			if acmod == 0 {
				r.bits(5) // dialnorm2
				if r.bit() == 1 {
					r.bits(8) // compr2
				}
			}
			if r.bit() == 1 { // chanmape
				chanmap := r.bits(16)
				unit = 0
				for i, sp := range eac3ChanmapSpeakers {
					if chanmap&(1<<(15-i)) != 0 {
						unit |= sp
					}
				}
			}
			if r.err {
				break
			}
		}
		speakers |= unit
		off += size
	}
	return speakers
}

// speakerChannels counts the channels of a set of speaker positions, a pair counting two.
func speakerChannels(speakers uint32) uint8 {
	return uint8(bits.OnesCount32(speakers&^spkDualMono) + bits.OnesCount32(speakers&eac3PairSpeakers))
}
