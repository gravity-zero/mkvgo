package reader

import (
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// FuzzInBandScan throws arbitrary records and samples at the H.264, HEVC and
// MPEG-2 scan walks, whole or truncated, called WITHOUT their recover() so a
// missing bound panics. The verdict must stay one the API documents: ScanType
// "", "progressive" or "interlaced", and a FieldOrder that agrees with it.
func FuzzInBandScan(f *testing.F) {
	avcc := scanAvcC([][]byte{scanSPS{mbaff: true, picStruct: true, hrd: true}.nal()}, [][]byte{scanPPS(0, 0, true)})
	f.Add(uint8(0), avcc, au(picTimingSEI(true, 3, 1), scanSlice{lsb: 2, bottomPOC: true, delta: -1}.nal()), false)
	f.Add(uint8(0), avcc, au(scanSlice{field: true, frameNum: 1, lsb: 2}.nal(), scanSlice{field: true, bottom: true, frameNum: 1, lsb: 3}.nal()), true)
	f.Add(uint8(0), scanAvcC(nil, nil), au(scanSPS{pocType: 1}.nal(), scanPPS(0, 0, true), scanSlice{pocType: 1, bottomPOC: true, delta: 1}.nal()), false)
	f.Add(uint8(1), seedHex(hevcHDRPrivateHex), au(hevcPicTimingSEI(11, 0)), false)
	f.Add(uint8(2), []byte(nil), append(mpeg2SeqExt(false), mpeg2PicExt(1, false, false, false)...), false)
	f.Add(uint8(2), []byte(nil), mpeg2PicExt(3, true, true, true), true)

	f.Fuzz(func(t *testing.T, sel uint8, cp, frame []byte, truncated bool) {
		costguard.Fuzz(t, len(cp)+len(frame), func() {
			tr := mkv.Track{Type: mkv.VideoTrack, CodecPrivate: cp}
			var s scanner
			switch sel % 3 {
			case 0:
				tr.Codec = "h264"
				if a := newAVCScan(cp); a != nil {
					a.walk(frame, truncated)
					a.walk(frame, truncated)
					s = a
				}
			case 1:
				tr.Codec = "hevc"
				if h := newHEVCScan(cp); h != nil {
					h.frameFieldInfo = h.frameFieldInfo || len(frame)%2 == 0
					h.walk(frame, truncated)
					s = h
				}
			case 2:
				tr.Codec = "V_MPEG2"
				m := &mpeg2Scan{orders: map[string]bool{}}
				m.feed(frame, truncated)
				s = m
			}
			if s == nil {
				return
			}
			s.done()
			s.apply(&tr)
			switch tr.ScanType {
			case "":
				if tr.FieldOrder != "" {
					t.Fatalf("FieldOrder %q with no scan type", tr.FieldOrder)
				}
			case "progressive":
				if tr.FieldOrder != "progressive" {
					t.Fatalf("progressive with FieldOrder %q", tr.FieldOrder)
				}
			case "interlaced":
				if tr.FieldOrder != "" && tr.FieldOrder != "tt" && tr.FieldOrder != "bb" {
					t.Fatalf("interlaced with FieldOrder %q", tr.FieldOrder)
				}
			default:
				t.Fatalf("ScanType %q", tr.ScanType)
			}
		})
	})
}
