package mkv

import (
	"bytes"
	"math"
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
		// Zero display dimensions must be ignored (fall back to coded dims), so the
		// SAR is 1:1, not a 0-based ratio.
		{"zero display dims → coded",
			Track{Width: u32(1920), Height: u32(1080), DisplayWidth: u32(0), DisplayHeight: u32(0)},
			"1:1", "16:9"},
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
		// collapses to the (max-1):max best approximation, like mainstream probers → 719:720.
		{48379, 48420, 720, 719, 720},
		// Legit ratios stay exact under the conventional 1024*1024 bound.
		{257, 160, aspectReduceMax, 257, 160},
		{48379, 48420, aspectReduceMax, 48379, 48420},
		{16, 9, aspectReduceMax, 16, 9},
		// Already reduced; gcd applied first.
		{3840, 2160, aspectReduceMax, 16, 9},
		// Boundary: a value exactly at max stays as-is (the num<=max && den<=max
		// short-circuit must include equality).
		{aspectReduceMax, 1, aspectReduceMax, aspectReduceMax, 1},
	} {
		if n, d := avReduce(tt.num, tt.den, tt.max); n != tt.wantN || d != tt.wantD {
			t.Errorf("avReduce(%d,%d,%d) = %d:%d, want %d:%d", tt.num, tt.den, tt.max, n, d, tt.wantN, tt.wantD)
		}
	}

	// Property test: for several ratios that exceed the bound, avReduce must return
	// terms within the bound AND the best rational approximation achievable there  -
	// verified against a brute-force search. This pins the internal arithmetic: any
	// mutation either over-shoots the bound or yields a worse approximation.
	for _, c := range []struct{ num, den, max uint64 }{
		{48379, 48420, 720},
		{3000001, 2999990, 4096},
		{5000000, 4999993, 10000},
		{16000, 9001, 1000},
		{1920000, 1079999, 5000},
	} {
		n, d := avReduce(c.num, c.den, c.max)
		if n == 0 || d == 0 || n > c.max || d > c.max {
			t.Errorf("avReduce(%d,%d,%d) = %d:%d out of bound", c.num, c.den, c.max, n, d)
			continue
		}
		target := float64(c.num) / float64(c.den)
		got := math.Abs(float64(n)/float64(d) - target)
		best := bestRationalError(target, c.max)
		if got > best+1e-12 {
			t.Errorf("avReduce(%d,%d,%d) = %d:%d (err %.3e) not optimal (best %.3e)",
				c.num, c.den, c.max, n, d, got, best)
		}
	}
}

// bestRationalError returns the smallest |p/q - target| over all p,q in [1,max]  -
// a brute-force reference for the best bounded rational approximation.
func bestRationalError(target float64, max uint64) float64 {
	best := math.MaxFloat64
	for q := uint64(1); q <= max; q++ {
		p := uint64(math.Round(float64(q) * target))
		if p < 1 {
			p = 1
		}
		if p > max {
			p = max
		}
		if e := math.Abs(float64(p)/float64(q) - target); e < best {
			best = e
		}
	}
	return best
}

func TestCodecLongNameAndChannelLayout(t *testing.T) {
	if got := (&Track{Codec: "h264"}).CodecLongName(); got != "H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10" {
		t.Errorf("h264 long name = %q", got)
	}
	if got := (&Track{Codec: "eac3"}).CodecLongName(); got != "ATSC A/52B (AC-3, E-AC-3)" {
		t.Errorf("eac3 long name = %q", got)
	}
	if got := (&Track{Codec: "wat"}).CodecLongName(); got != "" {
		t.Errorf("unknown codec long name = %q, want empty", got)
	}
	u8 := func(v uint8) *uint8 { return &v }
	for _, tt := range []struct {
		codec string
		ch    *uint8
		want  string
	}{
		{"aac", u8(1), "mono"},
		{"aac", u8(2), "stereo"},
		{"eac3", u8(6), "5.1(side)"}, // E-AC-3 surrounds at the sides
		{"ac3", u8(6), "5.1"},        // AC-3 uses the back positions
		{"aac", u8(8), "7.1"},
		{"aac", nil, ""},
	} {
		if got := (&Track{Codec: tt.codec, Channels: tt.ch}).ChannelLayout(); got != tt.want {
			t.Errorf("ChannelLayout(%s,%v) = %q, want %q", tt.codec, tt.ch, got, tt.want)
		}
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
	// A zero OutputSampleRate must NOT win - it falls through to the base rate.
	if got := (&Track{SampleRate: f(44100), OutputSampleRate: f(0)}).EffectiveSampleRate(); got != 44100 {
		t.Errorf("zero output rate should fall through to base, got %v", got)
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
		{Issue{Severity: SeverityError, Message: "bad track"}, "[error] bad track"},
		{Issue{Severity: SeverityWarning, Message: "no cues"}, "[warning] no cues"},
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
