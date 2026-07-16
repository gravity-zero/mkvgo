package ops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// retime_errors_test.go - every retime refusal must be routable with
// errors.Is: a caller that repairs a library in bulk needs "permanent for
// this call, do not retry" as a type, not as message text to parse.

// retimeErrFixture builds the standard two-track fixture the refusal cases
// share: 4 clusters, video on 1, audio on 2 starting at the video's time.
func retimeErrFixture(t *testing.T, dir string) string {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{0x01}},
		})
	}
	return buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 4000)
}

// TestRetime_TypedRefusals drives each permanent refusal through the public
// engines and asserts its sentinel.
func TestRetime_TypedRefusals(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		shift map[uint64]int64
		run   func(ctx context.Context, path string, shift map[uint64]int64) error
		want  error
	}{
		{
			name:  "unknown track",
			shift: map[uint64]int64{9: -300_000_000},
			run: func(ctx context.Context, p string, s map[uint64]int64) error {
				return RetimeTracksReplace(ctx, p, s)
			},
			want: ErrUnknownTrack,
		},
		{
			name:  "below timecode resolution",
			shift: map[uint64]int64{2: 1},
			run: func(ctx context.Context, p string, s map[uint64]int64) error {
				return RetimeTracksReplace(ctx, p, s)
			},
			want: ErrShiftNotRepresentable,
		},
		{
			name:  "not a whole number of ticks",
			shift: map[uint64]int64{2: 1_500_000},
			run: func(ctx context.Context, p string, s map[uint64]int64) error {
				return RetimeTracksReplace(ctx, p, s)
			},
			want: ErrShiftNotRepresentable,
		},
		{
			name:  "past int16 relative range",
			shift: map[uint64]int64{2: 40_000_000_000},
			run: func(ctx context.Context, p string, s map[uint64]int64) error {
				return RetimeTracksReplace(ctx, p, s)
			},
			want: ErrShiftOutOfRange,
		},
		{
			name:  "negative absolute timestamp",
			shift: map[uint64]int64{2: -1_000_000_000},
			run: func(ctx context.Context, p string, s map[uint64]int64) error {
				return RetimeTracksReplace(ctx, p, s)
			},
			want: ErrShiftOutOfRange,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := retimeErrFixture(t, dir)
			err := tc.run(ctx, src, tc.shift)
			if err == nil {
				t.Fatal("want a refusal, got success")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("errors.Is(%v) = false for: %v", tc.want, err)
			}
		})
	}
}

// TestRetime_TrackHasNoBlocksTyped: a declared track with no blocks refuses
// with ErrTrackHasNoBlocks on both engines.
func TestRetime_TrackHasNoBlocksTyped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2), audioTrack(3)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{0x01}},
		})
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 4000)

	for name, run := range map[string]func() error{
		"replace": func() error { return RetimeTracksReplace(ctx, src, map[uint64]int64{3: -300_000_000}) },
		"inplace": func() error { return RetimeTracksInPlace(ctx, src, map[uint64]int64{3: -300_000_000}) },
	} {
		err := run()
		if err == nil {
			t.Fatalf("%s: want a refusal, got success", name)
		}
		if !errors.Is(err, ErrTrackHasNoBlocks) {
			t.Errorf("%s: errors.Is(ErrTrackHasNoBlocks) = false for: %v", name, err)
		}
	}
}

// TestRetime_UnknownSizeSegmentTyped: the in-place engine refuses a streamed
// (unknown-size) Segment with the exported sentinel, and the automatic mode
// routes the very same file to the rewrite and succeeds.
func TestRetime_UnknownSizeSegmentTyped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := retimeErrFixture(t, dir)

	unsized := filepath.Join(dir, "streamed.mkv")
	writeAll(t, unsized, makeSegmentStreamed(t, readAll(t, src)))

	err := RetimeTracksInPlace(ctx, unsized, map[uint64]int64{2: -1_000_000})
	if err == nil {
		t.Fatal("in place on a streamed Segment: want a refusal, got success")
	}
	if !errors.Is(err, ErrUnknownSizeSegment) {
		t.Errorf("errors.Is(ErrUnknownSizeSegment) = false for: %v", err)
	}

	// Shift audio later so the automatic mode has a valid shift to apply
	// (the fixture's audio starts at 0; earlier would go negative).
	if err := RetimeTracks(ctx, unsized, map[uint64]int64{2: 100_000_000}); err != nil {
		t.Errorf("the automatic mode must route a streamed Segment to the rewrite: %v", err)
	}
}

// TestRetime_CorruptSourceTyped: corruption INSIDE the declared Segment
// keeps refusing the rewrite, now typed - the caller repairs (resync) and
// retries instead of retrying blindly.
func TestRetime_CorruptSourceTyped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := retimeErrFixture(t, dir)
	data := readAll(t, src)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if len(offsets) < 3 {
		t.Fatalf("fixture has only %d clusters", len(offsets))
	}
	corrupt := append([]byte{}, data...)
	for i := int64(0); i < 8; i++ {
		corrupt[offsets[2]+i] = 0x00
	}
	path := filepath.Join(dir, "corrupt.mkv")
	writeAll(t, path, corrupt)

	err := RetimeTracksReplace(ctx, path, map[uint64]int64{2: 100_000_000})
	if err == nil {
		t.Fatal("want a refusal on mid-file corruption, got success")
	}
	if !errors.Is(err, ErrCorruptSource) {
		t.Errorf("errors.Is(ErrCorruptSource) = false for: %v", err)
	}
}
