package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestLevelTables(t *testing.T) {
	if avcLevel(0) != nil || avcLevel(99) != nil || u16v(avcLevel(9)) != 9 || u16v(avcLevel(40)) != 40 {
		t.Error("avcLevel: 0 and 99 must be nil, 9 and 40 kept")
	}
	if hevcLevel(0) != nil || hevcLevel(100) != nil || u16v(hevcLevel(120)) != 120 {
		t.Error("hevcLevel: 0 and 100 must be nil, 120 kept")
	}
	if av1Level(24) != nil || av1Level(30) != nil || u16v(av1Level(23)) != 23 || u16v(av1Level(31)) != 31 {
		t.Error("av1Level: 24..30 reserved must be nil, 23 and 31 kept")
	}
}

// TestAVCProfileFromConstraintFlags: the constraint_set flags qualify the
// profile the way probers spell it, and a level_idc outside the table reads
// as absent. The SPS bytes are patched in place: avcC[9..11] = profile_idc,
// constraint flags, level_idc of the embedded SPS.
func TestAVCProfileFromConstraintFlags(t *testing.T) {
	base := buildHighSPSAvcC(1, 1, 1)
	if base[8] != 0x67 {
		t.Fatalf("fixture: avcC[8] = 0x%02X, want the SPS NAL header 0x67", base[8])
	}
	cases := []struct {
		name         string
		profile, cst byte
		level        byte
		wantProfile  string
		wantLevel    int
	}{
		{"High", 100, 0x00, 40, "High", 40},
		{"Constrained High", 100, 0x0C, 40, "Constrained High", 40},
		{"Baseline", 66, 0x00, 30, "Baseline", 30},
		{"Constrained Baseline", 66, 0x40, 30, "Constrained Baseline", 30},
		{"High 10 Intra", 110, 0x10, 41, "High 10 Intra", 41},
		{"level out of table", 100, 0x00, 0, "High", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := append([]byte{}, base...)
			cp[1], cp[3] = tc.profile, tc.level
			cp[9], cp[10], cp[11] = tc.profile, tc.cst, tc.level
			tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: cp}
			fillColourFromCodecPrivate(&tr)
			if tr.Profile != tc.wantProfile || u16v(tr.Level) != tc.wantLevel {
				t.Errorf("profile=%q level=%d, want %q / %d", tr.Profile, u16v(tr.Level), tc.wantProfile, tc.wantLevel)
			}
		})
	}
}

// TestProgressiveOnlyCodecs: VP8/VP9/AV1 have no interlaced coding, so an
// unstated scan type reads as progressive; a container that states one keeps
// its word.
func TestProgressiveOnlyCodecs(t *testing.T) {
	ctx := context.Background()
	c, err := ReadMeta(ctx, bytes.NewReader(vp9MKV(nil, nil, 640, 360, nil)), "x.webm")
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Tracks[0]; tr.ScanType != "progressive" || tr.FieldOrder != "progressive" {
		t.Errorf("vp9 unstated: scan=%q order=%q, want progressive/progressive", tr.ScanType, tr.FieldOrder)
	}
	// FlagInterlaced = 1 (a wrong statement for VP9, but the container's).
	c, err = ReadMeta(ctx, bytes.NewReader(vp9MKV(nil, uintElem(mkv.IDFlagInterlaced, 1, 1), 640, 360, nil)), "x.webm")
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Tracks[0]; tr.ScanType != "interlaced" || tr.FieldOrder != "" {
		t.Errorf("vp9 stated interlaced: scan=%q order=%q, want interlaced/\"\"", tr.ScanType, tr.FieldOrder)
	}
	h := mkv.Track{Type: mkv.VideoTrack, Codec: "h264"}
	fillCodecDerived(&h)
	if h.ScanType != "" {
		t.Errorf("h264 without an SPS must stay unknown, got %q", h.ScanType)
	}
}

func TestVP9FeatureMetadataFromVpcC(t *testing.T) {
	fullBox := []byte{1, 0, 0, 0, 2, 31, 10<<4 | 0<<1 | 1, 9, 16, 9, 0, 0}
	got, ok := VP9FeatureMetadataFromVpcC(fullBox)
	want := []byte{1, 1, 2, 2, 1, 31, 3, 1, 10, 4, 1, 0}
	if !ok || !bytes.Equal(got, want) {
		t.Errorf("FullBox vpcC → % x (ok=%v), want % x", got, ok, want)
	}
	bare := []byte{0, 99, 8 << 4, 1, 1, 1, 0, 0} // level 99: not a level, left out
	got, ok = VP9FeatureMetadataFromVpcC(bare)
	want = []byte{1, 1, 0, 3, 1, 8, 4, 1, 0}
	if !ok || !bytes.Equal(got, want) {
		t.Errorf("bare vpcC with bogus level → % x (ok=%v), want % x", got, ok, want)
	}
	if _, ok := VP9FeatureMetadataFromVpcC(want); ok {
		t.Error("feature metadata must not convert again")
	}
	if _, ok := VP9FeatureMetadataFromVpcC(nil); ok {
		t.Error("empty must not convert")
	}
	// Round trip through the reader: the records parse back to the same facts.
	m, ok := ParseVP9FeatureMetadata(got)
	if !ok || m.Profile == nil || *m.Profile != 0 || m.Level != nil || m.BitDepth == nil || *m.BitDepth != 8 {
		t.Errorf("parse back = %+v (ok=%v)", m, ok)
	}
}
