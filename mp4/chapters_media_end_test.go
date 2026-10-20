package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// chapterTrackSpan is the first and last samples of the file's QuickTime chapter track.
func chapterTrackSpan(t *testing.T, path string) (n int, last inSample) {
	t.Helper()
	boxes := moovBoxesOf(t, path)
	ids := chapterTrackRefs(boxes)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		if tkhd, err := boxAt(b.payload, "tkhd"); b.typ == "trak" && err == nil && ids[tkhdTrackID(tkhd)] {
			ss := chapterTrackSamples(b.payload, fi.Size())
			if len(ss) > 0 {
				return len(ss), ss[len(ss)-1]
			}
		}
	}
	return 0, inSample{}
}

// The chpl list and the QuickTime chapter track of a remux are one list that ends with the media: a chapter starting
// past it is in neither, the last one runs to it, and the movie lasts as long as its media.
func TestRemuxChaptersEndWithTheMedia(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	var blocks []genBlock
	for i := 0; i < 250; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{byte(i)}})
	}
	for name, chapters := range map[string][]mkv.Chapter{
		"past the end":     {{StartMs: 0, Title: "A"}, {StartMs: 4000, Title: "B"}, {StartMs: 30_000, Title: "Past"}},
		"a short last end": {{StartMs: 0, Title: "A"}, {StartMs: 4000, EndMs: 5000, Title: "B"}},
		"no last end":      {{StartMs: 0, Title: "A"}, {StartMs: 9900, Title: "B"}},
		"past the duration the source declares, within its media": {{StartMs: 0, Title: "A"}, {StartMs: 9980, Title: "B"}},
	} {
		t.Run(name, func(t *testing.T) {
			src := buildMKVWithChapters(t, tracks, blocks, chapters)
			dst := filepath.Join(t.TempDir(), "o.mp4")
			if err := RemuxToMP4(context.Background(), src, dst); err != nil {
				t.Fatal(err)
			}
			m := mustMeta(t, dst)
			if m.DurationMs != 10_000 {
				t.Errorf("the movie lasts %d ms, its media 10000", m.DurationMs)
			}
			if len(m.Chapters) != 2 || m.Chapters[0].Title != "A" || m.Chapters[1].Title != "B" {
				t.Errorf("chpl lists %+v, want A and B", m.Chapters)
			}
			if n, last := chapterTrackSpan(t, dst); n != 2 || last.ctsMs+last.durMs != 10_000 {
				t.Errorf("the chapter track holds %d chapters, the last ending at %d ms; want 2, ending at 10000", n, last.ctsMs+last.durMs)
			}
		})
	}
}
