package mkv

import "testing"

func u16(v uint16) *uint16 { return &v }

func TestColourNamesMatchFFprobe(t *testing.T) {
	// code points and the exact strings ffprobe (av_color_*_name) reports.
	tests := []struct {
		name                                   string
		matrix, transfer, primaries, rng       *uint16
		wantSpace, wantTrc, wantPri, wantRange string
	}{
		{
			name:   "SDR BT.709 limited",
			matrix: u16(1), transfer: u16(1), primaries: u16(1), rng: u16(1),
			wantSpace: "bt709", wantTrc: "bt709", wantPri: "bt709", wantRange: "tv",
		},
		{
			name:   "HDR10 BT.2020 PQ full",
			matrix: u16(9), transfer: u16(16), primaries: u16(9), rng: u16(2),
			wantSpace: "bt2020nc", wantTrc: "smpte2084", wantPri: "bt2020", wantRange: "pc",
		},
		{
			name:   "HLG BT.2020",
			matrix: u16(9), transfer: u16(18), primaries: u16(9), rng: u16(1),
			wantSpace: "bt2020nc", wantTrc: "arib-std-b67", wantPri: "bt2020", wantRange: "tv",
		},
		{
			name:   "unknown / unset codes",
			matrix: u16(2), transfer: u16(2), primaries: u16(2), rng: u16(0),
			wantSpace: "unknown", wantTrc: "unknown", wantPri: "unknown", wantRange: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &Track{
				ColorSpace:     tt.matrix,
				ColorTransfer:  tt.transfer,
				ColorPrimaries: tt.primaries,
				ColorRange:     tt.rng,
			}
			if got := tr.ColorSpaceName(); got != tt.wantSpace {
				t.Errorf("ColorSpaceName = %q, want %q", got, tt.wantSpace)
			}
			if got := tr.ColorTransferName(); got != tt.wantTrc {
				t.Errorf("ColorTransferName = %q, want %q", got, tt.wantTrc)
			}
			if got := tr.ColorPrimariesName(); got != tt.wantPri {
				t.Errorf("ColorPrimariesName = %q, want %q", got, tt.wantPri)
			}
			if got := tr.ColorRangeName(); got != tt.wantRange {
				t.Errorf("ColorRangeName = %q, want %q", got, tt.wantRange)
			}
		})
	}
}

func TestColourNamesNilIsEmpty(t *testing.T) {
	tr := &Track{} // no Colour element at all
	if tr.ColorSpaceName() != "" || tr.ColorTransferName() != "" ||
		tr.ColorPrimariesName() != "" || tr.ColorRangeName() != "" {
		t.Error("nil colour fields must map to empty strings")
	}
}

func TestIsHDR(t *testing.T) {
	tests := []struct {
		name           string
		primaries, trc *uint16
		want           bool
	}{
		{"PQ HDR10", u16(9), u16(16), true},
		{"HLG", u16(9), u16(18), true},
		{"BT.2020 but SDR transfer", u16(9), u16(14), false},
		{"PQ transfer but BT.709 primaries", u16(1), u16(16), false},
		{"no colour element", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &Track{ColorPrimaries: tt.primaries, ColorTransfer: tt.trc}
			if got := tr.IsHDR(); got != tt.want {
				t.Errorf("IsHDR = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFFprobeCodecName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"srt", "subrip"},
		{"vobsub", "dvd_subtitle"},
		{"pgs", "hdmv_pgs_subtitle"},
		{"dvbsub", "dvb_subtitle"},
		{"h264", "h264"}, // identical in both tools
		{"opus", "opus"}, // identical
		{"ass", "ass"},   // identical
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		if got := FFprobeCodecName(tt.in); got != tt.want {
			t.Errorf("FFprobeCodecName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolvedLanguage(t *testing.T) {
	cases := []struct {
		name          string
		legacy, bcp47 string
		want          string
	}{
		{"both → bcp47 wins", "fre", "fr", "fr"},
		{"legacy only", "ger", "", "ger"},
		{"bcp47 only", "", "pt-BR", "pt-BR"},
		{"neither", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &Track{Language: c.legacy, LanguageBCP47: c.bcp47}
			if got := tr.ResolvedLanguage(); got != c.want {
				t.Errorf("ResolvedLanguage = %q, want %q", got, c.want)
			}
		})
	}
}
