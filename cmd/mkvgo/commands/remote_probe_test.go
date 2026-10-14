package commands_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

// tailIndexedMKV is cuelessMKV followed by about 150 KB of Cues and a tag, both
// indexed by a SeekHead at the very end that the head SeekHead points at.
func tailIndexedMKV(n int) []byte {
	file := cuelessMKV(n)
	seg := bytes.Index(file, []byte{0x18, 0x53, 0x80, 0x67}) + 12
	seek := func(id []byte, pos uint64) []byte {
		return ebmlElem(0x4DBB, ebmlElem(0x53AB, id), ebmlUint(0x53AC, pos))
	}
	headSH := func(pos uint64) []byte { return ebmlElem(0x114D9B74, seek([]byte{0x11, 0x4D, 0x9B, 0x74}, pos)) }
	var points [][]byte
	for i := 0; i < 2200; i++ {
		points = append(points, ebmlElem(0xBB, ebmlUint(0xB3, uint64(i)*1000),
			ebmlElem(0xB7, ebmlUint(0xF7, 1), ebmlUint(0xF1, uint64(len(headSH(0)))))))
	}
	cues := ebmlElem(0x1C53BB6B, points...)
	tags := ebmlElem(0x1254C367, ebmlElem(0x7373, ebmlElem(0x63C0),
		ebmlElem(0x67C8, ebmlElem(0x45A3, []byte("TITLE")), ebmlElem(0x4487, []byte("tail test")))))
	cuesPos := uint64(len(headSH(0)) + len(file) - seg)
	tailSH := ebmlElem(0x114D9B74, seek([]byte{0x1C, 0x53, 0xBB, 0x6B}, cuesPos), seek([]byte{0x12, 0x54, 0xC3, 0x67}, cuesPos+uint64(len(cues))))
	return bytes.Join([][]byte{file[:seg], headSH(cuesPos + uint64(len(cues)+len(tags))), file[seg:], cues, tags, tailSH}, nil)
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

// cuedMKVWithFont is cuedMKV with a font of size bytes attached and a tag, written
// before the clusters as a muxer does.
func cuedMKVWithFont(t *testing.T, n, size int) []byte {
	t.Helper()
	font := make([]byte, size)
	for i := range font {
		font[i] = byte(i * 7)
	}
	return cuedMKVMeta(t, n, &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Attachments: []mkv.Attachment{{ID: 7454167688999517126, Name: "font.ttf", MIMEType: "font/ttf", Data: font}},
		Tags:        []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "font test"}}}}})
}

// cuedMKV writes n one-second clusters of one 256 KiB video keyframe each,
// with Cues and the SeekHead that points at them, as a muxer finalizes a file.
func cuedMKV(t *testing.T, n int) []byte {
	t.Helper()
	return cuedMKVMeta(t, n, &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}})
}

// cuedMKVMeta is cuedMKV with the head metadata of c.
func cuedMKVMeta(t *testing.T, n int, c *mkv.Container) []byte {
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
	if err := m.WriteMetadata(c, tracks, int64(n)*1000); err != nil {
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
		{"with a 760 KB font", cuedMKVWithFont(t, 16, 760_000), 3, 600 << 10, true},
		{"with a 100 KB font", cuedMKVWithFont(t, 16, 100_000), 2, 600 << 10, true},
		{"with a SeekHead at the end", tailIndexedMKV(16), 3, 512<<10 + 160<<10, true},
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
		if strings.Contains(c.name, "font") && (!strings.Contains(out, `"font.ttf"`) || !strings.Contains(out, `"size": 760000`) && !strings.Contains(out, `"size": 100000`) || !strings.Contains(out, `"font test"`)) {
			t.Errorf("%s: the attachment list or the tags are missing:\n%s", c.name, out)
		}
		if strings.Contains(c.name, "SeekHead") && !strings.Contains(out, `"tail test"`) {
			t.Errorf("%s: the tag is missing:\n%s", c.name, out)
		}
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

// noDefaultDurationMKV writes 2 s of a 25 fps video track that declares no frame
// rate (no DefaultDuration), in two clusters with Cues.
func noDefaultDurationMKV(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodd.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, h := uint32(64), uint32(64)
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}},
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h}}, 2000); err != nil {
		t.Fatal(err)
	}
	for c := int64(0); c < 2; c++ {
		var blocks []mkv.Block
		for i := int64(0); i < 25; i++ {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: c*1000 + i*40, Keyframe: i == 0, Data: make([]byte, 1000)})
		}
		if err := m.WriteClusterWithCues(c*1000, 1_000_000, blocks); err != nil {
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

// TestProbeEstimatedFrameRate: a video track without DefaultDuration reports the
// frame rate its first block timestamps fit, flagged as estimated, from a local
// file and over HTTP alike, the remote probe making no extra request for it.
func TestProbeEstimatedFrameRate(t *testing.T) {
	file := noDefaultDurationMKV(t)
	path := filepath.Join(t.TempDir(), "nodd.mkv")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "f.mkv", time.Time{}, bytes.NewReader(file))
	}))
	defer srv.Close()
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	for name, src := range map[string]string{"local": path, "remote": srv.URL + "/f.mkv"} {
		var got struct {
			Tracks []struct {
				FrameRate *float64 `json:"frame_rate"`
				Estimated bool     `json:"frame_rate_estimated"`
			} `json:"tracks"`
		}
		if err := json.Unmarshal([]byte(capture(t, func() { commands.CmdProbe(src) })), &got); err != nil {
			t.Fatal(err)
		}
		if v := got.Tracks[0]; v.FrameRate == nil || *v.FrameRate != 25 || !v.Estimated {
			t.Errorf("%s: frame_rate %v, frame_rate_estimated %v; want 25, true", name, v.FrameRate, v.Estimated)
		}
	}
	if n := requests.Load(); n > 2 {
		t.Errorf("the remote probe made %d requests, want 2 (the head, then the Cues)", n)
	}
}

// tailCuesMKV is cuelessMKV with Cues indexing its clusters and a tag at the end,
// and no SeekHead, as most muxers leave a file.
func tailCuesMKV(n int) []byte {
	file := cuelessMKV(n)
	seg := bytes.Index(file, []byte{0x18, 0x53, 0x80, 0x67}) + 12
	var points [][]byte
	for i, off := 0, seg; ; i++ {
		at := bytes.Index(file[off:], []byte{0x1F, 0x43, 0xB6, 0x75})
		if at < 0 {
			break
		}
		off += at
		points = append(points, ebmlElem(0xBB, ebmlUint(0xB3, uint64(i)*1000),
			ebmlElem(0xB7, ebmlUint(0xF7, 1), ebmlUint(0xF1, uint64(off-seg)))))
		off += 4
	}
	tags := ebmlElem(0x1254C367, ebmlElem(0x7373, ebmlElem(0x63C0),
		ebmlElem(0x67C8, ebmlElem(0x45A3, []byte("TITLE")), ebmlElem(0x4487, []byte("tail test")))))
	return bytes.Join([][]byte{file, ebmlElem(0x1C53BB6B, points...), tags}, nil)
}

// TestRemoteKeyframesMatchLocal: the keyframes a remote probe reports are the
// local probe's, whether the Cues come through a SeekHead or the read back from the end.
func TestRemoteKeyframesMatchLocal(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	keyframes := func(src string) []int64 {
		var got struct {
			Keyframes []int64 `json:"keyframes"`
		}
		if err := json.Unmarshal([]byte(capture(t, func() { commands.CmdProbe(src) })), &got); err != nil {
			t.Fatal(err)
		}
		return got.Keyframes
	}
	for name, file := range map[string][]byte{
		"Cues through the SeekHead":          cuedMKV(t, 16),
		"Cues at the end, no SeekHead":       tailCuesMKV(16),
		"Cues through a SeekHead at the end": tailIndexedMKV(16),
	} {
		path := filepath.Join(t.TempDir(), "f.mkv")
		if err := os.WriteFile(path, file, 0o600); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "f.mkv", time.Time{}, bytes.NewReader(file))
		}))
		local, remote := keyframes(path), keyframes(srv.URL+"/f.mkv")
		srv.Close()
		if len(local) == 0 || fmt.Sprint(local) != fmt.Sprint(remote) {
			t.Errorf("%s: %d local keyframes, %d remote; want the same, not none", name, len(local), len(remote))
		}
	}
}
