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

// TestInBandScanReadsASampleHead: the MP4 scan reads each sample's first 16 KiB, where the SEI and the first slice
// header sit; the Matroska one walked the whole block, up to 64 MiB of NALs. Both read the head alone: a slice past
// it proves nothing.
func TestInBandScanReadsASampleHead(t *testing.T) {
	var c scanCase
	for _, sc := range scanCases() {
		if sc.scan == "interlaced" && !sc.noOpt && sc.video == nil {
			c = sc
			break
		}
	}
	if c.scan == "" {
		t.Fatal("no interlaced case")
	}
	filler := lenPrefixed4(append([]byte{0x0C}, make([]byte, 20_000)...)) // filler data NAL, past the 16 KiB head
	var frames [][]byte
	for _, f := range c.frames {
		frames = append(frames, append(append([]byte{}, filler...), f...))
	}
	avcc := scanAvcC([][]byte{c.sps.nal()}, [][]byte{scanPPS(0, c.sps.id, c.bottomPOC)})
	meta, err := ReadMeta(context.Background(), bytes.NewReader(avcScanMKV(avcc, c.video, frames...)), "", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if got := meta.Tracks[0].ScanType; got == "interlaced" {
		t.Errorf("%s: slices past each sample's first 16 KiB proved %q", c.name, got)
	}
}
