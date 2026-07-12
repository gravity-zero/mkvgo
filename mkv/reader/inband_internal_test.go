package reader

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// seekStartFails answers position queries (Seek 0,SeekCurrent) but errors on any
// real Seek/Read, to drive the I/O-error branches of the in-band/tail helpers.
type seekStartFails struct{}

func (seekStartFails) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (seekStartFails) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekCurrent && off == 0 {
		return 0, nil
	}
	return 0, io.ErrClosedPipe
}

func u16ptr(v uint16) *uint16 { return &v }

func TestNeedsInBandColour(t *testing.T) {
	full := mustHex(t, hevcHDRPrivateHex) // hvcC WITH SPS arrays
	cases := []struct {
		name string
		t    mkv.Track
		want bool
	}{
		{"bare hvcC, no colour", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: bareHvcC()}, true},
		{"short codec id", mkv.Track{Type: mkv.VideoTrack, Codec: "V_MPEGH/ISO/HEVC", CodecPrivate: bareHvcC()}, true},
		{"audio track", mkv.Track{Type: mkv.AudioTrack, Codec: "hevc", CodecPrivate: bareHvcC()}, false},
		{"non-hevc", mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: bareHvcC()}, false},
		{"already has transfer", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: bareHvcC(), ColorTransfer: u16ptr(16)}, false},
		{"already has primaries", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: bareHvcC(), ColorPrimaries: u16ptr(9)}, false},
		{"hvcC carries an SPS", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: full}, false},
		{"too-short hvcC", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3}}, false},
		{"no codec private", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc"}, false},
	}
	for _, c := range cases {
		if got := NeedsInBandColour(&c.t); got != c.want {
			t.Errorf("%s: NeedsInBandColour = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsHEVCCodec(t *testing.T) {
	for _, c := range []struct {
		codec string
		want  bool
	}{{"hevc", true}, {"h265", true}, {"V_MPEGH/ISO/HEVC", true}, {"h264", false}, {"av1", false}, {"", false}} {
		if got := isHEVCCodec(c.codec); got != c.want {
			t.Errorf("isHEVCCodec(%q) = %v, want %v", c.codec, got, c.want)
		}
	}
}

func TestApplyInBandColour(t *testing.T) {
	sps := extractHEVCSPSNAL(t, mustHex(t, hevcHDRPrivateHex))

	// SPS only → colour from the VUI (PQ).
	tr := mkv.Track{CodecPrivate: bareHvcC()}
	ApplyInBandColour(&tr, lenPrefixed4(sps))
	if tr.ColorTransferName() != "smpte2084" || tr.ColorSpaceName() != "bt2020nc" {
		t.Errorf("SPS-only: transfer=%q space=%q, want smpte2084/bt2020nc", tr.ColorTransferName(), tr.ColorSpaceName())
	}

	// SPS + ATC SEI → transfer overridden to HLG.
	tr2 := mkv.Track{CodecPrivate: bareHvcC()}
	ApplyInBandColour(&tr2, append(lenPrefixed4(sps), lenPrefixed4(atcSEINAL(18))...))
	if tr2.ColorTransferName() != "arib-std-b67" {
		t.Errorf("SPS+ATC SEI: transfer=%q, want arib-std-b67", tr2.ColorTransferName())
	}

	// No SPS in the frame → colour stays nil, no panic.
	tr3 := mkv.Track{CodecPrivate: bareHvcC()}
	ApplyInBandColour(&tr3, []byte{0, 0, 0, 1, 0x00})
	if tr3.ColorTransfer != nil {
		t.Errorf("no SPS: transfer = %v, want nil", tr3.ColorTransfer)
	}
}

func TestATCFromSEI(t *testing.T) {
	cases := []struct {
		name string
		rbsp []byte
		want uint16
		ok   bool
	}{
		{"atc present", []byte{147, 0x01, 18, 0x80}, 18, true},
		{"ff-coded non-atc type", []byte{0xff, 0x00, 0x01, 0xaa, 0x80}, 0, false}, // type 255
		{"other payload type", []byte{136, 0x02, 0xaa, 0xbb, 0x80}, 0, false},
		{"atc after another message", []byte{136, 0x01, 0xaa, 147, 0x01, 16, 0x80}, 16, true},
		{"size zero", []byte{147, 0x00, 0x80}, 0, false},
		{"truncated type", []byte{147}, 0, false},
		{"truncated payload", []byte{147, 0x04, 0x01}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, c := range cases {
		got, ok := atcFromSEI(c.rbsp)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: atcFromSEI = (%d,%v), want (%d,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestReadSEIValue(t *testing.T) {
	cases := []struct {
		b    []byte
		want int
		ok   bool
		next int
	}{
		{[]byte{0x05}, 5, true, 1},
		{[]byte{0xff, 0xff, 0x03}, 513, true, 3},
		{[]byte{0xff}, 0, false, 1},
		{nil, 0, false, 0},
	}
	for _, c := range cases {
		i := 0
		got, ok := readSEIValue(c.b, &i)
		if got != c.want || ok != c.ok {
			t.Errorf("readSEIValue(%v) = (%d,%v), want (%d,%v)", c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestFirstHEVCNAL(t *testing.T) {
	sps := []byte{0x42, 0x01, 0xaa} // nal_unit_type 33 (SPS)
	vps := []byte{0x40, 0x01, 0xbb} // nal_unit_type 32 (VPS)
	frame := append(lenPrefixed4(vps), lenPrefixed4(sps)...)

	if got := firstHEVCNAL(frame, 4, 33); string(got) != string(sps) {
		t.Errorf("firstHEVCNAL type 33 = %x, want %x", got, sps)
	}
	if got := firstHEVCNAL(frame, 4, 39); got != nil {
		t.Errorf("firstHEVCNAL type 39 = %x, want nil", got)
	}
	if got := firstHEVCNAL(frame, 0, 33); got != nil {
		t.Error("nalLen 0 must yield nil")
	}
	if got := firstHEVCNAL(frame, 5, 33); got != nil {
		t.Error("nalLen 5 must yield nil")
	}
	// Length field overruns the buffer → nil.
	if got := firstHEVCNAL([]byte{0x00, 0x00, 0x00, 0x40, 0x42, 0x01}, 4, 33); got != nil {
		t.Error("overrunning NAL length must yield nil")
	}
}

func TestTilesToEndAndAnchor(t *testing.T) {
	cues := cuesElem(3)
	tags := tagsElem("ENCODER", "x")
	tail := append(append([]byte{}, cues...), tags...)

	if !tilesToEnd(tail, 0, int64(len(tail))) {
		t.Error("Cues+Tags must tile exactly to end")
	}
	// Claiming a longer end → does not tile (gap).
	if tilesToEnd(tail, 0, int64(len(tail)+8)) {
		t.Error("a gap before end must fail tiling")
	}
	// A non-segment-level element at the front → not a tail.
	notTail := append(uintElem(mkv.IDPixelWidth, 1, 1), tags...)
	if tilesToEnd(notTail, 0, int64(len(notTail))) {
		t.Error("a non-segment-level lead element must fail tiling")
	}

	// tailAnchor finds the Cues offset after some leading filler bytes.
	buf := append([]byte{0xDE, 0xAD, 0xBE, 0xEF}, tail...)
	off, ok := tailAnchor(buf, 100, 100+int64(len(buf)))
	if !ok || off != 4 {
		t.Errorf("tailAnchor = (%d,%v), want (4,true)", off, ok)
	}
	if _, ok := tailAnchor([]byte{0x01, 0x02, 0x03}, 0, 3); ok {
		t.Error("no Cues magic → tailAnchor must report not-found")
	}
}

func TestMinOffsetAtOrAfter(t *testing.T) {
	offs := []int64{10, 300, 50, 200}
	if got, ok := minOffsetAtOrAfter(offs, 100); !ok || got != 200 {
		t.Errorf("minOffsetAtOrAfter(100) = (%d,%v), want (200,true)", got, ok)
	}
	if _, ok := minOffsetAtOrAfter(offs, 400); ok {
		t.Error("no offset past floor → ok must be false")
	}
	if _, ok := minOffsetAtOrAfter(nil, 0); ok {
		t.Error("empty offsets → ok must be false")
	}
}

func TestIsTailElement(t *testing.T) {
	for _, id := range []uint32{mkv.IDVoid, mkv.IDCues, mkv.IDTags, mkv.IDInfo, mkv.IDTracks, mkv.IDChapters, mkv.IDAttachments, mkv.IDSeekHead} {
		if !isTailElement(id) {
			t.Errorf("isTailElement(0x%X) = false, want true", id)
		}
	}
	if isTailElement(mkv.IDPixelWidth) {
		t.Error("a non-tail element must be rejected")
	}
}

// TestParseTailBufferAllElements drives parseTailBuffer over a tail holding every
// element type its switch handles (plus Void → default), so all branches run.
func TestParseTailBufferAllElements(t *testing.T) {
	var tail []byte
	for _, e := range [][]byte{
		cuesElem(1),
		tagsElem("ENCODER", "x"),
		masterElem(mkv.IDChapters),
		masterElem(mkv.IDAttachments),
		masterElem(mkv.IDInfo),
		masterElem(mkv.IDTracks),
		voidElem(2), // default case (skip)
	} {
		tail = append(tail, e...)
	}
	p := &parser{metaBudget: maxMetadataBytes}
	c := &mkv.Container{}
	if err := p.parseTailBuffer(c, tail); err != nil {
		t.Fatalf("parseTailBuffer: %v", err)
	}
	if len(c.Cues) != 1 || len(c.Tags) != 1 {
		t.Errorf("cues=%d tags=%d, want 1/1", len(c.Cues), len(c.Tags))
	}
}

func TestScanTailForCuesDirect(t *testing.T) {
	// 1 KiB of zero "cluster" bytes (no Cues magic), then a real tail.
	tail := append(cuesElem(3), tagsElem("E", "x")...)
	data := append(make([]byte, 1024), tail...)

	p := &parser{r: bytes.NewReader(data), metaBudget: maxMetadataBytes}
	c := &mkv.Container{}
	done, err := p.scanTailForCues(c)
	if err != nil || !done || len(c.Cues) != 3 || len(c.Tags) != 1 {
		t.Fatalf("scan = (%v,%v) cues=%d tags=%d, want done/3/1", done, err, len(c.Cues), len(c.Tags))
	}

	// No Cues at the tail → not found, defer to the walk.
	p2 := &parser{r: bytes.NewReader(make([]byte, 600)), metaBudget: maxMetadataBytes}
	if done, err := p2.scanTailForCues(&mkv.Container{}); done || err != nil {
		t.Errorf("no-Cues scan = (%v,%v), want (false,nil)", done, err)
	}

	// Positioned at EOF → nothing to scan.
	r3 := bytes.NewReader(data)
	r3.Seek(0, io.SeekEnd)
	p3 := &parser{r: r3, metaBudget: maxMetadataBytes}
	if done, _ := p3.scanTailForCues(&mkv.Container{}); done {
		t.Error("scan from EOF must be a no-op")
	}
}

func TestOffsetIsSegmentElement(t *testing.T) {
	data := append([]byte{0xDE, 0xAD, 0xBE, 0xEF}, cuesElem(1)...) // Cues at offset 4
	p := &parser{r: bytes.NewReader(data)}

	if ok, err := p.offsetIsSegmentElement(4); err != nil || !ok {
		t.Errorf("offset 4 (Cues) = (%v,%v), want (true,nil)", ok, err)
	}
	if ok, _ := p.offsetIsSegmentElement(0); ok {
		t.Error("offset 0 (non-segment-level bytes) must be false")
	}
	if ok, _ := p.offsetIsSegmentElement(int64(len(data))); ok {
		t.Error("offset at EOF (undecodable) must be false")
	}
}

func TestInBandColourErrorPaths(t *testing.T) {
	var sf seekStartFails

	// scanTailForCues: the SeekEnd to find the file end fails.
	p := &parser{r: sf, metaBudget: maxMetadataBytes}
	if done, err := p.scanTailForCues(&mkv.Container{}); done || err == nil {
		t.Errorf("scanTailForCues with failing SeekEnd = (%v,%v), want (false,err)", done, err)
	}

	// offsetIsSegmentElement: the seek to the candidate offset fails.
	if ok, err := p.offsetIsSegmentElement(10); ok || err == nil {
		t.Errorf("offsetIsSegmentElement with failing seek = (%v,%v), want (false,err)", ok, err)
	}

	// fillColourFromFirstSample: a track needs it but the rewind seek fails - must
	// leave colour nil without panicking.
	vid := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: bareHvcC()}
	c := &mkv.Container{Tracks: []mkv.Track{vid}}
	fillColourFromFirstSample(context.Background(), sf, c)
	if c.Tracks[0].ColorTransfer != nil {
		t.Error("seek failure must leave colour nil")
	}

	// fillColourFromFirstSample: rewind succeeds but the file is not a valid MKV,
	// so NewBlockReader fails - same graceful no-op.
	c2 := &mkv.Container{Tracks: []mkv.Track{vid}}
	fillColourFromFirstSample(context.Background(), bytes.NewReader([]byte{0, 0, 0, 0}), c2)
	if c2.Tracks[0].ColorTransfer != nil {
		t.Error("invalid MKV must leave colour nil")
	}
}
