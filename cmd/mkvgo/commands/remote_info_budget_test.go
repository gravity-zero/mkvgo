package commands_test

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// boxTail is head followed by n "free" boxes of size bytes each, zeros past their headers, read without holding them.
type boxTail struct {
	head    []byte
	n, size int64
	off     int64
}

func (b *boxTail) len() int64 { return int64(len(b.head)) + b.n*b.size }

func (b *boxTail) Read(p []byte) (int, error) {
	if b.off >= b.len() {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), b.len()-b.off))
	for i := range n {
		p[i] = b.byteAt(b.off + int64(i))
	}
	b.off += int64(n)
	return n, nil
}

func (b *boxTail) byteAt(o int64) byte {
	if o < int64(len(b.head)) {
		return b.head[o]
	}
	r := (o - int64(len(b.head))) % b.size
	var h [8]byte
	binary.BigEndian.PutUint32(h[:], uint32(b.size))
	copy(h[4:], "free")
	if r < 8 {
		return h[r]
	}
	return 0
}

func (b *boxTail) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += b.off
	case io.SeekEnd:
		off += b.len()
	}
	if off < 0 {
		return 0, errors.New("negative seek")
	}
	b.off = off
	return off, nil
}

// TestRemoteInfoReadBudget: info on a remote MP4 checked for a cut by walking every top-level box, a request each
// past the read window - thousands for a fragmented film; a remote info keeps probe's budget and skips the check.
func TestRemoteInfoReadBudget(t *testing.T) {
	head, err := os.ReadFile("../../../internal/testdata/fps30.mp4")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "f.mp4", time.Time{}, &boxTail{head: head, n: 200, size: 4 << 20})
	}))
	defer srv.Close()
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	capture(t, func() { commands.CmdInfo(srv.URL + "/f.mp4") })
	if n := requests.Load(); n > 8 {
		t.Errorf("info on a remote MP4 with 200 boxes after its moov: %d requests, want a handful", n)
	}
}
