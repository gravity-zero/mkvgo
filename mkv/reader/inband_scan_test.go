package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// avcSliceNAL builds an H.264 I-slice NAL (IDR when idr) for the SPS of
// spsPrefixScan: 4-bit frame_num, field_pic_flag present.
func avcSliceNAL(idr, field bool) []byte {
	b := &tbw{}
	b.ue(0) // first_mb_in_slice
	b.ue(7) // slice_type I
	b.ue(0) // pic_parameter_set_id
	b.u(0, 4)
	if field {
		b.u(1, 1) // field_pic_flag
		b.u(0, 1) // bottom_field_flag
	} else {
		b.u(0, 1)
	}
	b.ue(0) // idr_pic_id
	b.u(0xffff, 16)
	hdr := byte(0x41)
	if idr {
		hdr = 0x65
	}
	return append([]byte{hdr}, b.bytes()...)
}

// avcAccessUnit is one length-prefixed access unit: an AUD, then the slice.
func avcAccessUnit(idr, field bool) []byte {
	return append(lenPrefixed4([]byte{0x09, 0xf0}), lenPrefixed4(avcSliceNAL(idr, field))...)
}

// avcScanMKV builds an MKV with one H.264 track (avcC, optional Video element)
// and one cluster holding the given frames as SimpleBlocks.
func avcScanMKV(avcc, video []byte, frames ...[]byte) []byte {
	elems := [][]byte{
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEG4/ISO/AVC"),
		bytesElem(mkv.IDCodecPrivate, avcc),
	}
	if video != nil {
		elems = append(elems, video)
	}
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	for i, f := range frames {
		block := append([]byte{0x81, 0x00, byte(i), 0x00}, f...)
		if i == 0 {
			block[3] = 0x80
		}
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(block)))
		cluster.Write(block)
	}
	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4)))
	seg.Write(masterElem(mkv.IDTracks, trackEntry(elems...)))
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
	seg.Write(cluster.Bytes())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

func TestAVCFieldPicture(t *testing.T) {
	l, ok := avcFieldCodingLayout(buildHighSPSAvcCScan(1, 1, 1, false))
	if !ok {
		t.Fatal("no field-coding layout for an SPS with frame_mbs_only_flag 0")
	}
	if !avcFieldPicture(avcAccessUnit(true, true), l) {
		t.Error("a field-coded slice was not seen")
	}
	if avcFieldPicture(avcAccessUnit(true, false), l) {
		t.Error("a frame-coded slice read as a field")
	}
	if avcFieldPicture([]byte{0, 0, 0, 9, 0x65}, l) {
		t.Error("a length past the frame read as a field")
	}
	if _, ok := avcFieldCodingLayout(buildHighSPSAvcCScan(1, 1, 1, true)); ok {
		t.Error("an SPS coding frames only gave a field-coding layout")
	}
}

// TestAVCFieldCodingLayoutTwoSPS: two SPS that disagree on the slice layout leave
// the slice unreadable, so no layout.
func TestAVCFieldCodingLayoutTwoSPS(t *testing.T) {
	first := buildHighSPSAvcCScan(1, 1, 1, false)[8:] // the SPS NAL after the 6-byte header and its 2-byte length
	w := &tbw{}
	w.u(100, 8) // profile_idc
	w.u(0, 8)
	w.u(40, 8)
	w.ue(1) // seq_parameter_set_id
	w.ue(1) // chroma_format_idc
	w.ue(0)
	w.ue(0)
	w.u(0, 2)
	w.ue(1) // log2_max_frame_num_minus4: a 5-bit frame_num, the first SPS has 4
	w.ue(0)
	w.ue(0)
	w.ue(1)
	w.u(0, 1)
	w.ue(29)
	w.ue(17)
	w.u(0, 2) // frame_mbs_only_flag 0, mb_adaptive_frame_field_flag
	w.u(1, 1)
	w.u(0, 2) // no crop, no VUI
	second := append([]byte{0x67}, w.bytes()...)
	cp := []byte{0x01, 100, 0, 40, 0xFF, 0xE2}
	for _, s := range [][]byte{first, second} {
		cp = append(cp, byte(len(s)>>8), byte(len(s)))
		cp = append(cp, s...)
	}
	if _, ok := avcFieldCodingLayout(cp); ok {
		t.Error("two SPS with different frame_num widths gave a layout")
	}
	if _, ok := avcFieldCodingLayout(append([]byte{0x01, 100, 0, 40, 0xFF, 0xE1}, cp[6:6+2+len(first)]...)); !ok {
		t.Error("the first SPS alone gave no layout")
	}
}

// TestInBandScanFieldPicFlag: with WithInBandColourFallback, an H.264 track whose
// SPS allows field coding is interlaced once a slice is field-coded, and stays
// unknown when every slice read codes a frame.
func TestInBandScanFieldPicFlag(t *testing.T) {
	fieldSPS := buildHighSPSAvcCScan(1, 1, 1, false)
	frame, field := avcAccessUnit(true, false), avcAccessUnit(false, true)
	cases := []struct {
		name   string
		avcc   []byte
		video  []byte
		frames [][]byte
		opt    bool
		scan   string
	}{
		{"field slice on the third frame", fieldSPS, nil, [][]byte{frame, frame, field}, true, "interlaced"},
		{"frames only", fieldSPS, nil, [][]byte{frame, frame, frame}, true, ""},
		{"field slice, option off", fieldSPS, nil, [][]byte{field}, false, ""},
		{"container says progressive", fieldSPS, masterElem(mkv.IDVideo, uintElem(mkv.IDFlagInterlaced, 2, 1)), [][]byte{field}, true, "progressive"},
		{"field slice past the bound", fieldSPS, nil, append(repeatFrame(frame, maxScanSamples), field), true, ""},
	}
	for _, c := range cases {
		data := avcScanMKV(c.avcc, c.video, c.frames...)
		var opts []ReadOption
		if c.opt {
			opts = append(opts, WithInBandColourFallback())
		}
		meta, err := ReadMeta(context.Background(), bytes.NewReader(data), "", opts...)
		if err != nil {
			t.Fatalf("%s: ReadMeta: %v", c.name, err)
		}
		full, err := Read(context.Background(), bytes.NewReader(data), "", opts...)
		if err != nil {
			t.Fatalf("%s: Read: %v", c.name, err)
		}
		for path, ct := range map[string]*mkv.Container{"meta": meta, "full": full} {
			if got := ct.Tracks[0]; got.ScanType != c.scan || (got.ScanType == "interlaced" && got.FieldOrder != "") {
				t.Errorf("%s (%s): ScanType/FieldOrder = %q/%q, want %q", c.name, path, got.ScanType, got.FieldOrder, c.scan)
			}
		}
	}
}

func repeatFrame(f []byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = f
	}
	return out
}

// TestNeedsInBandColourAVCScan: the mp4 driver reads the first sample of an H.264
// track whose scan type the SPS left open, and only that one.
func TestNeedsInBandColourAVCScan(t *testing.T) {
	open := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildHighSPSAvcCScan(1, 1, 1, false)}
	if !NeedsInBandColour(&open) {
		t.Error("an H.264 track with field coding allowed and no scan type needs no sample")
	}
	stated := open
	stated.ScanType = "progressive"
	frames := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildHighSPSAvcCScan(1, 1, 1, true)}
	for _, tr := range []mkv.Track{stated, frames} {
		if NeedsInBandColour(&tr) {
			t.Errorf("scan %q, frames-only SPS: a sample was asked for", tr.ScanType)
		}
	}
	ApplyInBandColour(&open, avcAccessUnit(true, true))
	if open.ScanType != "interlaced" {
		t.Errorf("ApplyInBandColour on a field slice: ScanType = %q", open.ScanType)
	}
}
