package mp4

import "testing"

// dvSampleEntry builds a visual sample entry of the given fourcc carrying an
// hvcC config box and a dvcC Dolby Vision box.
func dvSampleEntry(fourcc string, dvRecord []byte) []byte {
	payload := make([]byte, 78) // visual sample entry fixed header
	payload = append(payload, box("hvcC", []byte{1, 2, 3})...)
	payload = append(payload, box("dvcC", dvRecord)...)
	return box(fourcc, payload)
}

// TestDolbyVisionMP4 checks parseSampleEntry decodes a dvcC box into the track's
// Dolby Vision config, for both a dvhe entry and a cross-compatible hvc1 entry.
func TestDolbyVisionMP4(t *testing.T) {
	rec := []byte{1, 0, 16, 0x35, 0x10, 0, 0, 0} // profile 8, level 6, compat 1
	for _, fourcc := range []string{"dvhe", "hvc1"} {
		stsd := fullBox("stsd", 0, 0, func(w *bw) {
			w.u32(1) // entry_count
			w.bytes(dvSampleEntry(fourcc, rec))
		})
		var tr inTrack
		ok, _, err := parseSampleEntry(&tr, stsd[8:])
		if err != nil || !ok {
			t.Fatalf("%s: parseSampleEntry ok=%v err=%v", fourcc, ok, err)
		}
		if tr.codec != "hevc" {
			t.Errorf("%s: codec = %q, want hevc", fourcc, tr.codec)
		}
		if tr.dolbyVision == nil {
			t.Fatalf("%s: dolbyVision nil, want decoded", fourcc)
		}
		if tr.dolbyVision.Profile != 8 || tr.dolbyVision.BLSignalCompatID != 1 {
			t.Errorf("%s: dv = %+v, want profile 8 / compat 1", fourcc, tr.dolbyVision)
		}
	}
}

// TestDolbyVisionMP4Absent checks a plain hvc1 entry (no dvcC) leaves it nil.
func TestDolbyVisionMP4Absent(t *testing.T) {
	payload := make([]byte, 78)
	payload = append(payload, box("hvcC", []byte{1, 2, 3})...)
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(box("hvc1", payload))
	})
	var tr inTrack
	if ok, _, err := parseSampleEntry(&tr, stsd[8:]); err != nil || !ok {
		t.Fatalf("parseSampleEntry ok=%v err=%v", ok, err)
	}
	if tr.dolbyVision != nil {
		t.Errorf("dolbyVision = %+v, want nil", tr.dolbyVision)
	}
}
