package ops

import (
	"runtime"
	"testing"
)

func TestSafePath(t *testing.T) {
	base := t.TempDir()
	absPath := "/etc/passwd"
	if runtime.GOOS == "windows" {
		absPath = "C:\\Windows\\System32"
	}
	for _, tt := range []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"normal", "1.h264", false},
		{"subdir", "sub/2.aac", false},
		{"traversal", "../../../etc/passwd", true},
		{"double traversal", "../../evil", true},
		{"absolute", absPath, true},
		{"dot prefix", "./ok.h264", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := safePath(base, tt.input)
			if tt.wantErr && err == nil {
				t.Errorf("safePath(%q, %q) expected error", base, tt.input)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("safePath(%q, %q) unexpected error: %v", base, tt.input, err)
			}
		})
	}
}

func TestSanitizeCodec(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  string
	}{
		{"h264", "h264"},
		{"../../evil", "____evil"},
		{"V_MPEG4/ISO/AVC", "V_MPEG4_ISO_AVC"},
		{"", "raw"},
		{"back\\slash", "back_slash"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeCodec(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeCodec(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
