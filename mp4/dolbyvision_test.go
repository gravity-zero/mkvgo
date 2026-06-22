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
	// Cross-compatible (profile 8, dvvC, plain hvc1/avc1 entry) and single-layer
	// (profile 5, dvcC, dedicated dva1 entry) must both survive both directions.
	t.Run("profile8", func(t *testing.T) {
		roundTripDV(t, &mkv.DolbyVision{VersionMajor: 1, Profile: 8, Level: 6, RPUPresent: true, BLPresent: true, BLSignalCompatID: 1})
	})
	t.Run("profile5", func(t *testing.T) {
		roundTripDV(t, &mkv.DolbyVision{VersionMajor: 1, Profile: 5, Level: 6, RPUPresent: true, BLPresent: true, BLSignalCompatID: 0})
	})
}

func roundTripDV(t *testing.T, dv *mkv.DolbyVision) {
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
	// The writer emits a dvcC/dvvC BlockAdditionMapping for the source MKV.
	srcMKV := buildMKVWithChapters(t, tracks, blocks, nil)
	dir := t.TempDir()

	check := func(stage string, got *mkv.DolbyVision) {
		if got == nil || got.Profile != dv.Profile || got.BLSignalCompatID != dv.BLSignalCompatID {
			t.Fatalf("%s Dolby Vision = %+v, want profile %d / compat %d", stage, got, dv.Profile, dv.BLSignalCompatID)
		}
	}

	// MKV → MP4: the config box is written into the visual sample entry; the probe
	// reads it back (also proving the reader parsed the source's mapping).
	mp4Path := filepath.Join(dir, "dv.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	check("MP4", videoDV(c.Tracks))

	// MP4 → MKV: a BlockAdditionMapping is written; the reader reads it back.
	backMKV := filepath.Join(dir, "back.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, backMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	full, _ := readMKV(t, backMKV)
	check("round-tripped MKV", videoDV(full.Tracks))
}

func videoDV(tracks []mkv.Track) *mkv.DolbyVision {
	for i := range tracks {
		if tracks[i].Type == mkv.VideoTrack {
			return tracks[i].DolbyVision
		}
	}
	return nil
}

// TestDolbyVisionSampleEntryType checks the visual sample entry uses the Dolby
// tag (dva1/dvh1/dav1) only for a non-cross-compatible stream, and keeps the plain
// tag for a cross-compatible one.
func TestDolbyVisionSampleEntryType(t *testing.T) {
	videoEntry := func(dv *mkv.DolbyVision) string {
		tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25), DolbyVision: dv}}
		blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{1}}}
		_, boxes := remux(t, buildMKV(t, tracks, blocks))
		for _, tr := range moovTraks(t, boxes) {
			if tr.handler == "vide" {
				return tr.sampleEntry
			}
		}
		return ""
	}
	// Cross-compatible (bl_signal_compatibility_id != 0) keeps the plain AVC tag.
	if got := videoEntry(&mkv.DolbyVision{Profile: 8, BLSignalCompatID: 1, RPUPresent: true, BLPresent: true}); got != "avc1" {
		t.Errorf("profile 8 entry = %q, want avc1", got)
	}
	// Single-layer (bl_signal_compatibility_id 0) uses the Dolby entry type.
	if got := videoEntry(&mkv.DolbyVision{Profile: 5, BLSignalCompatID: 0, RPUPresent: true, BLPresent: true}); got != "dva1" {
		t.Errorf("profile 5 entry = %q, want dva1", got)
	}
}
