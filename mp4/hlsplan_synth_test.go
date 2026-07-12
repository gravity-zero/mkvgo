package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildNoCueMKV writes the same seekable layout as buildMKV but with NO Cues
// element: clusters go through writer.WriteCluster, which records nothing.
func buildNoCueMKV(t testing.TB, tracks []mkv.Track, blocks []genBlock) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nocue.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	var blks []mkv.Block
	var durationMs int64
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
		if gb.pts > durationMs {
			durationMs = gb.pts
		}
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, durationMs); err != nil {
		f.Close()
		t.Fatal(err)
	}
	start := 0
	for i := 1; i <= len(blks); i++ {
		if i == len(blks) || blks[i].Timecode-blks[start].Timecode >= 1000 {
			if err := writer.WriteCluster(f, blks[start].Timecode, scale, blks[start:i]); err != nil {
				f.Close()
				t.Fatal(err)
			}
			start = i
		}
	}
	if err := m.Finalize(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func synthFixtureBlocks(audioDelayMs int64) []genBlock {
	var gblocks []genBlock
	for i := 0; i < 150; i++ { // video, 40ms frames, keyframe every 1s
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 300; i++ { // audio, 20ms frames
		gblocks = append(gblocks, genBlock{track: 2, pts: audioDelayMs + int64(i)*20, key: true,
			data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	return gblocks
}

func synthFixtureTracks() []mkv.Track {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	return []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}
}

// TestPlanHLSSynthesizeIndex: a no-Cue source refuses by default (with the
// option named in the error) and, with SynthesizeIndex, serves the exact
// output the full pass produces - the synthesized in-memory index stands in
// for the missing one without writing anything.
func TestPlanHLSSynthesizeIndex(t *testing.T) {
	ctx := context.Background()
	src := buildNoCueMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))

	_, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err == nil {
		t.Fatal("a no-Cue source must refuse without SynthesizeIndex")
	}
	if !strings.Contains(err.Error(), "SynthesizeIndex") {
		t.Errorf("the refusal must name the option: %v", err)
	}

	// The full pass has no index dependency: it is the reference output.
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, SynthesizeIndex: true})
	if err != nil {
		t.Fatal(err)
	}

	fullSegs, _ := filepath.Glob(filepath.Join(dir, "seg0*.m4s"))
	if plan.NumSegments() != len(fullSegs) {
		t.Fatalf("plan has %d segments, full pass wrote %d", plan.NumSegments(), len(fullSegs))
	}
	for name, got := range map[string][]byte{
		"init.mp4":      plan.InitSegment(),
		"playlist.m3u8": plan.MediaPlaylist(),
	} {
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
	for n := 0; n < plan.NumSegments(); n++ {
		got, err := plan.Segment(ctx, n)
		if err != nil {
			t.Fatalf("Segment(%d): %v", n, err)
		}
		want, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("segment %d differs from the full pass (%d vs %d bytes)", n, len(got), len(want))
		}
	}
}

// TestHLSAudioPresentationShift: the shift re-bases the audio's edit list in
// the init segment - media segments stay byte-identical, the video init is
// untouched, full pass and plan agree, and an over-shift clamps to 0.
func TestHLSAudioPresentationShift(t *testing.T) {
	ctx := context.Background()
	// Audio content starts 300ms after the video (the classic repack defect).
	src := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(300))
	shift := map[uint64]int64{2: 300_000_000} // cancel it (ns, AudioStartDelays' shape)

	plain, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	shifted, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, AudioPresentationShift: shift})
	if err != nil {
		t.Fatal(err)
	}

	audioInit := func(p *HLSPlan) []byte {
		data, _, err := p.Resource(ctx, "init_a1.mp4")
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if bytes.Equal(audioInit(plain), audioInit(shifted)) {
		t.Error("the shift must change the audio init segment (edit list)")
	}
	if !bytes.Equal(plain.InitSegment(), shifted.InitSegment()) {
		t.Error("the video init must not change")
	}
	for n := 0; n < plain.NumSegments(); n++ {
		a, err := plain.Segment(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		b, err := shifted.Segment(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("media segment %d must be byte-identical under the shift", n)
		}
	}

	// Full pass parity: the same shift yields the same audio init.
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 2000, AudioPresentationShift: shift}); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(dir, "init_a1.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(audioInit(shifted), want) {
		t.Error("plan and full pass disagree on the shifted audio init")
	}

	// Clamp: shifting further back than the presentation start lands at 0,
	// the same init an exactly-cancelling shift on a 0-delayed source gets.
	over, err := PlanHLS(ctx, src, Options{SegmentMs: 2000,
		AudioPresentationShift: map[uint64]int64{2: 5_000_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	exact := audioInit(shifted) // 300ms delay - 300ms shift = offset 0
	if !bytes.Equal(audioInit(over), exact) {
		t.Error("an over-shift must clamp to presentation start (offset 0)")
	}
}
