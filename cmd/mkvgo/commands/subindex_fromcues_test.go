package commands_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

type countedFile struct {
	*os.File
	n *int64
}

func (f countedFile) Read(p []byte) (int, error) {
	k, err := f.File.Read(p)
	*f.n += int64(k)
	return k, err
}

func (f countedFile) ReadAt(p []byte, off int64) (int, error) {
	k, err := f.File.ReadAt(p, off)
	*f.n += int64(k)
	return k, err
}

// cuedSubtitleMKV writes 40 one-second clusters of 64 KiB video frames with a
// subtitle block in each, every subtitle block cued with its relative position
// and the track's NUMBER_OF_FRAMES statistic trusted.
func cuedSubtitleMKV(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cued.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, h := uint32(320), uint32(240)
	const clusters = 40
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}, Tags: []mkv.Tag{{TargetID: 22, SimpleTags: []mkv.SimpleTag{
		{Name: "_STATISTICS_WRITING_APP", Value: "test"}, {Name: "DURATION", Value: "00:00:39.600000000"}, {Name: "NUMBER_OF_FRAMES", Value: "40"}}}}}
	tracks := []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
		{ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: "srt"},
	}
	if err := mw.WriteMetadata(c, tracks, clusters*1000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < clusters; i++ {
		ts := int64(i) * 1000
		blocks := []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: append([]byte{0, 0, 1, 0x2D, 0x65}, make([]byte, 64<<10)...)},
			{TrackNumber: 2, Timecode: ts + 100, Keyframe: true, Duration: 500, Data: []byte("cue")},
			{TrackNumber: 1, Timecode: ts + 500, Data: append([]byte{0, 0, 1, 0x2D, 0x41}, make([]byte, 64<<10)...)},
		}
		clusterPos := mw.RelPos()
		offsets, err := writer.WriteClusterOffsets(f, ts, 1000000, blocks)
		if err != nil {
			t.Fatal(err)
		}
		for j, b := range blocks {
			if b.Keyframe {
				mw.Cues = append(mw.Cues, mkv.CuePoint{TimeMs: b.Timecode, Track: b.TrackNumber, ClusterPos: clusterPos, RelativePos: offsets[j]})
			}
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// subtitle-index -from-cues reads the metadata and one cluster header per cued
// cluster, never the media: it used to run the full walk first and throw it away.
func TestSubtitleIndexFromCuesReadsNoMedia(t *testing.T) {
	src := cuedSubtitleMKV(t)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	read := func(fromCues bool) (*matroska.SubtitleIndex, int64) {
		var n int64
		fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			return countedFile{f, &n}, nil
		}}
		ix, err := commands.BuildSubtitleIndex(src, nil, fromCues, matroska.Options{FS: fs})
		if err != nil {
			t.Fatalf("fromCues=%v: %v", fromCues, err)
		}
		return ix, n
	}
	walked, walkBytes := read(false)
	derived, cueBytes := read(true)
	if !reflect.DeepEqual(derived.TrackBlocks(2), walked.TrackBlocks(2)) {
		t.Fatalf("the derived index differs from the walk")
	}
	t.Logf("file %d bytes, walk read %d, from-cues read %d", st.Size(), walkBytes, cueBytes)
	if cueBytes > st.Size()/4 {
		t.Errorf("-from-cues read %d bytes of a %d-byte file: the media was walked", cueBytes, st.Size())
	}
}
