package mp4

import (
	"context"
	"io"
	"math/rand"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// bigFrameSource is 1080p-sized frames: 180 KiB keyframes every 2 s, 20 KiB others, 20 ms AAC.
func bigFrameSource(tb testing.TB, secs int) string {
	tb.Helper()
	w, h := uint32(1920), uint32(1080)
	sr, ch := 44100.0, uint8(2)
	rng := rand.New(rand.NewSource(1))
	var blocks []genBlock
	for i := 0; i < secs*25; i++ {
		key := i%50 == 0
		size := 20 << 10
		if key {
			size = 180 << 10
		}
		d := make([]byte, size)
		rng.Read(d)
		copy(d, iframeTestFrame(i, key)[:8])
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: d})
		if i%2 == 0 {
			blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 40, key: true, data: []byte{0xAA, byte(i), 1, 2, 3, 4, 5, 6}})
			blocks = append(blocks, genBlock{track: 2, pts: int64(i)*40 + 20, key: true, data: []byte{0xAB, byte(i), 1, 2, 3, 4, 5, 6}})
		}
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}
	return buildMKV(tb, tracks, blocks)
}

// plainWriter is a sink without ReadFrom: the path without sendfile.
type plainWriter struct{ writes int }

func (p *plainWriter) Write(b []byte) (int, error) { p.writes++; return len(b), nil }

// fileWriter takes the file itself, as a socket does.
type fileWriter struct{ writes int }

func (f *fileWriter) Write(b []byte) (int, error) { f.writes++; return len(b), nil }
func (f *fileWriter) ReadFrom(r io.Reader) (int64, error) {
	f.writes++
	return io.Copy(struct{ io.Writer }{io.Discard}, r)
}

func BenchmarkWriteSpans(b *testing.B) {
	src := bigFrameSource(b, 8)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 4000})
	if err != nil {
		b.Fatal(err)
	}
	h, err := plan.Open(ctx, "seg00001.m4s")
	if err != nil {
		b.Fatal(err)
	}
	b.Run("buffered", func(b *testing.B) {
		w := &plainWriter{}
		b.SetBytes(h.Size())
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := h.WriteTo(ctx, w); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(w.writes)/float64(b.N), "writes/seg")
	})
	b.Run("sendfile", func(b *testing.B) {
		w := &fileWriter{}
		b.SetBytes(h.Size())
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := h.WriteTo(ctx, w); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(w.writes)/float64(b.N), "writes/seg")
	})
}

// BenchmarkOpenAndWrite is a whole served segment: Open (the source check
// included) and the write, with the table warm.
func BenchmarkOpenAndWrite(b *testing.B) {
	src := bigFrameSource(b, 8)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 4000})
	if err != nil {
		b.Fatal(err)
	}
	w := &fileWriter{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h, err := plan.Open(ctx, "seg00001.m4s")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := h.WriteTo(ctx, w); err != nil {
			b.Fatal(err)
		}
	}
}
