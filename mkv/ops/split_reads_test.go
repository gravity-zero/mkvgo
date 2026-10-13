package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestSplit_ReadsTheSourceOnce: consecutive parts continue one walk of the
// source; each part used to read it again from the top (13 parts of a 1.7 GiB
// file read 12.7 GiB).
func TestSplit_ReadsTheSourceOnce(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(c, []mkv.Track{videoTrack(1), audioTrack(2)}, 60_000))
	for tc := int64(0); tc < 60_000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: make([]byte, 8<<10)},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: make([]byte, 1<<10)},
			{TrackNumber: 2, Timecode: tc + 250, Keyframe: true, Data: make([]byte, 1<<10)},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	var read int64
	fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return countingFile{f, &read}, nil
	}}
	parts, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "parts"), EveryMs: 5000,
	}, mkv.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 10 {
		t.Fatalf("got %d parts, want ~12", len(parts))
	}
	if limit := st.Size() * 11 / 10; read > limit {
		t.Errorf("Split read %d bytes of a %d-byte source for %d parts, want <= %d", read, st.Size(), len(parts), limit)
	}
}
