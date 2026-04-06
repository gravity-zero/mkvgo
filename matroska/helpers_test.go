package matroska

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	mkvmod "github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const fixturePath = "../internal/testdata/sample.mkv"

func assertNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertEqual[T comparable](t *testing.T, got, want T, label string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func assertDeepEqual(t *testing.T, got, want any, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s mismatch:\n  got:  %+v\n  want: %+v", label, got, want)
	}
}

func requireFixture(t *testing.T) *Container {
	t.Helper()
	c, err := Open(context.Background(), fixturePath)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	return c
}

func countBlocks(t *testing.T, path string, timecodeScale int64) map[uint64]int {
	t.Helper()
	f, err := os.Open(path)
	assertNoErr(t, err)
	defer f.Close()

	br, err := NewBlockReader(f, timecodeScale)
	assertNoErr(t, err)

	counts := map[uint64]int{}
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		assertNoErr(t, err)
		if len(blk.Data) == 0 {
			t.Errorf("block track %d at %dms: empty data", blk.TrackNumber, blk.Timecode)
		}
		counts[blk.TrackNumber]++
	}
	return counts
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	assertNoErr(t, err)
	defer in.Close()
	out, err := os.Create(dst)
	assertNoErr(t, err)
	defer out.Close()
	_, err = io.Copy(out, in)
	assertNoErr(t, err)
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func writeAndParse(t *testing.T, fn func(w io.Writer) error) *Container {
	t.Helper()
	stubInfo := SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}
	var seg bytes.Buffer
	assertNoErr(t, writer.WriteSegmentInfo(&seg, &stubInfo, 0))
	assertNoErr(t, fn(&seg))

	var buf bytes.Buffer
	assertNoErr(t, writer.WriteEBMLHeader(&buf))
	assertNoErr(t, writer.WriteMasterElement(&buf, mkvmod.IDSegment, seg.Bytes()))

	c, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "test.mkv")
	assertNoErr(t, err)
	return c
}
