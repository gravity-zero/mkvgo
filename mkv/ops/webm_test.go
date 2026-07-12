package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildSource writes a small seekable MKV (one video track + a 2-block cluster)
// to path, using the given codec.
func buildSource(t *testing.T, path, codec string) {
	t.Helper()
	w, h := uint32(320), uint32(240)
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: codec, Width: &w, Height: &h}}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01, 0x02, 0x03}},
		{TrackNumber: 1, Timecode: 40, Keyframe: false, Data: []byte{0x04, 0x05}},
	}

	var seg bytes.Buffer
	if err := writer.WriteSegmentInfo(&seg, &info, 0); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTracks(&seg, tracks); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteCluster(&seg, 0, info.TimecodeScale, blocks); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writer.WriteEBMLHeader(&buf); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemuxToWebM(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "out.webm")
	buildSource(t, src, "vp9")

	if err := RemuxToWebM(context.Background(), src, dst); err != nil {
		t.Fatalf("RemuxToWebM: %v", err)
	}

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw[:64], []byte("webm")) {
		t.Error("output DocType is not webm")
	}

	// The output is an unknown-size stream - read it back with ReadStream.
	c, br, err := reader.ReadStream(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStream output: %v", err)
	}
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "vp9" {
		t.Errorf("tracks = %+v, want one vp9 track", c.Tracks)
	}
	if err := mkv.ValidateWebM(c); err != nil {
		t.Errorf("remuxed output is not WebM-valid: %v", err)
	}
	n := 0
	for {
		_, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("block read: %v", err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("remuxed %d blocks, want 2", n)
	}
}

func TestRemuxToWebMRejectsNonWebMCodec(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "out.webm")
	buildSource(t, src, "h264")

	if err := RemuxToWebM(context.Background(), src, dst); err == nil {
		t.Fatal("RemuxToWebM accepted an h264 source")
	}
}

// TestRemuxToWebMStripsChapters verifies the WebM element-subset behaviour: a
// source carrying a Chapter (not part of the WebM streaming output) is remuxed
// to a WebM that contains no chapters, and WebMNonSubsetElements flags the loss.
func TestRemuxToWebMStripsChapters(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "out.webm")

	w, h := uint32(320), uint32(240)
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h}}
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{1, 2, 3}}}
	chapters := []mkv.Chapter{{ID: 1, Title: "intro", StartMs: 0}}

	var seg bytes.Buffer
	mustNil(t, writer.WriteSegmentInfo(&seg, &info, 0))
	mustNil(t, writer.WriteTracks(&seg, tracks))
	mustNil(t, writer.WriteChapters(&seg, chapters))
	mustNil(t, writer.WriteCluster(&seg, 0, info.TimecodeScale, blocks))
	var buf bytes.Buffer
	mustNil(t, writer.WriteEBMLHeader(&buf))
	mustNil(t, writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()))
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	sc, err := reader.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Chapters) != 1 {
		t.Fatalf("source build: expected 1 chapter, got %d", len(sc.Chapters))
	}
	if got := mkv.WebMNonSubsetElements(sc); len(got) != 1 || got[0] != "Chapters" {
		t.Errorf("WebMNonSubsetElements = %v, want [Chapters]", got)
	}

	if err := RemuxToWebM(context.Background(), src, dst); err != nil {
		t.Fatalf("RemuxToWebM: %v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := reader.ReadStream(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(out.Chapters) != 0 {
		t.Errorf("WebM output should carry no chapters, got %d", len(out.Chapters))
	}
}

// TestRemuxToWebMClustersByTime guards against the keyframe-per-cluster trap:
// every Opus audio frame is a keyframe, so RemuxToWebM must group by time, not
// open a fresh cluster per block.
func TestRemuxToWebMClustersByTime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "out.webm")

	opusHead := append([]byte("OpusHead"), 0x01, 0x02, 0x38, 0x01, 0x80, 0xbb, 0x00, 0x00, 0x00, 0x00, 0x00)
	sr := 48000.0
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead, SampleRate: &sr}}
	var blocks []mkv.Block
	for i := 0; i < 100; i++ { // 100 "keyframe" frames over 2000 ms
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 20, Keyframe: true, Data: []byte{0xAA}})
	}

	var seg bytes.Buffer
	mustNil(t, writer.WriteSegmentInfo(&seg, &info, 0))
	mustNil(t, writer.WriteTracks(&seg, tracks))
	mustNil(t, writer.WriteCluster(&seg, 0, info.TimecodeScale, blocks))
	var buf bytes.Buffer
	mustNil(t, writer.WriteEBMLHeader(&buf))
	mustNil(t, writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()))
	mustNil(t, os.WriteFile(src, buf.Bytes(), 0o644))

	mustNil(t, RemuxToWebM(context.Background(), src, dst))

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	clusters := bytes.Count(raw, []byte{0x1F, 0x43, 0xB6, 0x75})
	if clusters < 2 || clusters > 4 {
		t.Errorf("got %d clusters for 100 keyframe frames over 2s, want time-based (~2-3, not per-frame)", clusters)
	}
}

// The remuxed WebM is a SEEKABLE file: known-size clusters readable by the
// seekable reader, a Cues index, DocType "webm", and a verbatim block copy.
func TestRemuxToWebMSeekableOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "out.webm")

	w, h := uint32(320), uint32(240)
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h}}
	var blocks []mkv.Block
	for i := 0; i < 60; i++ { // 3s of video, keyframe every second
		blocks = append(blocks, mkv.Block{
			TrackNumber: 1, Timecode: int64(i) * 50, Keyframe: i%20 == 0, Data: []byte{0xAA},
		})
	}

	var seg bytes.Buffer
	mustNil(t, writer.WriteSegmentInfo(&seg, &info, 3000))
	mustNil(t, writer.WriteTracks(&seg, tracks))
	mustNil(t, writer.WriteCluster(&seg, 0, info.TimecodeScale, blocks))
	var buf bytes.Buffer
	mustNil(t, writer.WriteEBMLHeader(&buf))
	mustNil(t, writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()))
	mustNil(t, os.WriteFile(src, buf.Bytes(), 0o644))

	mustNil(t, RemuxToWebM(context.Background(), src, dst))

	// The seekable reader (which cannot read unknown-size clusters - the old
	// streaming output) must parse the file and find a Cues index.
	got, err := reader.Open(context.Background(), dst)
	mustNil(t, err)
	if len(got.Cues) == 0 {
		t.Errorf("remuxed WebM has no Cues index (not seekable)")
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Codec != "vp9" {
		t.Errorf("tracks = %+v, want 1 vp9 track", got.Tracks)
	}
	raw, err := os.ReadFile(dst)
	mustNil(t, err)
	if !bytes.Contains(raw[:64], []byte("webm")) {
		t.Errorf("output does not declare the webm DocType")
	}
	counts := countBlocksFromFile(t, dst, 1_000_000)
	if counts[1] != 60 {
		t.Errorf("blocks = %d, want 60 (verbatim copy)", counts[1])
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// BenchmarkRemuxToWebM remuxes a ~5 MB VP9 source (5000 blocks of 1 KiB,
// keyframe every 50) so the cost is dominated by block copy, not setup.
func BenchmarkRemuxToWebM(b *testing.B) {
	dir := b.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "out.webm")

	w, h := uint32(640), uint32(360)
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h}}
	payload := make([]byte, 1024)
	const n = 5000

	var clusters bytes.Buffer
	for i := 0; i < n; i += 50 {
		var blocks []mkv.Block
		for j := i; j < i+50 && j < n; j++ {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(j) * 40, Keyframe: j == i, Data: payload})
		}
		if err := writer.WriteCluster(&clusters, int64(i)*40, info.TimecodeScale, blocks); err != nil {
			b.Fatal(err)
		}
	}
	var seg bytes.Buffer
	mustNilB(b, writer.WriteSegmentInfo(&seg, &info, 0))
	mustNilB(b, writer.WriteTracks(&seg, tracks))
	seg.Write(clusters.Bytes())
	var buf bytes.Buffer
	mustNilB(b, writer.WriteEBMLHeader(&buf))
	mustNilB(b, writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()))
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(buf.Len()))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := RemuxToWebM(context.Background(), src, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func mustNilB(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}
