package mkv

import (
	"bytes"
	"testing"
)

func TestRestoreHeader_NoStripping(t *testing.T) {
	tr := &Track{}
	data := []byte{0x01, 0x02}
	got := tr.RestoreHeader(data)
	if &got[0] != &data[0] {
		t.Error("expected same slice when no header stripping")
	}
}

func TestRestoreHeader_EmptyHeaderStripping(t *testing.T) {
	tr := &Track{HeaderStripping: []byte{}}
	data := []byte{0x01, 0x02}
	got := tr.RestoreHeader(data)
	if &got[0] != &data[0] {
		t.Error("expected same slice when header stripping is empty")
	}
}

func TestRestoreHeader_WithData(t *testing.T) {
	tr := &Track{HeaderStripping: []byte{0xAA, 0xBB}}
	data := []byte{0x01, 0x02, 0x03}
	got := tr.RestoreHeader(data)
	want := []byte{0xAA, 0xBB, 0x01, 0x02, 0x03}
	if !bytes.Equal(got, want) {
		t.Errorf("RestoreHeader = %x, want %x", got, want)
	}
}

func TestRestoreHeader_NilData(t *testing.T) {
	tr := &Track{HeaderStripping: []byte{0xAA}}
	got := tr.RestoreHeader(nil)
	want := []byte{0xAA}
	if !bytes.Equal(got, want) {
		t.Errorf("RestoreHeader(nil) = %x, want %x", got, want)
	}
}

func TestAspectRatios(t *testing.T) {
	u32 := func(v uint32) *uint32 { return &v }
	for _, tt := range []struct {
		name             string
		t                Track
		wantSAR, wantDAR string
	}{
		{"square 1920x1080", Track{Width: u32(1920), Height: u32(1080)}, "1:1", "16:9"},
		{"anamorphic NTSC DVD 720x480 → 16:9",
			Track{Width: u32(720), Height: u32(480), DisplayWidth: u32(853), DisplayHeight: u32(480)},
			"853:720", "853:480"},
		{"anamorphic exact 32:27 (PAL wide)",
			Track{Width: u32(720), Height: u32(576), DisplayWidth: u32(1024), DisplayHeight: u32(576)},
			"64:45", "16:9"},
		{"no dimensions", Track{}, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.t.SampleAspectRatio(); got != tt.wantSAR {
				t.Errorf("SampleAspectRatio = %q, want %q", got, tt.wantSAR)
			}
			if got := tt.t.DisplayAspectRatio(); got != tt.wantDAR {
				t.Errorf("DisplayAspectRatio = %q, want %q", got, tt.wantDAR)
			}
		})
	}
}

func TestAvReduce(t *testing.T) {
	for _, tt := range []struct {
		num, den, max uint64
		wantN, wantD  uint64
	}{
		// Faithful av_reduce: a pathological near-square ratio under a tight bound
		// collapses to the (max-1):max best approximation, like ffmpeg → 719:720.
		{48379, 48420, 720, 719, 720},
		// Legit ratios stay exact under ffmpeg's 1024*1024 bound.
		{257, 160, aspectReduceMax, 257, 160},
		{48379, 48420, aspectReduceMax, 48379, 48420},
		{16, 9, aspectReduceMax, 16, 9},
		// Already reduced; gcd applied first.
		{3840, 2160, aspectReduceMax, 16, 9},
	} {
		if n, d := avReduce(tt.num, tt.den, tt.max); n != tt.wantN || d != tt.wantD {
			t.Errorf("avReduce(%d,%d,%d) = %d:%d, want %d:%d", tt.num, tt.den, tt.max, n, d, tt.wantN, tt.wantD)
		}
	}
	// Both parts must always stay within the bound.
	if n, d := avReduce(7_000_003, 6_999_999, aspectReduceMax); n > aspectReduceMax || d > aspectReduceMax {
		t.Errorf("avReduce over-bound: %d:%d exceeds %d", n, d, aspectReduceMax)
	}
}

func TestEffectiveSampleRate(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	if got := (&Track{SampleRate: f(24000), OutputSampleRate: f(48000)}).EffectiveSampleRate(); got != 48000 {
		t.Errorf("SBR effective rate = %v, want 48000", got)
	}
	if got := (&Track{SampleRate: f(44100)}).EffectiveSampleRate(); got != 44100 {
		t.Errorf("plain effective rate = %v, want 44100", got)
	}
	if got := (&Track{}).EffectiveSampleRate(); got != 0 {
		t.Errorf("unknown effective rate = %v, want 0", got)
	}
}

func TestIssue_String(t *testing.T) {
	for _, tt := range []struct {
		issue Issue
		want  string
	}{
		{Issue{SeverityError, "bad track"}, "[error] bad track"},
		{Issue{SeverityWarning, "no cues"}, "[warning] no cues"},
	} {
		if got := tt.issue.String(); got != tt.want {
			t.Errorf("Issue.String() = %q, want %q", got, tt.want)
		}
	}
}

func TestDiff_String(t *testing.T) {
	for _, tt := range []struct {
		diff Diff
		want string
	}{
		{Diff{DiffAdded, "tracks", "track 2"}, "[added] tracks: track 2"},
		{Diff{DiffRemoved, "chapters", "ch 1"}, "[removed] chapters: ch 1"},
		{Diff{DiffChanged, "info", "title"}, "[changed] info: title"},
	} {
		if got := tt.diff.String(); got != tt.want {
			t.Errorf("Diff.String() = %q, want %q", got, tt.want)
		}
	}
}
