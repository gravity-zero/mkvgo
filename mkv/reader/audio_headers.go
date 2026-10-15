package reader

import (
	"encoding/binary"

	"github.com/gravity-zero/mkvgo/mkv"
)

// fillAudioHeaders fills the channel count, sample rate and bit depth a Vorbis, FLAC or Opus track's CodecPrivate
// states when the container leaves them out; container values win per field, like the AAC configuration's.
func fillAudioHeaders(t *mkv.Track) {
	if t.Type != mkv.AudioTrack || (t.Channels != nil && t.SampleRate != nil && t.BitDepth != nil) {
		return
	}
	var ch uint8
	var rate float64
	var bits uint8
	switch t.Codec {
	case "vorbis", "A_VORBIS":
		ch, rate = vorbisIdentification(t.CodecPrivate)
	case "flac", "A_FLAC":
		ch, rate, bits = flacStreamInfo(t.CodecPrivate)
	case "opus", "A_OPUS":
		if cp := t.CodecPrivate; len(cp) >= 10 && string(cp[:8]) == "OpusHead" {
			ch, rate = cp[9], 48000 // the decoder's output rate, whatever the input was sampled at
		}
	default:
		return
	}
	if t.Channels == nil && ch > 0 {
		t.Channels = &ch
	}
	if t.SampleRate == nil && rate > 0 {
		t.SampleRate = &rate
	}
	if t.BitDepth == nil && bits > 0 {
		t.BitDepth = &bits
	}
}

// vorbisIdentification reads the identification header, the first of the three Xiph-laced packets of the CodecPrivate.
func vorbisIdentification(cp []byte) (uint8, float64) {
	if len(cp) < 1 || cp[0] != 2 {
		return 0, 0
	}
	off := 1
	for packet := 0; packet < 2; packet++ { // the lacing sizes of the first two packets, 255-runs
		for {
			if off >= len(cp) {
				return 0, 0
			}
			b := cp[off]
			off++
			if b < 255 {
				break
			}
		}
	}
	if len(cp) < off+16 || cp[off] != 1 || string(cp[off+1:off+7]) != "vorbis" {
		return 0, 0
	}
	return cp[off+11], float64(binary.LittleEndian.Uint32(cp[off+12:]))
}

// flacStreamInfo reads the STREAMINFO block that opens a "fLaC" CodecPrivate.
func flacStreamInfo(cp []byte) (uint8, float64, uint8) {
	if len(cp) < 4+4+18 || string(cp[:4]) != "fLaC" || cp[4]&0x7F != 0 {
		return 0, 0, 0
	}
	b := cp[8:]
	rate := uint32(b[10])<<12 | uint32(b[11])<<4 | uint32(b[12])>>4
	return (b[12]>>1)&7 + 1, float64(rate), (b[12]&1)<<4 | b[13]>>4 + 1
}
