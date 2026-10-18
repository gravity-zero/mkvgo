package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// webvttCountingFS is os's filesystem counting the bytes read through it.
func webvttCountingFS(n *int64) *mkv.FS {
	return &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return &webvttCountedFile{File: f, n: n}, nil
	}, Stat: os.Stat}
}

type webvttCountedFile struct {
	*os.File
	n *int64
}

func (f *webvttCountedFile) Read(p []byte) (int, error) {
	k, err := f.File.Read(p)
	*f.n += int64(k)
	return k, err
}

func (f *webvttCountedFile) ReadAt(p []byte, off int64) (int, error) {
	k, err := f.File.ReadAt(p, off)
	*f.n += int64(k)
	return k, err
}

// webvttFixture writes 40 s of video in 1 s clusters with large frames and a subtitle cue every 2 s (20 in all);
// cueSub picks the subtitle blocks the Cues name (nil: all), frames is the NUMBER_OF_FRAMES statistic ("" for none).
func webvttFixture(t *testing.T, dir, name string, cueSub func(i int) bool, frames string) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var tags []mkv.Tag
	if frames != "" {
		tags = []mkv.Tag{{TargetID: 22, SimpleTags: []mkv.SimpleTag{{Name: "_STATISTICS_WRITING_APP", Value: "test"}, {Name: "DURATION", Value: "00:00:38.200000000"}, {Name: "NUMBER_OF_FRAMES", Value: frames}}}}
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}, Tags: tags},
		[]mkv.Track{{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h}, {ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: "srt"}}, 40000))
	sub := 0
	for c := 0; c < 40; c++ {
		var set []mkv.Block
		for i := 0; i < 25; i++ {
			tc := int64(c*1000 + i*40)
			set = append(set, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: i == 0, Data: append([]byte{0, 0, 1, 0x2D, 0x65}, make([]byte, 8000)...)})
			if i == 5 && c%2 == 0 {
				set = append(set, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Duration: 900, Data: []byte("cue " + string(rune('a'+sub)))})
			}
		}
		clusterPos := mw.RelPos()
		offsets, err := writer.WriteClusterOffsets(f, set[0].Timecode, 1000000, set)
		mustNil(t, err)
		for k, b := range set {
			cued := b.Keyframe && b.TrackNumber == 1
			if b.TrackNumber == 2 {
				cued = cueSub == nil || cueSub(sub)
				sub++
			}
			if cued {
				mw.Cues = append(mw.Cues, mkv.CuePoint{TimeMs: b.Timecode, Track: b.TrackNumber, ClusterPos: clusterPos, RelativePos: offsets[k]})
			}
		}
	}
	mustNil(t, mw.Finalize())
	return path
}

// TestWebVTTServedFromVerifiedCues pins that extraction takes the source's own Cues when they are verified
// complete for the track, reading a fraction of the file, and walks otherwise; the WebVTT is the same either way.
func TestWebVTTServedFromVerifiedCues(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	walk := func(path string) []byte {
		var b bytes.Buffer
		if err := extractSubtitleWebVTTWalk(ctx, path, 2, &b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	for _, c := range []struct {
		name, frames string
		indexed      bool
	}{
		{"verified cues", "20", true},
		{"no NUMBER_OF_FRAMES", "", false},
		{"a count the cues do not reach", "21", false},
	} {
		path := webvttFixture(t, dir, c.name+".mkv", nil, c.frames)
		var read int64
		var got bytes.Buffer
		if err := ExtractSubtitleWebVTT(ctx, path, 2, &got, mkv.Options{FS: webvttCountingFS(&read)}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := walk(path); !bytes.Equal(got.Bytes(), want) {
			t.Errorf("%s: WebVTT differs from the walk's:\n%s\nwant\n%s", c.name, got.Bytes(), want)
		}
		st, _ := os.Stat(path)
		if indexed := read < st.Size()/4; indexed != c.indexed {
			t.Errorf("%s: read %d of %d bytes, indexed %v; want %v", c.name, read, st.Size(), indexed, c.indexed)
		}
	}
}

// TestWebVTTWithPartialCuesLosesNoCue pins that cues naming only some of a track's blocks never serve: the walk
// does, and every cue is there.
func TestWebVTTWithPartialCuesLosesNoCue(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	for name, cueSub := range map[string]func(int) bool{"half cued": func(i int) bool { return i%2 == 0 }, "none cued": func(int) bool { return false }} {
		// The statistic says 20, so a count of 10 or 0 cues is not verified: the walk serves.
		path := webvttFixture(t, dir, name+".mkv", cueSub, "20")
		var got bytes.Buffer
		if err := ExtractSubtitleWebVTT(ctx, path, 2, &got); err != nil {
			t.Fatal(err)
		}
		if n := bytes.Count(got.Bytes(), []byte("-->")); n != 20 {
			t.Errorf("%s: %d cues out of 20", name, n)
		}
	}
}

// TestWebVTTOfADamagedOrCutSource pins what a source with missing bytes gives, the same through the Cues and the
// walk: a zeroed hole within DamageBytes is passed over, a larger one refused with nothing written, and a file
// that stops before its declared end (a download not finished) writes the cues it holds and says so.
func TestWebVTTOfADamagedOrCutSource(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	data, err := os.ReadFile(webvttFixture(t, dir, "src.mkv", nil, "20"))
	if err != nil {
		t.Fatal(err)
	}
	hole := filepath.Join(dir, "hole.mkv")
	zeroed := append([]byte(nil), data...)
	for i := len(zeroed) * 3 / 10; i < len(zeroed)*5/10; i++ {
		zeroed[i] = 0
	}
	mustNil(t, os.WriteFile(hole, zeroed, 0o644))
	for _, c := range []struct {
		bound   int64
		refused bool
	}{{0, false}, {4096, true}} {
		var got, walked bytes.Buffer
		err := ExtractSubtitleWebVTT(ctx, hole, 2, &got, mkv.Options{DamageBytes: c.bound})
		werr := extractSubtitleWebVTTWalk(ctx, hole, 2, &walked, mkv.Options{DamageBytes: c.bound})
		if (err != nil) != c.refused || (werr != nil) != c.refused || !bytes.Equal(got.Bytes(), walked.Bytes()) {
			t.Errorf("hole, bound %d: err %v, walk err %v, same output %v; refused want %v", c.bound, err, werr, bytes.Equal(got.Bytes(), walked.Bytes()), c.refused)
		}
		if c.refused && got.Len() != 0 {
			t.Errorf("hole, bound %d: a refusal wrote %d bytes", c.bound, got.Len())
		}
	}
	cut := filepath.Join(dir, "cut.mkv")
	mustNil(t, os.WriteFile(cut, data[:len(data)*6/10], 0o644))
	var got bytes.Buffer
	err = ExtractSubtitleWebVTT(ctx, cut, 2, &got)
	if !errors.Is(err, ErrSourceTruncated) {
		t.Errorf("cut source: err = %v, want ErrSourceTruncated", err)
	}
	if n := bytes.Count(got.Bytes(), []byte("-->")); n == 0 || n >= 20 {
		t.Errorf("cut source: %d cues written, want those before the cut", n)
	}
}
