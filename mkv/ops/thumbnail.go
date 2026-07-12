package ops

// thumbnail.go - ExtractKeyframeSample: the mkvgo half of a thumbnail /
// storyboard pipeline. It seeks to the video keyframe nearest a timestamp
// through the Cues (a few bounded reads, no scan) and writes that single
// compressed sample in a form a decoder ingests directly: Annex-B with the
// parameter sets prepended for H.264/HEVC, an IVF wrapper for VP9/AV1.
// Decoding the image stays the consumer's job (mkvgo never transcodes):
//
//	mkvgo extract-frame movie.mkv 00:12:30 -o frame.h264
//	<decoder> -i frame.h264 -frames:v 1 thumb.jpg
//
// Scrubbing storyboards are this in a loop over the keyframe index.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// KeyframeSample is one extracted video keyframe, decoder-ready.
type KeyframeSample struct {
	PtsMs int64  // the keyframe's actual presentation time
	Codec string // canonical codec ("h264", "hevc", "vp9", "av1")
	Ext   string // suggested file extension (".h264", ".hevc", ".ivf")
	Data  []byte // Annex-B (H.264/HEVC, parameter sets prepended) or IVF (VP9/AV1)
}

// ExtractKeyframeSample returns the video keyframe nearest atMs (the cued
// keyframe at or before it; the first one for an atMs before the first cue).
// The read is bounded: metadata + Cues head-only, then one window at the
// cued cluster.
func ExtractKeyframeSample(ctx context.Context, srcPath string, atMs int64, opts ...mkv.Options) (*KeyframeSample, error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs, reader.WithCues())
	if err != nil {
		return nil, err
	}
	if len(c.Cues) == 0 {
		return nil, fmt.Errorf("%s: no Cues index - keyframe extraction seeks through the Cues (run `mkvgo reindex` first)", srcPath)
	}
	var video *mkv.Track
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.VideoTrack {
			video = &c.Tracks[i]
			break
		}
	}
	if video == nil {
		return nil, fmt.Errorf("%s: no video track", srcPath)
	}

	// The cue at or before atMs (cues are video-keyframe-keyed and ascending).
	cue := c.Cues[0]
	for _, cu := range c.Cues {
		if cu.TimeMs > atMs {
			break
		}
		cue = cu
	}

	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	br, err := reader.NewBlockReaderAt(src, c.Info.TimecodeScale, c.SegmentStart+cue.ClusterPos)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no video keyframe found at cue time %dms", cue.TimeMs)
		}
		if err != nil {
			return nil, fmt.Errorf("read block: %w", err)
		}
		if b.TrackNumber != video.ID || !b.Keyframe {
			continue
		}
		data := video.RestoreHeader(b.Data)
		return packKeyframe(video, b.Timecode, data)
	}
}

// packKeyframe wraps one compressed keyframe for a decoder.
func packKeyframe(t *mkv.Track, ptsMs int64, data []byte) (*KeyframeSample, error) {
	ks := &KeyframeSample{PtsMs: ptsMs}
	switch t.Codec {
	case "h264":
		out, err := annexBFromLengthPrefixed(data, avcNALLengthSize(t.CodecPrivate))
		if err != nil {
			return nil, err
		}
		ks.Codec, ks.Ext = "h264", ".h264"
		ks.Data = append(avcParameterSetsAnnexB(t.CodecPrivate), out...)
	case "hevc":
		out, err := annexBFromLengthPrefixed(data, hevcNALLengthSize(t.CodecPrivate))
		if err != nil {
			return nil, err
		}
		ks.Codec, ks.Ext = "hevc", ".hevc"
		ks.Data = append(hevcParameterSetsAnnexB(t.CodecPrivate), out...)
	case "vp9", "vp8":
		ks.Codec, ks.Ext = t.Codec, ".ivf"
		ks.Data = ivfWrap(t, ptsMs, data)
	case "av1":
		ks.Codec, ks.Ext = "av1", ".ivf"
		ks.Data = ivfWrap(t, ptsMs, data)
	default:
		return nil, fmt.Errorf("keyframe extraction supports H.264/HEVC/VP8/VP9/AV1 (track codec %q)", t.Codec)
	}
	return ks, nil
}

// annexBFromLengthPrefixed rewrites length-prefixed NAL units as Annex-B.
func annexBFromLengthPrefixed(data []byte, lenSize int) ([]byte, error) {
	if lenSize < 1 || lenSize > 4 {
		return nil, fmt.Errorf("bad NAL length size %d", lenSize)
	}
	out := make([]byte, 0, len(data)+16)
	for pos := 0; pos < len(data); {
		if pos+lenSize > len(data) {
			return nil, fmt.Errorf("truncated NAL length at %d", pos)
		}
		var n int
		for i := 0; i < lenSize; i++ {
			n = n<<8 | int(data[pos+i])
		}
		pos += lenSize
		if n <= 0 || pos+n > len(data) {
			return nil, fmt.Errorf("NAL size %d out of bounds at %d", n, pos)
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, data[pos:pos+n]...)
		pos += n
	}
	return out, nil
}

// avcNALLengthSize reads the NAL length size from an avcC record.
func avcNALLengthSize(avcC []byte) int {
	if len(avcC) < 5 {
		return 4
	}
	return int(avcC[4]&0x03) + 1
}

// avcParameterSetsAnnexB extracts the SPS/PPS NALs from an avcC as Annex-B.
func avcParameterSetsAnnexB(avcC []byte) []byte {
	var out []byte
	if len(avcC) < 6 {
		return out
	}
	// avcC: …[4]=0xFC|lengthSizeMinusOne, [5]=0xE0|numSPS, SPS entries,
	// 1 byte numPPS, PPS entries.
	numSPS := int(avcC[5] & 0x1F)
	pos := 6
	readSets := func(count int) {
		for i := 0; i < count && pos+2 <= len(avcC); i++ {
			n := int(binary.BigEndian.Uint16(avcC[pos:]))
			pos += 2
			if pos+n > len(avcC) {
				return
			}
			out = append(out, 0, 0, 0, 1)
			out = append(out, avcC[pos:pos+n]...)
			pos += n
		}
	}
	readSets(numSPS)
	if pos < len(avcC) {
		count := int(avcC[pos])
		pos++
		readSets(count) // PPS
	}
	return out
}

// hevcNALLengthSize reads the NAL length size from an hvcC record.
func hevcNALLengthSize(hvcC []byte) int {
	if len(hvcC) < 22 {
		return 4
	}
	return int(hvcC[21]&0x03) + 1
}

// hevcParameterSetsAnnexB extracts the VPS/SPS/PPS arrays from an hvcC.
func hevcParameterSetsAnnexB(hvcC []byte) []byte {
	var out []byte
	if len(hvcC) < 23 {
		return out
	}
	pos := 23
	for a := 0; a < int(hvcC[22]) && pos+3 <= len(hvcC); a++ {
		pos++ // array_completeness + NAL type
		count := int(binary.BigEndian.Uint16(hvcC[pos:]))
		pos += 2
		for i := 0; i < count && pos+2 <= len(hvcC); i++ {
			n := int(binary.BigEndian.Uint16(hvcC[pos:]))
			pos += 2
			if pos+n > len(hvcC) {
				return out
			}
			out = append(out, 0, 0, 0, 1)
			out = append(out, hvcC[pos:pos+n]...)
			pos += n
		}
	}
	return out
}

// ivfWrap wraps one VP8/VP9/AV1 frame in a minimal IVF container (what
// standard decoder tools read as a standalone file).
func ivfWrap(t *mkv.Track, ptsMs int64, frame []byte) []byte {
	fourcc := map[string]string{"vp8": "VP80", "vp9": "VP90", "av1": "AV01"}[t.Codec]
	var w, h uint16
	if t.Width != nil {
		w = uint16(*t.Width)
	}
	if t.Height != nil {
		h = uint16(*t.Height)
	}
	out := make([]byte, 0, 32+12+len(frame))
	out = append(out, 'D', 'K', 'I', 'F', 0, 0, 32, 0)
	out = append(out, fourcc...)
	var buf [20]byte
	binary.LittleEndian.PutUint16(buf[0:], w)
	binary.LittleEndian.PutUint16(buf[2:], h)
	binary.LittleEndian.PutUint32(buf[4:], 1000) // timebase denominator (ms)
	binary.LittleEndian.PutUint32(buf[8:], 1)    // timebase numerator
	binary.LittleEndian.PutUint32(buf[12:], 1)   // frame count
	out = append(out, buf[:16]...)
	binary.LittleEndian.PutUint32(buf[0:], uint32(len(frame)))
	binary.LittleEndian.PutUint64(buf[4:], uint64(ptsMs))
	out = append(out, buf[:12]...)
	return append(out, frame...)
}
