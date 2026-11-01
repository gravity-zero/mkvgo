package reader

import (
	"bytes"
	"context"
	"os"
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
// untouched and never panics, whole or as a truncated prefix.
func TestInBandScanSurvivesGarbage(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{picStruct: true, hrd: true}.nal()}, [][]byte{scanPPS(0, 0, true)})
	good := au(picTimingSEI(true, 1, -1), scanSlice{field: true, lsb: 2}.nal())
	for n := 0; n < len(good); n++ {
		for _, cut := range []bool{false, true} {
			tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
			s := NewInBandScan(&tr)
			s.Feed(good[:n], cut)
			s.Apply(&tr)
		}
	}
	for _, f := range [][]byte{{0xff, 0xff, 0xff, 0xff, 0x65}, {0, 0, 0, 2, 0x65, 0xff}, {0, 0, 0, 3, 0x06, 0x01, 0xff}} {
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
		s := NewInBandScan(&tr)
		s.Feed(f, false)
		s.Apply(&tr)
		if tr.ScanType != "" {
			t.Errorf("% x: ScanType = %q", f, tr.ScanType)
		}
	}
}

// TestInBandScanAPI: what the mp4 driver uses - a scan only for an H.264 track
// whose scan type the SPS left open, several samples, and the head of a slice
// cut short by a bounded read.
func TestInBandScanAPI(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, true)})
	open := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	stated := open
	stated.ScanType = "progressive"
	frames := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: scanAvcC([][]byte{scanSPS{frameOnly: true}.nal()}, nil)}
	vp9 := mkv.Track{Type: mkv.VideoTrack, Codec: "vp9", CodecPrivate: avcc}
	for _, tr := range []mkv.Track{stated, frames, vp9} {
		if NewInBandScan(&tr) != nil {
			t.Errorf("codec %q scan %q: a scan was returned", tr.Codec, tr.ScanType)
		}
	}
	if NeedsInBandColour(&open) {
		t.Error("NeedsInBandColour asks for an H.264 sample: the scan is NewInBandScan's")
	}
	s := NewInBandScan(&open)
	if s == nil {
		t.Fatal("no scan for an H.264 track with field coding allowed")
	}
	slice := au(scanSlice{lsb: 2, bottomPOC: true, delta: -1}.nal())
	s.Feed(au(scanSlice{idr: true}.nal()), false)
	s.Feed(slice[:len(slice)-2], false) // a cut slice in a whole sample is damage: ignored
	tr := open
	s.Apply(&tr)
	if tr.ScanType != "" {
		t.Fatalf("a cut slice not marked truncated proved %q", tr.ScanType)
	}
	s.Feed(slice[:len(slice)-2], true)
	s.Apply(&open)
	if open.ScanType != "interlaced" || open.FieldOrder != "bb" {
		t.Errorf("ScanType/FieldOrder = %q/%q, want interlaced/bb", open.ScanType, open.FieldOrder)
	}
}

// TestInBandScanAVC3: a record with no SPS (avc3) is settled from the SPS the
// samples carry - field coding allowed and proved, or frames only.
func TestInBandScanAVC3(t *testing.T) {
	pps := scanPPS(0, 0, true)
	cases := []struct {
		name        string
		sps         scanSPS
		delta       int32
		scan, order string
	}{
		{"in-band SPS, frames only", scanSPS{frameOnly: true}, 0, "progressive", "progressive"},
		{"in-band SPS, field coding, fields at two instants", scanSPS{}, 1, "interlaced", "tt"},
		{"in-band SPS, field coding, no proof", scanSPS{}, 0, "", ""},
	}
	for _, c := range cases {
		slice := scanSlice{idr: true, bottomPOC: !c.sps.frameOnly, delta: c.delta}
		if c.sps.frameOnly {
			slice = scanSlice{idr: true}
		}
		frame := au(c.sps.nal(), pps, frameOnlySlice(c.sps.frameOnly, slice))
		meta, err := ReadMeta(context.Background(), bytes.NewReader(avcScanMKV(scanAvcC(nil, nil), nil, frame)), "", WithInBandColourFallback())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := meta.Tracks[0]; got.ScanType != c.scan || got.FieldOrder != c.order {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want %q/%q", c.name, got.ScanType, got.FieldOrder, c.scan, c.order)
		}
	}
}

// frameOnlySlice builds the slice for an SPS that codes frames only, which has no
// field_pic_flag, or the field-coding slice otherwise.
func frameOnlySlice(frameOnly bool, o scanSlice) []byte {
	if !frameOnly {
		return o.nal()
	}
	b := &tbw{}
	b.ue(0)
	b.ue(7)
	b.ue(0)
	b.u(0, 4) // frame_num
	b.ue(0)   // idr_pic_id
	b.u(0, 4) // pic_order_cnt_lsb
	b.u(0xffff, 16)
	return append([]byte{0x65}, b.bytes()...)
}

// hevcPicTimingSEI is a prefix SEI NAL holding one picture timing message that
// opens on pic_struct and source_scan_type (frame_field_info_present_flag 1).
func hevcPicTimingSEI(picStruct, sst uint32) []byte {
	b := &tbw{}
	b.u(picStruct, 4)
	b.u(sst, 2)
	b.u(0, 1) // duplicate_flag
	b.u(0, 9)
	p := b.bytes()
	return append(append([]byte{39 << 1, 0x01, 0x01, byte(len(p))}, p...), 0x80)
}

// TestHEVCScanPicTiming: a picture is interlaced when its timing SEI says so
// (source_scan_type 0) or shows a field, never when it says its source is
// progressive; pic_struct 3-6 and 9-12 name the order.
func TestHEVCScanPicTiming(t *testing.T) {
	cases := []struct {
		name        string
		sei         [][2]uint32 // pic_struct, source_scan_type per sample
		scan, order string
	}{
		{"interlaced source, frame", [][2]uint32{{0, 0}}, "interlaced", ""},
		{"top field before its bottom", [][2]uint32{{11, 2}, {12, 2}}, "interlaced", ""},
		{"top field before its bottom, alone", [][2]uint32{{11, 2}, {10, 2}}, "interlaced", "tt"},
		{"bottom field after its top is top first", [][2]uint32{{10, 0}}, "interlaced", "tt"},
		{"top field after its bottom", [][2]uint32{{9, 0}}, "interlaced", "bb"},
		{"top-bottom frame, interlaced source", [][2]uint32{{3, 0}}, "interlaced", "tt"},
		{"top-bottom frame, source unknown", [][2]uint32{{3, 2}}, "", ""},
		{"field, progressive source (PsF)", [][2]uint32{{1, 1}}, "", ""},
		{"frames, progressive source", [][2]uint32{{0, 1}, {0, 1}}, "", ""},
		{"field alone", [][2]uint32{{2, 2}}, "interlaced", ""},
	}
	for _, c := range cases {
		s := &hevcScan{nalLen: 4, frameFieldInfo: true, orders: map[string]bool{}}
		for _, v := range c.sei {
			s.feed(au(hevcPicTimingSEI(v[0], v[1])), false)
		}
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "hevc"}
		s.apply(&tr)
		if tr.ScanType != c.scan || tr.FieldOrder != c.order {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want %q/%q", c.name, tr.ScanType, tr.FieldOrder, c.scan, c.order)
		}
	}
	s := &hevcScan{nalLen: 4, orders: map[string]bool{}} // no frame_field_info: the SEI opens on other fields
	s.feed(au(hevcPicTimingSEI(1, 0)), false)
	if s.interlaced {
		t.Error("a timing SEI read without frame_field_info_present_flag proved interlaced")
	}
}

// stripScanElements rewrites FlagInterlaced to 0 and FieldOrder to 2
// (undetermined), same size, so the container states nothing.
func stripScanElements(t *testing.T, data []byte) []byte {
	t.Helper()
	b := append([]byte(nil), data...)
	if i := bytes.Index(b, []byte{0x9a, 0x81}); i >= 0 {
		b[i+2] = 0
	} else {
		t.Fatal("fixture: no FlagInterlaced")
	}
	if i := bytes.Index(b, []byte{0x9d, 0x81}); i >= 0 {
		b[i+2] = 2
	}
	return b
}

// TestMPEG2ScanFFmpegFiles: MPEG-2 files ffmpeg wrote carry no CodecPrivate - the
// sequence header and its extensions travel in the first block. With the
// container's scan elements cleared, the in-band option reads the order from
// top_field_first and progressive_sequence 1 as progressive; without it the
// scan type stays unknown.
func TestMPEG2ScanFFmpegFiles(t *testing.T) {
	for file, want := range map[string][2]string{
		"mpeg2_tff.mkv":         {"interlaced", "tt"},
		"mpeg2_bff.mkv":         {"interlaced", "bb"},
		"mpeg2_progressive.mkv": {"progressive", "progressive"},
	} {
		raw, err := os.ReadFile("../../internal/testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		data := stripScanElements(t, raw)
		for _, opt := range []bool{false, true} {
			var opts []ReadOption
			exp := [2]string{"", ""}
			if opt {
				opts, exp = append(opts, WithInBandColourFallback()), want
			}
			c, err := ReadMeta(context.Background(), bytes.NewReader(data), "", opts...)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if got := c.Tracks[0]; got.ScanType != exp[0] || got.FieldOrder != exp[1] {
				t.Errorf("%s (in-band %v): ScanType/FieldOrder = %q/%q, want %q/%q", file, opt, got.ScanType, got.FieldOrder, exp[0], exp[1])
			}
		}
	}
}

// mpeg2PicExt is a picture coding extension with the given picture_structure,
// top_field_first, repeat_first_field and progressive_frame.
func mpeg2PicExt(structure uint32, tff, rff, progressive bool) []byte {
	b := &tbw{}
	b.u(8, 4)       // extension_start_code_identifier
	b.u(0xffff, 16) // f_codes
	b.u(0, 2)
	b.u(structure, 2)
	b.u(boolBit(tff), 1)
	b.u(0, 5)
	b.u(boolBit(rff), 1)
	b.u(0, 1)
	b.u(boolBit(progressive), 1)
	b.u(0, 7)
	return append([]byte{0, 0, 1, 0xb5}, b.bytes()...)
}

func mpeg2SeqExt(progressiveSeq bool) []byte {
	b := &tbw{}
	b.u(1, 4)
	b.u(0x48, 8)
	b.u(boolBit(progressiveSeq), 1)
	b.u(0, 3)
	return append([]byte{0, 0, 1, 0xb5}, b.bytes()...)
}

func TestMPEG2ScanPictures(t *testing.T) {
	cases := []struct {
		name        string
		samples     [][]byte
		scan, order string
	}{
		{"field pair, bottom first", [][]byte{append(mpeg2PicExt(2, false, false, false), mpeg2PicExt(1, false, false, false)...)}, "interlaced", "bb"},
		{"field pairs, top first", [][]byte{mpeg2PicExt(1, false, false, false), mpeg2PicExt(2, false, false, false), mpeg2PicExt(1, false, false, false)}, "interlaced", "tt"},
		{"soft telecine: progressive frames repeating a field", [][]byte{append(mpeg2SeqExt(false), mpeg2PicExt(3, true, true, true)...), mpeg2PicExt(3, false, false, true)}, "", ""},
		{"interlaced frames, orders disagree", [][]byte{mpeg2PicExt(3, true, false, false), mpeg2PicExt(3, false, false, false)}, "interlaced", ""},
		{"progressive sequence", [][]byte{mpeg2SeqExt(true)}, "progressive", "progressive"},
		{"cut extension", [][]byte{mpeg2PicExt(3, true, false, false)[:6]}, "", ""},
	}
	for _, c := range cases {
		s := &mpeg2Scan{orders: map[string]bool{}}
		for _, f := range c.samples {
			s.feed(f, false)
		}
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "V_MPEG2"}
		s.apply(&tr)
		if tr.ScanType != c.scan || tr.FieldOrder != c.order {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want %q/%q", c.name, tr.ScanType, tr.FieldOrder, c.scan, c.order)
		}
	}
	if bc := mpeg2Colour(append([]byte{0, 0, 1, 0xb3, 0, 0, 0, 0}, mpeg2SeqExt(true)...)); bc == nil || bc.scanType != "progressive" {
		t.Error("a CodecPrivate sequence extension with progressive_sequence 1 gave no progressive scan type")
	}
}
