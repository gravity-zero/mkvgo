package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func mdhdDuration(t *testing.T, init []byte) int64 {
	t.Helper()
	i := bytes.Index(init, []byte("mdhd"))
	if i < 0 {
		t.Fatal("no mdhd")
	}
	if init[i+4] == 1 {
		return int64(binary.BigEndian.Uint64(init[i+28 : i+36]))
	}
	return int64(binary.BigEndian.Uint32(init[i+20 : i+24]))
}

// Laced AAC without a DefaultDuration: the stride measured between two
// millisecond timecodes (171 ms for eight frames, 1026 ticks) used to drive
// the whole timeline, a tick a frame slow. The timeline now runs on the
// codec's 1024-sample frame, so the audio lasts exactly its frames, in the
// full pass and in the plan alike.
func TestLacedAudioWithoutDefaultDurationKeepsCodecStride(t *testing.T) {
	src, wantFrames := buildLacedFixtureOpt(t, false)
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(ctx, src, out, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join(out, "init_a1.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mdhdDuration(t, full), int64(wantFrames)*1024; got != want {
		t.Errorf("full pass audio duration %d ticks, want %d (%d frames of 1024)", got, want, wantFrames)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	init, _, err := plan.Resource(ctx, "init_a1.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(init, full) {
		t.Errorf("plan init (duration %d) differs from the full pass (duration %d)", mdhdDuration(t, init), mdhdDuration(t, full))
	}
}
