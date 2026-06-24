package mkv

import "testing"

func TestParseDolbyVisionConfig(t *testing.T) {
	// profile 8, level 6, rpu=1 el=0 bl=1, bl_signal_compatibility_id 1 (HDR10).
	//   byte2 = profile<<1 | level>>5 = 16
	//   byte3 = level<<3 | rpu<<2 | el<<1 | bl = 0x35
	//   byte4 = compat<<4 = 0x10
	rec := []byte{1, 0, 16, 0x35, 0x10, 0, 0, 0}
	dv := ParseDolbyVisionConfig(rec)
	if dv == nil {
		t.Fatal("ParseDolbyVisionConfig returned nil for a valid record")
	}
	if dv.VersionMajor != 1 || dv.VersionMinor != 0 {
		t.Errorf("version = %d.%d, want 1.0", dv.VersionMajor, dv.VersionMinor)
	}
	if dv.Profile != 8 || dv.Level != 6 || dv.BLSignalCompatID != 1 {
		t.Errorf("profile/level/compat = %d/%d/%d, want 8/6/1", dv.Profile, dv.Level, dv.BLSignalCompatID)
	}
	if !dv.RPUPresent || dv.ELPresent || !dv.BLPresent {
		t.Errorf("flags rpu/el/bl = %v/%v/%v, want true/false/true", dv.RPUPresent, dv.ELPresent, dv.BLPresent)
	}
	if ParseDolbyVisionConfig([]byte{1, 0, 0, 0}) != nil {
		t.Error("ParseDolbyVisionConfig should return nil for a record shorter than 5 bytes")
	}
	// Boundary: exactly 5 bytes is the minimum that parses (not nil).
	if dv := ParseDolbyVisionConfig([]byte{2, 1, 0, 0, 0}); dv == nil {
		t.Error("ParseDolbyVisionConfig should parse a 5-byte record (boundary)")
	} else if dv.VersionMajor != 2 || dv.VersionMinor != 1 {
		t.Errorf("5-byte record fields = %d/%d, want 2/1", dv.VersionMajor, dv.VersionMinor)
	}
}

func TestDolbyVisionConfigRoundTrip(t *testing.T) {
	cases := []DolbyVision{
		{VersionMajor: 1, Profile: 5, Level: 6, RPUPresent: true, BLPresent: true},
		{VersionMajor: 1, Profile: 8, Level: 9, RPUPresent: true, BLPresent: true, BLSignalCompatID: 1},
		{VersionMajor: 1, Profile: 7, Level: 4, RPUPresent: true, ELPresent: true, BLPresent: true},
	}
	for _, want := range cases {
		got := ParseDolbyVisionConfig(EncodeDolbyVisionConfig(&want))
		if got == nil || *got != want {
			t.Errorf("round trip: got %+v, want %+v", got, want)
		}
	}

	if name, id := (&DolbyVision{Profile: 5}).BoxType(); name != "dvcC" || id != BlockAddIDTypeDVCC {
		t.Errorf("profile 5 box = %q/%#x, want dvcC", name, id)
	}
	if name, id := (&DolbyVision{Profile: 8}).BoxType(); name != "dvvC" || id != BlockAddIDTypeDVVC {
		t.Errorf("profile 8 box = %q/%#x, want dvvC", name, id)
	}
}
