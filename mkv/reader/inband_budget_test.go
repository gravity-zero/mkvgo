package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// interleavedMKV builds an MKV with H.264 track 1 and audio track 2 in one
// cluster: before each video frame, audioPer audio blocks, and tail more after
// the last one.
func interleavedMKV(avcc []byte, audioPer, tail int, frames ...[]byte) []byte {
	video := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEG4/ISO/AVC"),
		bytesElem(mkv.IDCodecPrivate, avcc),
	)
	audio := trackEntry(
		uintElem(mkv.IDTrackNumber, 2, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_PCM/INT/LIT"),
	)
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	write := func(track byte, data []byte, key bool) {
		block := append([]byte{0x80 | track, 0x00, 0x00, 0x00}, data...)
		if key {
			block[3] = 0x80
		}
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(block)))
		cluster.Write(block)
	}
	for i, f := range frames {
		for range audioPer {
			write(2, []byte{0, 0, 0, 0}, true)
		}
		write(1, f, i == 0)
	}
	for range tail {
		write(2, []byte{0, 0, 0, 0}, true)
	}
	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4)))
	seg.Write(masterElem(mkv.IDTracks, append(video, audio...)))
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
	seg.Write(cluster.Bytes())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

// TestInBandScanBudgetPerTrack: the audio blocks between video samples do not
// eat the video's maxScanSamples, so a proof in the 10th video sample behind
// 14 audio blocks each (150 blocks in all) is found; a video sample behind
// maxInBandBlocks blocks of other tracks in a row is not waited for, and a
// track given up keeps what its samples proved.
func TestInBandScanBudgetPerTrack(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, false)})
	plain := au(scanSlice{lsb: 2}.nal())
	proof := au(scanSlice{field: true, frameNum: 1, lsb: 2}.nal(), scanSlice{field: true, bottom: true, frameNum: 1, lsb: 3}.nal())
	cases := []struct {
		name        string
		audioPer    int
		tail        int
		frames      [][]byte
		scan, order string
	}{
		{"proof in the 10th video sample, 14 audio blocks before each", 14, 0, append(repeatFrame(plain, 9), proof), "interlaced", "tt"},
		{"proof behind one audio block less than the bound", maxInBandBlocks - 1, 0, [][]byte{proof}, "interlaced", "tt"},
		{"proof behind as many audio blocks as the bound", maxInBandBlocks, 0, [][]byte{proof}, "", ""},
		{"proof, then the video stops: its verdict stands", 0, 2 * maxInBandBlocks, [][]byte{proof}, "interlaced", "tt"},
	}
	for _, c := range cases {
		data := interleavedMKV(avcc, c.audioPer, c.tail, c.frames...)
		for path, read := range map[string]func() (*mkv.Container, error){
			"meta": func() (*mkv.Container, error) {
				return ReadMeta(context.Background(), bytes.NewReader(data), "", WithInBandColourFallback())
			},
			"full": func() (*mkv.Container, error) {
				return Read(context.Background(), bytes.NewReader(data), "", WithInBandColourFallback())
			},
		} {
			ct, err := read()
			if err != nil {
				t.Fatalf("%s (%s): %v", c.name, path, err)
			}
			if got := ct.Tracks[0]; got.ScanType != c.scan || got.FieldOrder != c.order {
				t.Errorf("%s (%s): ScanType/FieldOrder = %q/%q, want %q/%q", c.name, path, got.ScanType, got.FieldOrder, c.scan, c.order)
			}
		}
	}
}
