package reader

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestReadStreamAttachmentHeldOnce pins that a streamed attachment costs its data, not its element body as well.
func TestReadStreamAttachmentHeldOnce(t *testing.T) {
	const size = 16 << 20
	data := bytes.Repeat([]byte{0xA5}, size)
	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDAttachments, masterElem(mkv.IDAttachedFile,
		strElem(mkv.IDFileName, "a.bin"), bytesElem(mkv.IDFileData, data))))
	file := streamFixture(seg.Bytes())

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	c, _, err := ReadStream(context.Background(), bytes.NewReader(file))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Attachments) != 1 || !bytes.Equal(c.Attachments[0].Data, data) {
		t.Fatal("the attachment did not come back whole")
	}
	// The data's own read grows its buffer by doubling (about twice its size); the element body must add nothing.
	if got := after.TotalAlloc - before.TotalAlloc; got > size*3 {
		t.Errorf("reading a %d-byte attachment allocated %d bytes", size, got)
	}
}

// TestBodyReaderSeeks pins a bodyReader's seeks against a bytes.Reader's over the same body.
func TestBodyReaderSeeks(t *testing.T) {
	body := make([]byte, 300)
	for i := range body {
		body[i] = byte(i)
	}
	steps := []struct {
		off    int64
		whence int
		read   int
	}{{0, io.SeekCurrent, 20}, {-12, io.SeekCurrent, 5}, {100, io.SeekStart, 30}, {90, io.SeekStart, 3}, {10, io.SeekCurrent, 1}, {400, io.SeekStart, 4}, {280, io.SeekStart, 4}}
	want, got := bytes.NewReader(body), &bodyReader{r: bytes.NewReader(body)}
	for i, s := range steps {
		wp, werr := want.Seek(s.off, s.whence)
		gp, gerr := got.Seek(s.off, s.whence)
		if wp != gp || (werr == nil) != (gerr == nil) {
			t.Fatalf("step %d: seek = %d, %v; want %d, %v", i, gp, gerr, wp, werr)
		}
		wb, gb := make([]byte, s.read), make([]byte, s.read)
		wn, _ := io.ReadFull(want, wb)
		gn, _ := io.ReadFull(got, gb)
		if wn != gn || !bytes.Equal(wb[:wn], gb[:gn]) {
			t.Fatalf("step %d: read %x, want %x", i, gb[:gn], wb[:wn])
		}
	}
	if _, err := got.Seek(0, io.SeekStart); err == nil {
		t.Error("a seek back past the lookback succeeded")
	}
}
