package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// subOffsetFixture builds the shared source: 6s of video (keyframe every 1s)
// and 3 subtitle cues at t=0/2000/4000ms, ResolveCueEnds giving them
// (0,2000), (2000,4000) and (4000,6000) before any shift.
func subOffsetFixture(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 3; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 2000, key: true,
			data: []byte(fmt.Sprintf("cue numero %d", i+1))})
	}
	sortGenBlocks(gblocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
		},
		gblocks)
}

// A zero offset (the default; Options{} and Options{SubtitleOffsetMs: 0} are
// the same) reproduces today's WebVTT output exactly.
func TestRemuxToHLS_SubtitleOffsetZeroIsRegression(t *testing.T) {
	src := subOffsetFixture(t)

	dirZero := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dirZero, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	dirExplicit := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dirExplicit, Options{SegmentMs: 2000, SubtitleOffsetMs: 0}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sub1.vtt", "sub1_00001.vtt", "sub1_00002.vtt", "sub1_00003.vtt"} {
		a, err := os.ReadFile(filepath.Join(dirZero, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dirExplicit, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: Options{} and Options{SubtitleOffsetMs: 0} differ", name)
		}
	}
	whole, err := os.ReadFile(filepath.Join(dirZero, "sub1.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cue numero 1", "cue numero 2", "cue numero 3"} {
		if !bytes.Contains(whole, []byte(want)) {
			t.Errorf("sub1.vtt missing %q:\n%s", want, whole)
		}
	}
}

// A positive offset shifts every cue later and windowing (which segment a
// cue's VTT lands in) is decided AFTER the shift: a cue that used to open
// segment 1 disappears from it and reappears further along.
func TestRemuxToHLS_SubtitleOffsetPositive(t *testing.T) {
	src := subOffsetFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000, SubtitleOffsetMs: 2500}); err != nil {
		t.Fatal(err)
	}
	seg1, _ := os.ReadFile(filepath.Join(dir, "sub1_00001.vtt"))
	seg2, _ := os.ReadFile(filepath.Join(dir, "sub1_00002.vtt"))
	seg3, _ := os.ReadFile(filepath.Join(dir, "sub1_00003.vtt"))

	if bytes.Contains(seg1, []byte("cue numero")) {
		t.Errorf("segment 1 must be empty after a +2500ms shift (all cues moved later):\n%s", seg1)
	}
	if !bytes.Contains(seg2, []byte("cue numero 1")) {
		t.Errorf("segment 2 must carry the shifted first cue (0 -> 2500ms):\n%s", seg2)
	}
	if !bytes.Contains(seg2, []byte("00:00:02.500")) {
		t.Errorf("segment 2's cue must start at the shifted time 2500ms:\n%s", seg2)
	}
	if !bytes.Contains(seg3, []byte("cue numero 2")) || !bytes.Contains(seg3, []byte("cue numero 3")) {
		t.Errorf("segment 3 must carry the other shifted cues:\n%s", seg3)
	}
}

// A large negative offset drops cues whose shifted end lands at or before 0
// and clamps a cue straddling 0 to start there instead.
func TestRemuxToHLS_SubtitleOffsetNegativeDropAndClamp(t *testing.T) {
	src := subOffsetFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000, SubtitleOffsetMs: -5500}); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(filepath.Join(dir, "sub1.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	// cue 1 (0,2000)-5500=(-5500,-3500): end <= 0 -> dropped.
	// cue 2 (2000,4000)-5500=(-3500,-1500): end <= 0 -> dropped.
	// cue 3 (4000,6000)-5500=(-1500,500): straddles 0 -> clamped to (0,500).
	if bytes.Contains(whole, []byte("cue numero 1")) || bytes.Contains(whole, []byte("cue numero 2")) {
		t.Errorf("cues 1 and 2 must be fully dropped by a -5500ms shift:\n%s", whole)
	}
	if !bytes.Contains(whole, []byte("cue numero 3")) {
		t.Errorf("cue 3 must survive, clamped:\n%s", whole)
	}
	if !bytes.Contains(whole, []byte("00:00:00.000 --> 00:00:00.500")) {
		t.Errorf("cue 3 must be clamped to start at 0 (end at the shifted 500ms):\n%s", whole)
	}
}

// The on-demand plan's subtitle resources, WITH an offset set, stay
// byte-identical to the full pass - both the whole-track file and every
// windowed segment.
func TestPlanHLSSubtitleOffsetMatchesFullPass(t *testing.T) {
	for _, offset := range []int64{2500, -1500} {
		offset := offset
		t.Run(fmt.Sprintf("offset=%d", offset), func(t *testing.T) {
			src := subOffsetFixture(t)
			dir := t.TempDir()
			if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000, SubtitleOffsetMs: offset}); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, SubtitleOffsetMs: offset})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"sub1.vtt", "sub1_00001.vtt", "sub1_00002.vtt", "sub1_00003.vtt"} {
				want, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				got, _, err := plan.Resource(context.Background(), name)
				if err != nil {
					t.Fatalf("Resource(%q): %v", name, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("offset %d: %s differs from the full pass:\nplan: %s\nfull: %s", offset, name, got, want)
				}
			}
		})
	}
}
