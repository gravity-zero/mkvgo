package commands_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
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
	local := filepath.Join(t.TempDir(), "nocues.mkv")
	if err := os.WriteFile(local, file, 0o644); err != nil {
		t.Fatal(err)
	}
	if lo := capture(t, func() { commands.CmdProbe(local) }); strings.Contains(lo, `"keyframes"`) != strings.Contains(out, `"keyframes"`) {
		t.Errorf("local and remote probes disagree on the keyframe index:\nlocal:\n%s\nremote:\n%s", lo, out)
	}
}

// TestRemoteProbeMetadataParity: a remote probe reports the chapters, tags and
// attachment list a local one does, without loading the attachment payloads.
func TestRemoteProbeMetadataParity(t *testing.T) {
	file, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "testdata", "meta.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "meta.mkv", time.Time{}, bytes.NewReader(file))
	}))
	defer srv.Close()

	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdProbe(srv.URL + "/meta.mkv") })
	var got struct {
		Chapters    []json.RawMessage `json:"chapters"`
		Tags        []json.RawMessage `json:"tags"`
		Attachments []struct {
			Name     string `json:"name"`
			MIMEType string `json:"mime_type"`
			Size     int64  `json:"size"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("probe -json: %v\n%s", err, out)
	}
	if len(got.Chapters) != 2 || len(got.Tags) == 0 || len(got.Attachments) != 1 {
		t.Fatalf("remote probe: %d chapters, %d tags, %d attachments, want 2, >0, 1",
			len(got.Chapters), len(got.Tags), len(got.Attachments))
	}
	if a := got.Attachments[0]; a.Name != "notes.txt" || a.MIMEType != "text/plain" || a.Size != 25 {
		t.Errorf("attachment = %+v, want notes.txt text/plain 25 bytes", a)
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

// cuedMKV writes n one-second clusters of one 256 KiB video keyframe each,
// with Cues and the SeekHead that points at them, as a muxer finalizes a file.
func cuedMKV(t *testing.T, n int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cued.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, h := uint32(64), uint32(64)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}, tracks, int64(n)*1000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		b := []mkv.Block{{TrackNumber: 1, Timecode: int64(i) * 1000, Keyframe: true, Data: make([]byte, 256<<10)}}
		if err := m.WriteClusterWithCues(int64(i)*1000, 1_000_000, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRemoteProbeReadBudget pins what a remote probe costs on a multi-MiB
// Matroska file: with Cues, the 512 KiB head and the exact range of the Cues
// the SeekHead points at; without Cues nor SeekHead, the head and one bounded
// 512 KiB tail read for the tags and chapters a muxer writes last.
func TestRemoteProbeReadBudget(t *testing.T) {
	cases := []struct {
		name     string
		file     []byte
		requests int64
		bytes    int64
		keys     bool
	}{
		{"with Cues", cuedMKV(t, 16), 2, 512<<10 + 64<<10, true},
		{"without Cues", cuelessMKV(16), 2, 600 << 10, false},
	}
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	for _, c := range cases {
		var served, requests atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.ServeContent(&countingWriter{ResponseWriter: w, n: &served}, r, "f.mkv", time.Time{}, bytes.NewReader(c.file))
		}))
		out := capture(t, func() { commands.CmdProbe(srv.URL + "/f.mkv") })
		srv.Close()
		if got := strings.Contains(out, `"keyframes"`); got != c.keys {
			t.Errorf("%s: keyframes in the JSON = %v, want %v", c.name, got, c.keys)
		}
		if requests.Load() > c.requests || served.Load() > c.bytes {
			t.Errorf("%s (%d-byte file): %d requests, %d bytes; want at most %d and %d",
				c.name, len(c.file), requests.Load(), served.Load(), c.requests, c.bytes)
		}
		t.Logf("%s: %d requests, %d bytes of %d", c.name, requests.Load(), served.Load(), len(c.file))
	}
}
