package mp4

import (
	"bytes"
	"context"
	"testing"
)

// TestCopyParallelRunsOutOfOrder: runs whose source offsets go back (chunks stored out of order) were gathered into a
// read that starts after them, and sliced before its start. They are copied in turn, in the order given.
func TestCopyParallelRunsOutOfOrder(t *testing.T) {
	src := make([]byte, 4096)
	for i := range src {
		src[i] = byte(i * 7)
	}
	pieces := []span{{off: 2000, n: 100}, {emit: []byte("hdr")}, {off: 1000, n: 50}, {off: 1060, n: 40}, {off: 10, n: 30}}
	var want []byte
	for _, pc := range pieces {
		if pc.emit != nil {
			want = append(want, pc.emit...)
			continue
		}
		want = append(want, src[pc.off:pc.off+pc.n]...)
	}
	var out bytes.Buffer
	c := &spanCopier{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("copyParallel panicked: %v", r)
			}
		}()
		if err := c.copyParallel(context.Background(), &out, pieces, bytes.NewReader(src), 4096, 1<<20, 4); err != nil {
			t.Fatal(err)
		}
	}()
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("copied %d bytes, not the %d of the runs in order", out.Len(), len(want))
	}
}
