package commands_test

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// ebmlElem encodes one EBML element with an 8-byte size.
func ebmlElem(id uint32, payload ...[]byte) []byte {
	var idb []byte
	for s := 24; s >= 0; s -= 8 {
		if b := byte(id >> s); b != 0 || len(idb) > 0 {
			idb = append(idb, b)
		}
	}
	body := bytes.Join(payload, nil)
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(body)))
	size[0] = 0x01
	return append(append(idb, size...), body...)
}

func ebmlUint(id uint32, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return ebmlElem(id, b)
}

// cuelessMKV builds an unknown-size Segment with Info, Tracks and n Clusters of
// one 256 KiB video keyframe each, and no Cues.
func cuelessMKV(n int) []byte {
	head := ebmlElem(0x1A45DFA3, ebmlElem(0x4282, []byte("matroska")), ebmlUint(0x4287, 4), ebmlUint(0x4285, 2))
	info := ebmlElem(0x1549A966, ebmlUint(0x2AD7B1, 1_000_000), ebmlElem(0x4D80, []byte("t")), ebmlElem(0x5741, []byte("t")))
	tracks := ebmlElem(0x1654AE6B, ebmlElem(0xAE,
		ebmlUint(0xD7, 1), ebmlUint(0x73C5, 1), ebmlUint(0x83, 1), ebmlElem(0x86, []byte("V_VP9")),
		ebmlElem(0xE0, ebmlUint(0xB0, 64), ebmlUint(0xBA, 64))))
	body := [][]byte{info, tracks}
	for i := 0; i < n; i++ {
		block := append([]byte{0x81, 0, 0, 0x80}, make([]byte, 256<<10)...)
		body = append(body, ebmlElem(0x1F43B675, ebmlUint(0xE7, uint64(i*1000)), ebmlElem(0xA3, block)))
	}
	seg := append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, bytes.Join(body, nil)...)
	return append(head, seg...)
}

// TestRemoteProbeCuelessReadsHeadOnly: a remote probe of a Matroska file without
// Cues reads the head, not every Cluster for a keyframe index (it downloaded the
// whole file).
func TestRemoteProbeCuelessReadsHeadOnly(t *testing.T) {
	file := cuelessMKV(16)
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingWriter{ResponseWriter: w, n: &served}
		http.ServeContent(cw, r, "nocues.mkv", time.Time{}, bytes.NewReader(file))
	}))
	defer srv.Close()

	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdProbe(srv.URL + "/nocues.mkv") })
	if !strings.Contains(out, `"V_VP9"`) && !strings.Contains(out, `"vp9"`) {
		t.Fatalf("probe did not report the video track:\n%s", out)
	}
	if n := served.Load(); n >= 2<<20 {
		t.Errorf("remote probe read %d bytes of a %d-byte file without Cues, want < 2 MiB", n, len(file))
	}
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}
