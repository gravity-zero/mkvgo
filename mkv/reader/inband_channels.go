package reader

import (
	"context"
	"io"

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
		return ac3ChannelCount[acmod] + uint8(lfe), true
	case bsid <= 16: // E-AC-3: fscod, numblkscod (or fscod2), acmod, lfeon fill byte 4
		acmod := (f[4] >> 1) & 0x07
		return ac3ChannelCount[acmod] + f[4]&1, true
	}
	return 0, false
}
