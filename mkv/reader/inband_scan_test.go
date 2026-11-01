package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func (w *tbw) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

// scanSPS describes the H.264 SPS the scan fixtures build: 4-bit frame_num, a
// 4-bit POC lsb for pic_order_cnt_type 0, frame_mbs_only_flag 0.
type scanSPS struct {
	id        uint32
	pocType   uint32
	frameOnly bool
	mbaff     bool
	picStruct bool
	hrd       bool // an NAL HRD: pic timing opens on an 8-bit and a 5-bit delay
}

func (o scanSPS) nal() []byte {
	b := &tbw{}
	b.u(100, 8)
	b.u(0, 8)
	b.u(40, 8)
	b.ue(o.id)
	b.ue(1) // chroma_format_idc
	b.ue(0)
	b.ue(0)
	b.u(0, 2)
	b.ue(0) // log2_max_frame_num_minus4
	b.ue(o.pocType)
	switch o.pocType {
	case 0:
		b.ue(0) // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		b.u(0, 1) // delta_pic_order_always_zero_flag
		b.se(0)   // offset_for_non_ref_pic
		b.se(0)   // offset_for_top_to_bottom_field
		b.ue(0)   // num_ref_frames_in_pic_order_cnt_cycle
	}
	b.ue(1)
	b.u(0, 1)
	b.ue(29)
	b.ue(17)
	if o.frameOnly {
		b.u(1, 1)
	} else {
		b.u(0, 1)
		b.u(boolBit(o.mbaff), 1)
	}
	b.u(1, 1) // direct_8x8_inference_flag
	b.u(0, 1) // frame_cropping_flag
	b.u(1, 1) // vui_parameters_present_flag
	b.u(0, 3) // aspect, overscan, video_signal
	b.u(0, 2) // chroma_loc, timing
	b.u(boolBit(o.hrd), 1)
	if o.hrd {
		b.ue(0)    // cpb_cnt_minus1
		b.u(0, 8)  // scales
		b.ue(0)    // bit_rate_value_minus1
		b.ue(0)    // cpb_size_value_minus1
		b.u(0, 1)  // cbr_flag
		b.u(23, 5) // initial_cpb_removal_delay_length_minus1
		b.u(7, 5)  // cpb_removal_delay_length_minus1: 8 bits
		b.u(4, 5)  // dpb_output_delay_length_minus1: 5 bits
		b.u(24, 5) // time_offset_length
	}
	b.u(0, 1) // vcl_hrd_parameters_present_flag
	if o.hrd {
		b.u(0, 1) // low_delay_hrd_flag
	}
	b.u(boolBit(o.picStruct), 1)
	b.u(0, 1) // bitstream_restriction_flag
	b.u(1, 1) // rbsp_stop_one_bit
	return append([]byte{0x67}, b.bytes()...)
}

func boolBit(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}

func scanPPS(id, spsID uint32, bottomPOC bool) []byte {
	b := &tbw{}
	b.ue(id)
	b.ue(spsID)
	b.u(0, 1) // entropy_coding_mode_flag
	b.u(boolBit(bottomPOC), 1)
	b.ue(0)   // num_slice_groups_minus1
	b.u(1, 1) // stop bit
	return append([]byte{0x68}, b.bytes()...)
}

// scanAvcC builds an avcC holding the given SPS and PPS NALs.
func scanAvcC(sps, pps [][]byte) []byte {
	cp := []byte{0x01, 100, 0, 40, 0xFF, 0xE0 | byte(len(sps))}
	for _, n := range sps {
		cp = append(cp, byte(len(n)>>8), byte(len(n)))
		cp = append(cp, n...)
	}
	cp = append(cp, byte(len(pps)))
	for _, n := range pps {
		cp = append(cp, byte(len(n)>>8), byte(len(n)))
		cp = append(cp, n...)
	}
	return cp
}

// scanSlice is the head of a first slice (first_mb_in_slice 0).
type scanSlice struct {
	idr, field, bottom bool
	pps, frameNum, lsb uint32
	pocType            uint32
	bottomPOC          bool
	delta              int32 // delta_pic_order_cnt_bottom (type 0) or delta_pic_order_cnt[1] (type 1)
}

func (o scanSlice) nal() []byte {
	b := &tbw{}
	b.ue(0) // first_mb_in_slice
	b.ue(7) // slice_type I
	b.ue(o.pps)
	b.u(o.frameNum, 4)
	b.u(boolBit(o.field), 1)
	if o.field {
		b.u(boolBit(o.bottom), 1)
	}
	if o.idr {
		b.ue(0) // idr_pic_id
	}
	switch o.pocType {
	case 0:
		b.u(o.lsb, 4)
		if o.bottomPOC && !o.field {
			b.se(o.delta)
		}
	case 1:
		b.se(0)
		if o.bottomPOC && !o.field {
			b.se(o.delta)
		}
	}
	b.u(0xffff, 16)
	hdr := byte(0x41)
	if o.idr {
		hdr = 0x65
	}
	return append([]byte{hdr}, b.bytes()...)
}

// picTimingSEI builds a picture timing SEI NAL: the two HRD delays when hrd,
// pic_struct, and a first clock timestamp carrying ct_type when ct >= 0.
func picTimingSEI(hrd bool, picStruct uint32, ct int) []byte {
	b := &tbw{}
	if hrd {
		b.u(0, 8) // cpb_removal_delay: zero, so a reader that skips no delay sees pic_struct 0 and no timestamp
		b.u(0, 5) // dpb_output_delay
	}
	b.u(picStruct, 4)
	if ct >= 0 {
		b.u(1, 1)
		b.u(uint32(ct), 2)
	} else {
		b.u(0, 1)
	}
	b.u(0, 8)
	payload := b.bytes()
	nal := []byte{0x06, 0x01, byte(len(payload))}
	nal = append(nal, payload...)
	return append(nal, 0x80)
}

// au length-prefixes NALs into one access unit.
func au(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, lenPrefixed4(n)...)
	}
	return out
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

func repeatFrame(f []byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = f
	}
	return out
}

// scanCase is one track: its parameter sets, its samples, and the verdict.
type scanCase struct {
	name        string
	sps         scanSPS
	bottomPOC   bool
	video       []byte
	frames      [][]byte
	noOpt       bool
	scan, order string
}

func scanCases() []scanCase {
	base := scanSPS{}
	mbaffPS := scanSPS{mbaff: true, picStruct: true}
	frame := func(lsb uint32) []byte {
		return au(scanSlice{idr: lsb == 0, lsb: lsb}.nal())
	}
	frameDelta := func(d int32) []byte {
		return au(scanSlice{lsb: 2, bottomPOC: true, delta: d}.nal())
	}
	fields := func(firstBottom bool, lsbFirst, lsbSecond uint32) []byte {
		return au(scanSlice{field: true, bottom: firstBottom, frameNum: 1, lsb: lsbFirst}.nal(),
			scanSlice{field: true, bottom: !firstBottom, frameNum: 1, lsb: lsbSecond}.nal())
	}
	withSEI := func(hrd bool, ps uint32, ct int) []byte {
		return au(picTimingSEI(hrd, ps, ct), scanSlice{lsb: 2}.nal())
	}
	return []scanCase{
		{"field pair, top POC first", base, false, nil, [][]byte{frame(0), fields(false, 2, 3)}, false, "interlaced", "tt"},
		{"field pair, bottom POC first", base, false, nil, [][]byte{frame(0), fields(true, 2, 3)}, false, "interlaced", "bb"},
		{"field pair coded bottom then top, top shown first", base, false, nil, [][]byte{fields(true, 3, 2)}, false, "interlaced", "tt"},
		{"one field, its pair in no sample", base, false, nil, [][]byte{au(scanSlice{field: true, lsb: 2}.nal())}, false, "interlaced", ""},
		{"frame, bottom field later", base, true, nil, [][]byte{frameDelta(1)}, false, "interlaced", "tt"},
		{"frame, bottom field earlier", base, true, nil, [][]byte{frameDelta(-1)}, false, "interlaced", "bb"},
		{"frames whose fields share an instant", base, true, nil, [][]byte{frameDelta(0), frameDelta(0)}, false, "", ""},
		{"orders that disagree", base, true, nil, [][]byte{frameDelta(1), frameDelta(-1)}, false, "interlaced", ""},
		{"MBAFF frames, same instant, no SEI", scanSPS{mbaff: true}, true, nil, [][]byte{frameDelta(0)}, false, "", ""},
		{"frames only", base, false, nil, [][]byte{frame(0), frame(2), frame(4)}, false, "", ""},
		{"pic_struct top-bottom on MBAFF", mbaffPS, false, nil, [][]byte{withSEI(false, 3, -1), withSEI(false, 3, -1)}, false, "interlaced", "tt"},
		{"pic_struct bottom-top on MBAFF", mbaffPS, false, nil, [][]byte{withSEI(false, 4, -1)}, false, "interlaced", "bb"},
		{"pic_struct top-bottom, soft telecine", mbaffPS, false, nil, [][]byte{withSEI(false, 3, -1), withSEI(false, 5, -1)}, false, "", ""},
		{"pic_struct top-bottom, ct_type progressive", mbaffPS, false, nil, [][]byte{withSEI(false, 3, 0)}, false, "", ""},
		{"pic_struct top-bottom without MBAFF", scanSPS{picStruct: true}, false, nil, [][]byte{withSEI(false, 3, -1)}, false, "", ""},
		{"pic_struct top-bottom, ct_type interlaced", scanSPS{picStruct: true}, false, nil, [][]byte{withSEI(false, 3, 1)}, false, "interlaced", "tt"},
		{"pic_struct frame, ct_type interlaced", scanSPS{picStruct: true}, false, nil, [][]byte{withSEI(false, 0, 1)}, false, "interlaced", ""},
		{"pic_struct field", scanSPS{picStruct: true}, false, nil, [][]byte{withSEI(false, 1, -1)}, false, "interlaced", ""},
		{"pic_struct field after HRD delays", scanSPS{picStruct: true, hrd: true}, false, nil, [][]byte{withSEI(true, 2, -1)}, false, "interlaced", ""},
		{"pic_struct frame", scanSPS{picStruct: true, mbaff: true}, false, nil, [][]byte{withSEI(false, 0, -1)}, false, "", ""},
		// An x264 MBAFF stream whose frames change field order, measured: delta_pic_order_cnt_bottom -1 throughout, pic_struct 3 then 4.
		{"pic_struct order beats the POC", mbaffPS, true, nil, repeatFrame(au(picTimingSEI(false, 3, -1),
			scanSlice{lsb: 2, bottomPOC: true, delta: -1}.nal()), 3), false, "interlaced", "tt"},
		{"POC type 2, fields decoded bottom first", scanSPS{pocType: 2}, false, nil, [][]byte{au(
			scanSlice{field: true, bottom: true, frameNum: 1, pocType: 2}.nal(),
			scanSlice{field: true, frameNum: 1, pocType: 2}.nal())}, false, "interlaced", "bb"},
		{"POC type 1, bottom later", scanSPS{pocType: 1}, true, nil, [][]byte{au(scanSlice{pocType: 1, bottomPOC: true, delta: 1}.nal())}, false, "interlaced", "tt"},
		{"option off", base, false, nil, [][]byte{fields(false, 2, 3)}, true, "", ""},
		{"container says progressive", base, false, masterElem(mkv.IDVideo, uintElem(mkv.IDFlagInterlaced, 2, 1)), [][]byte{fields(false, 2, 3)}, false, "progressive", "progressive"},
		{"proof past the bound", base, false, nil, append(repeatFrame(frame(2), maxScanSamples), fields(false, 2, 3)), false, "", ""},
		{"frame_mbs_only", scanSPS{frameOnly: true}, false, nil, [][]byte{frame(0)}, false, "progressive", "progressive"},
	}
}

// TestInBandScan reads each case through the metadata and the full reader with
// WithInBandColourFallback.
func TestInBandScan(t *testing.T) {
	for _, c := range scanCases() {
		avcc := scanAvcC([][]byte{c.sps.nal()}, [][]byte{scanPPS(0, c.sps.id, c.bottomPOC)})
		data := avcScanMKV(avcc, c.video, c.frames...)
		var opts []ReadOption
		if !c.noOpt {
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
			if got := ct.Tracks[0]; got.ScanType != c.scan || got.FieldOrder != c.order {
				t.Errorf("%s (%s): ScanType/FieldOrder = %q/%q, want %q/%q", c.name, path, got.ScanType, got.FieldOrder, c.scan, c.order)
			}
		}
	}
}

// TestInBandScanParameterSets: the slice is read with the SPS its PPS names, so
// two SPS with different layouts are fine; parameter sets may come in-band; a
// slice whose PPS is nowhere proves nothing.
func TestInBandScanParameterSets(t *testing.T) {
	sps0 := scanSPS{id: 0, pocType: 2}.nal()
	sps1 := scanSPS{id: 1}.nal()
	field := au(scanSlice{pps: 1, field: true, frameNum: 1, lsb: 2}.nal(), scanSlice{pps: 1, field: true, bottom: true, frameNum: 1, lsb: 3}.nal())
	cases := []struct {
		name        string
		avcc        []byte
		frame       []byte
		scan, order string
	}{
		{"PPS 1 names SPS 1", scanAvcC([][]byte{sps0, sps1}, [][]byte{scanPPS(0, 0, false), scanPPS(1, 1, false)}), field, "interlaced", "tt"},
		{"PPS in-band", scanAvcC([][]byte{sps1}, nil), append(au(scanPPS(1, 1, false)), field...), "interlaced", "tt"},
		{"no PPS anywhere", scanAvcC([][]byte{sps1}, nil), field, "", ""},
	}
	for _, c := range cases {
		meta, err := ReadMeta(context.Background(), bytes.NewReader(avcScanMKV(c.avcc, nil, c.frame)), "", WithInBandColourFallback())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := meta.Tracks[0]; got.ScanType != c.scan || got.FieldOrder != c.order {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want %q/%q", c.name, got.ScanType, got.FieldOrder, c.scan, c.order)
		}
	}
}

// TestInBandScanSurvivesGarbage: a cut or forged access unit leaves the track
// untouched and never panics.
func TestInBandScanSurvivesGarbage(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{picStruct: true, hrd: true}.nal()}, [][]byte{scanPPS(0, 0, true)})
	good := au(picTimingSEI(true, 1, -1), scanSlice{field: true, lsb: 2}.nal())
	for n := 0; n < len(good); n++ {
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
		ApplyInBandColour(&tr, good[:n])
	}
	for _, f := range [][]byte{{0xff, 0xff, 0xff, 0xff, 0x65}, {0, 0, 0, 2, 0x65, 0xff}, {0, 0, 0, 3, 0x06, 0x01, 0xff}} {
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
		ApplyInBandColour(&tr, f)
		if tr.ScanType != "" {
			t.Errorf("% x: ScanType = %q", f, tr.ScanType)
		}
	}
}

// TestNeedsInBandColourAVCScan: the mp4 driver reads the first sample of an H.264
// track whose scan type the SPS left open, and only that one.
func TestNeedsInBandColourAVCScan(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, true)})
	open := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	if !NeedsInBandColour(&open) {
		t.Error("an H.264 track with field coding allowed and no scan type needs no sample")
	}
	stated := open
	stated.ScanType = "progressive"
	frames := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: scanAvcC([][]byte{scanSPS{frameOnly: true}.nal()}, nil)}
	for _, tr := range []mkv.Track{stated, frames} {
		if NeedsInBandColour(&tr) {
			t.Errorf("scan %q, frames-only SPS: a sample was asked for", tr.ScanType)
		}
	}
	ApplyInBandColour(&open, au(scanSlice{lsb: 2, bottomPOC: true, delta: -1}.nal()))
	if open.ScanType != "interlaced" || open.FieldOrder != "bb" {
		t.Errorf("ApplyInBandColour: ScanType/FieldOrder = %q/%q, want interlaced/bb", open.ScanType, open.FieldOrder)
	}
}
