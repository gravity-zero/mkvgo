package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// buildLacedSubtitleMKV writes a minimal Matroska holding ONE subtitle track
// whose single SimpleBlock is Xiph-laced over three cues. Real muxers do not
// lace subtitles, so no fixture in the corpus exercises it - but the reader
// accepts lacing on any track, and every frame of a lace reports the SAME
// BlockPos, which is the trap the index has to survive.
func buildLacedSubtitleMKV(t *testing.T, dir string) string {
	t.Helper()

	var tracks bytes.Buffer
	var entry bytes.Buffer
	ebml.WriteElementHeader(&entry, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&entry, 1, 1)
	ebml.WriteElementHeader(&entry, mkv.IDTrackUID, 1)
	ebml.WriteUint(&entry, 1, 1)
	ebml.WriteElementHeader(&entry, mkv.IDTrackType, 1)
	ebml.WriteUint(&entry, 17, 1) // Matroska TrackType: subtitle
	ebml.WriteElementHeader(&entry, mkv.IDCodecID, int64(len("S_TEXT/UTF8")))
	entry.WriteString("S_TEXT/UTF8")
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(entry.Len()))
	tracks.Write(entry.Bytes())

	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 4)
	ebml.WriteUint(&info, 1000000, 4)

	// Xiph lacing: [track VINT][int16 rel TC][flags=0x02 → Xiph][laceCount-1][sizes...][frames]
	f0, f1, f2 := []byte("first"), []byte("second"), []byte("third")
	block := []byte{0x81, 0x00, 0x00, 0x02, 0x02, byte(len(f0)), byte(len(f1))}
	block = append(block, f0...)
	block = append(block, f1...)
	block = append(block, f2...)

	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(block)))
	cluster.Write(block)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTracks, int64(tracks.Len()))
	seg.Write(tracks.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
	seg.Write(cluster.Bytes())

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())

	path := filepath.Join(dir, "laced.mkv")
	if err := os.WriteFile(path, full.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every frame of a laced block shares its block's position, so an index that
// recorded one entry per FRAME would seek to the same place N times and replay
// frame 0 - silently, since the recorded timecode also matches. Serving from
// the index must reproduce the walking extractor byte for byte.
func TestSubtitleIndex_LacedBlockServesEveryFrame(t *testing.T) {
	dir := t.TempDir()
	path := buildLacedSubtitleMKV(t, dir)

	var walk bytes.Buffer
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &walk); err != nil {
		t.Fatalf("walking extractor: %v", err)
	}
	// Guard the fixture itself: if the reader stopped delivering three frames,
	// this test would pass vacuously.
	if n := strings.Count(walk.String(), " --> "); n != 3 {
		t.Fatalf("fixture delivered %d cues, want 3 (lacing not exercised):\n%s", n, walk.String())
	}
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(walk.String(), want) {
			t.Fatalf("walk output missing %q:\n%s", want, walk.String())
		}
	}

	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("BuildSubtitleIndex: %v", err)
	}
	// One entry for the block, not one per frame.
	if n := ix.Blocks(1); n != 1 {
		t.Errorf("index holds %d entries for a single laced block, want 1", n)
	}

	var served bytes.Buffer
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 1, ix, &served); err != nil {
		t.Fatalf("serving from the index: %v", err)
	}
	if !bytes.Equal(walk.Bytes(), served.Bytes()) {
		t.Errorf("served output differs from the walk\n--- walk ---\n%s\n--- served ---\n%s",
			walk.String(), served.String())
	}

	// And it must survive the wire format.
	blob, err := ix.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var back SubtitleIndex
	if err := back.UnmarshalBinary(blob); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	var round bytes.Buffer
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 1, &back, &round); err != nil {
		t.Fatalf("serving from the decoded index: %v", err)
	}
	if !bytes.Equal(walk.Bytes(), round.Bytes()) {
		t.Errorf("decoded index serves different bytes than the walk:\n%s", round.String())
	}
}
