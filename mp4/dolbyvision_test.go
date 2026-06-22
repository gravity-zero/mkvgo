package mp4

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

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

// TestDolbyVisionRemuxRoundTrip carries a Dolby Vision configuration through both
// remux directions: MKV (BlockAdditionMapping) → MP4 (dvvC) → MKV, checking the
// profile and compatibility id survive each hop.
func TestDolbyVisionRemuxRoundTrip(t *testing.T) {
	dv := &mkv.DolbyVision{VersionMajor: 1, Profile: 8, Level: 6, RPUPresent: true, BLPresent: true, BLSignalCompatID: 1}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25), DolbyVision: dv},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{2}},
	}
	// The writer emits a dvvC BlockAdditionMapping for the source MKV.
	srcMKV := buildMKVWithChapters(t, tracks, blocks, nil)
	dir := t.TempDir()

	// MKV → MP4: a dvvC box is written into the visual sample entry; the probe
	// reads it back (this also proves the reader parsed the source's mapping).
	mp4Path := filepath.Join(dir, "dv.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	if got := videoDV(c.Tracks); got == nil || got.Profile != 8 || got.BLSignalCompatID != 1 {
		t.Fatalf("MP4 Dolby Vision = %+v, want profile 8 / compat 1", got)
	}

	// MP4 → MKV: a dvvC BlockAdditionMapping is written; the reader reads it back.
	backMKV := filepath.Join(dir, "back.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, backMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	full, _ := readMKV(t, backMKV)
	if got := videoDV(full.Tracks); got == nil || got.Profile != 8 || got.BLSignalCompatID != 1 {
		t.Fatalf("round-tripped MKV Dolby Vision = %+v, want profile 8 / compat 1", got)
	}
}

func videoDV(tracks []mkv.Track) *mkv.DolbyVision {
	for i := range tracks {
		if tracks[i].Type == mkv.VideoTrack {
			return tracks[i].DolbyVision
		}
	}
	return nil
}
