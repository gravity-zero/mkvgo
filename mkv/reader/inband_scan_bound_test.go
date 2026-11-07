package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestInBandScanParameterSetIDsBounded: a PPS id is at most 255 and an SPS id 31 (H.264 7.4.2); the scan kept any
// 32-bit id, so one frame of tiny PPS NALs with distinct ids grew its map by the million.
func TestInBandScanParameterSetIDsBounded(t *testing.T) {
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, false)})}
	s, ok := newScanner(&tr).(*avcScan)
	if !ok {
		t.Fatal("no H.264 scan")
	}
	var frame []byte
	for id := range uint32(3000) {
		frame = append(frame, lenPrefixed4(scanPPS(id, id%64, false))...)
		frame = append(frame, lenPrefixed4(scanSPS{id: id}.nal())...)
	}
	s.feed(frame, false)
	if len(s.pps) > 256 || len(s.sps) > 32 {
		t.Errorf("%d PPS and %d SPS kept, want at most 256 and 32", len(s.pps), len(s.sps))
	}
	for id, p := range s.pps {
		if id > 255 || p.spsID > 31 {
			t.Errorf("PPS %d naming SPS %d kept", id, p.spsID)
		}
	}
}

// TestInBandScanReadsTheSecondFieldOfABlock: a Matroska block holding both fields of a frame, its first one past
// 16 KiB (a 1080i broadcast muxed by mkvmerge, measured), lost its field order once the scan read a block's first
// 16 KiB alone; the walk reads the head of every NAL, so the second field's slice header is read.
func TestInBandScanReadsTheSecondFieldOfABlock(t *testing.T) {
	big := append(scanSlice{field: true, frameNum: 1, lsb: 2}.nal(), bytes.Repeat([]byte{0x55}, 20_000)...)
	pair := au(big, scanSlice{field: true, bottom: true, frameNum: 1, lsb: 3}.nal())
	avcc := scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, false)})
	meta, err := ReadMeta(context.Background(), bytes.NewReader(avcScanMKV(avcc, nil, pair)), "", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if tr := meta.Tracks[0]; tr.ScanType != "interlaced" || tr.FieldOrder != "tt" {
		t.Errorf("got %q/%q, want interlaced/tt", tr.ScanType, tr.FieldOrder)
	}
}

// TestInBandScanBoundsTheNALsOfASample: a block of thousands of tiny NALs costs the walk no more than its first
// maxSampleNALs; a slice past them proves nothing.
func TestInBandScanBoundsTheNALsOfASample(t *testing.T) {
	c := scanCases()[0]
	filler := lenPrefixed4([]byte{0x0C, 0xFF}) // filler data NAL
	var frames [][]byte
	for _, f := range c.frames {
		frames = append(frames, append(bytes.Repeat(filler, maxSampleNALs), f...))
	}
	avcc := scanAvcC([][]byte{c.sps.nal()}, [][]byte{scanPPS(0, c.sps.id, c.bottomPOC)})
	meta, err := ReadMeta(context.Background(), bytes.NewReader(avcScanMKV(avcc, c.video, frames...)), "", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if got := meta.Tracks[0].ScanType; got == "interlaced" {
		t.Errorf("%s: slices past %d NALs proved %q", c.name, maxSampleNALs, got)
	}
}
