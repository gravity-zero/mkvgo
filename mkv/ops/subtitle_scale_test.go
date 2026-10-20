package ops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// scaleSubtitleMKV writes a subtitle track of n blocks, one a second and none stating a duration, in 10 s clusters.
func scaleSubtitleMKV(t testing.TB, codec string, n int, data func(i int) []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scale.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}}
	if err := mw.WriteMetadata(c, []mkv.Track{subtitleTrack(1, codec)}, int64(n)*1000); err != nil {
		t.Fatal(err)
	}
	for at := 0; at < n; at += 10 {
		var blocks []mkv.Block
		for i := at; i < min(at+10, n); i++ {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 1000, Keyframe: true, Data: data(i)})
		}
		if err := mw.WriteClusterWithCues(int64(at)*1000, 1000000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The text extractions end each cue that states none on the one after: a track four times as long costs about four
// times as much, whatever the format.
func TestSubtitleExtractionScalesLinearly(t *testing.T) {
	ctx := context.Background()
	text := func(i int) []byte { return []byte(fmt.Sprintf("cue %d, a line of a usual length", i)) }
	t.Run("srt", func(t *testing.T) {
		costguard.Linear(t, 20000, func(n int) time.Duration {
			src, out := scaleSubtitleMKV(t, "srt", n, text), filepath.Join(t.TempDir(), "out.srt")
			start := time.Now()
			if err := ExtractSubtitle(ctx, src, 1, out); err != nil {
				t.Fatal(err)
			}
			return costguard.Since(start)
		})
	})
	t.Run("webvtt", func(t *testing.T) {
		costguard.Linear(t, 40000, func(n int) time.Duration {
			src := scaleSubtitleMKV(t, "srt", n, text)
			start := time.Now()
			if err := ExtractSubtitleWebVTT(ctx, src, 1, io.Discard); err != nil {
				t.Fatal(err)
			}
			return costguard.Since(start)
		})
	})
	t.Run("pgs", func(t *testing.T) {
		costguard.Linear(t, 80000, func(n int) time.Duration {
			src := scaleSubtitleMKV(t, "pgs", n, func(i int) []byte {
				if i%2 == 1 {
					return pgsClear()
				}
				return pgsShow(1, 100, 900, i%20 == 0)
			})
			start := time.Now()
			if err := ForEachSubtitlePGS(ctx, src, 1, func(PGSCue) error { return nil }); err != nil {
				t.Fatal(err)
			}
			return costguard.Since(start)
		})
	})
}
