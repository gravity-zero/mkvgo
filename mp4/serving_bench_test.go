package mp4

import (
	"context"
	"fmt"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildServingSource writes a PlanHLS-compatible MKV of roughly `seconds` of
// video+audio+subtitles, with a keyframe every second (so SegmentMs=1000 cuts
// one segment per second). It is the shared fixture for the serving benchmarks
// and the capacity anti-regression gates: the same builder at two sizes proves
// serving cost does not scale with source length.
func buildServingSource(tb testing.TB, seconds int) string {
	tb.Helper()
	w, h := uint32(1280), uint32(720)
	sr := 48000.0
	ch := uint8(2)
	const fps = 25
	var blocks []genBlock
	for f := 0; f < seconds*fps; f++ {
		ms := int64(f) * (1000 / fps)
		blocks = append(blocks, genBlock{track: 1, pts: ms, key: f%fps == 0, data: cencVideoSample()})
		blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: cencAudioSample(f)})
	}
	// One subtitle cue per second.
	for s := 0; s < seconds; s++ {
		blocks = append(blocks, genBlock{track: 3, pts: int64(s) * 1000, key: true, data: []byte(fmt.Sprintf("line %d", s))})
	}
	sortGenBlocks(blocks)
	return buildPlanFixture(tb,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
			{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng", Name: "English"},
		},
		blocks, nil)
}

func benchPlan(tb testing.TB, seconds int) *HLSPlan {
	tb.Helper()
	src := buildServingSource(tb, seconds)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000})
	if err != nil {
		tb.Fatal(err)
	}
	return plan
}

// BenchmarkServeSegment_HLS measures the steady-state cost of serving one media
// segment on demand from an already-built plan (the copy-rung path: compressed
// samples repackaged into fMP4, never re-encoded). Reports allocs/op and B/op -
// the machine-independent capacity signal.
func BenchmarkServeSegment_HLS(b *testing.B) {
	plan := benchPlan(b, 60)
	ctx := context.Background()
	n := plan.NumSegments()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := plan.Segment(ctx, i%n); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkServeManifest_DASH measures serving the DASH manifest (the distinct
// DASH cost; its media segments are the same CMAF set the HLS rung serves).
func BenchmarkServeManifest_DASH(b *testing.B) {
	plan := benchPlan(b, 60)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := plan.Resource(ctx, "manifest.mpd"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkServeSegment_ABRVariant serves a segment from a non-reference ABR
// variant (the video-only copy rung).
func BenchmarkServeSegment_ABRVariant(b *testing.B) {
	src := buildServingSource(b, 60)
	abr, err := PlanABR(context.Background(), []string{src, src}, Options{SegmentMs: 1000})
	if err != nil {
		b.Fatal(err)
	}
	v := abr.Variant(abr.NumVariants() - 1)
	ctx := context.Background()
	n := v.NumSegments()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.Segment(ctx, i%n); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPlanHLS measures plan construction (head-only parse + index) - the
// per-stream setup cost paid once when a client starts.
func BenchmarkPlanHLS(b *testing.B) {
	src := buildServingSource(b, 60)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := PlanHLS(ctx, src, Options{SegmentMs: 1000}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkServeConcurrent models N independent streams: each parallel worker
// holds its own plan and serves segments round-robin. Throughput here (with
// -cpu N) is the closest portable proxy for "simultaneous streams per core".
func BenchmarkServeConcurrent(b *testing.B) {
	src := buildServingSource(b, 60)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
		if err != nil {
			b.Error(err)
			return
		}
		n := plan.NumSegments()
		i := 0
		for pb.Next() {
			if _, err := plan.Segment(ctx, i%n); err != nil {
				b.Error(err)
				return
			}
			i++
		}
	})
}
