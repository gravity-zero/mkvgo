package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// uintElem encodes a uint element of exactly valLen payload bytes.
func uintElem(t *testing.T, id uint32, v uint64, valLen int) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := ebml.WriteElementHeader(&b, id, int64(valLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := ebml.WriteUint(&b, v, valLen); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// graftClusterHints rebuilds every cluster of data with a CRC-32 first child, a
// Position and a PrevSize - all three carrying values that will be WRONG once
// the cluster is copied to another offset. That is the shape a muxer writing
// the optional position hints leaves behind.
func graftClusterHints(t *testing.T, data []byte, bogusPos, bogusPrev uint64) []byte {
	t.Helper()
	out := append([]byte(nil), data[:0]...)
	rest := data
	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	for {
		i := bytes.Index(rest, magic)
		if i < 0 {
			return append(out, rest...)
		}
		h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(rest[i:]))
		if err != nil || h.ID != mkv.IDCluster || h.Size < 0 {
			t.Fatalf("cluster at %d does not parse", i)
		}
		body := rest[i+hdrLen : i+hdrLen+int(h.Size)]

		hints := append(uintElem(t, idClusterPosition, bogusPos, 3), uintElem(t, idClusterPrevSize, bogusPrev, 3)...)
		newBody := make([]byte, 0, 6+len(hints)+len(body))
		newBody = append(newBody, 0xBF, 0x84, 0, 0, 0, 0) // CRC-32 placeholder
		newBody = append(newBody, hints...)
		newBody = append(newBody, body...)
		binary.LittleEndian.PutUint32(newBody[2:6], crc32.ChecksumIEEE(newBody[6:]))

		var hdr bytes.Buffer
		if _, err := ebml.WriteElementHeader(&hdr, mkv.IDCluster, int64(len(newBody))); err != nil {
			t.Fatal(err)
		}
		out = append(out, rest[:i]...)
		out = append(out, hdr.Bytes()...)
		out = append(out, newBody...)
		rest = rest[i+hdrLen+int(h.Size):]
	}
}

// clusterHint returns the value of a cluster body's uint hint, whether the
// element was retired to a Void, and whether it is there at all.
func clusterHint(t *testing.T, body []byte, id uint32) (v uint64, voided, present bool) {
	t.Helper()
	br := bytes.NewReader(body)
	total := int64(len(body))
	sawVoid := false
	for br.Len() > 0 {
		start := total - int64(br.Len())
		h, n, err := ebml.ReadElementHeader(br)
		if err != nil || h.Size < 0 {
			return 0, false, false
		}
		if h.ID == id {
			raw := body[start+int64(n) : start+int64(n)+h.Size]
			for _, b := range raw {
				v = v<<8 | uint64(b)
			}
			return v, false, true
		}
		if h.ID == mkv.IDVoid {
			sawVoid = true
		}
		if _, err := br.Seek(h.Size, io.SeekCurrent); err != nil {
			return 0, false, false
		}
	}
	return 0, sawVoid, false
}

// TestReindex_RetargetsClusterPositionHints: a cluster states WHERE it sits
// (Position) and what preceded it (PrevSize). A reindex rebuilds the head, so
// every cluster lands at a new Segment position - and those two hints, copied
// verbatim, then point somewhere else entirely while Reindex reports success.
// A reader that trusts them seeks into the middle of another cluster.
func TestReindex_RetargetsClusterPositionHints(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := delayedAudioFixture(t, dir, "hints.mkv", 3, 100)

	grafted := filepath.Join(dir, "hints.grafted.mkv")
	writeAll(t, grafted, graftClusterHints(t, readAll(t, src), 0x424242, 0x171717))
	resealSegmentSize(t, grafted)

	out := filepath.Join(dir, "reindexed.mkv")
	if err := Reindex(ctx, grafted, out); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	data := readAll(t, out)
	h1, n1, err := ebml.ReadElementHeader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	segIDOff := int64(n1) + h1.Size
	h2, n2, err := ebml.ReadElementHeader(bytes.NewReader(data[segIDOff:]))
	if err != nil || h2.ID != mkv.IDSegment {
		t.Fatal("no Segment")
	}
	segDataStart := segIDOff + int64(n2)

	clusters := 0
	prevSize := int64(-1)
	for off := segDataStart; off < int64(len(data)); {
		h, n, err := ebml.ReadElementHeader(bytes.NewReader(data[off:]))
		if err != nil || h.Size < 0 {
			t.Fatalf("top-level element at %d does not parse: %v", off, err)
		}
		size := int64(n) + h.Size
		if h.ID == mkv.IDCluster {
			body := data[off+int64(n) : off+size]
			relPos := off - segDataStart

			// Position must state where the cluster REALLY is now.
			if got, _, present := clusterHint(t, body, idClusterPosition); !present {
				t.Errorf("cluster %d: Position hint disappeared", clusters)
			} else if int64(got) != relPos {
				t.Errorf("cluster %d: Position = %d, want %d (its actual Segment position)",
					clusters, got, relPos)
			}

			// PrevSize must state the previous cluster's size - and the FIRST
			// cluster, which no longer has a predecessor, must not claim one.
			got, voided, present := clusterHint(t, body, idClusterPrevSize)
			switch {
			case clusters == 0:
				if present {
					t.Errorf("first cluster still claims a PrevSize of %d", got)
				}
				if !voided {
					t.Error("first cluster: the stale PrevSize was neither restated nor voided")
				}
			case !present:
				t.Errorf("cluster %d: PrevSize hint disappeared", clusters)
			case int64(got) != prevSize:
				t.Errorf("cluster %d: PrevSize = %d, want %d (the previous cluster's size)",
					clusters, got, prevSize)
			}

			// And the CRC-32 must cover what is actually written.
			if !bytes.Equal(body[0:2], []byte{0xBF, 0x84}) {
				t.Fatalf("cluster %d: the CRC-32 child vanished", clusters)
			}
			if got, want := binary.LittleEndian.Uint32(body[2:6]), crc32.ChecksumIEEE(body[6:]); got != want {
				t.Errorf("cluster %d: CRC-32 = %#08x, want %#08x: it was not resealed over the retargeted body",
					clusters, got, want)
			}

			prevSize = size
			clusters++
		}
		off += size
	}
	if clusters != 3 {
		t.Fatalf("found %d clusters in the output, want 3", clusters)
	}
}

// TestReindexInPlace_RefusesSecondSeekHead: only the head SeekHead is rewritten
// in place, so a chained second one would keep pointing at the Cues element the
// same run has just overwritten with a Void. The full reindex drops every
// SeekHead and writes a fresh one, so the file is handed to it.
func TestReindexInPlace_RefusesSecondSeekHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := delayedAudioFixture(t, dir, "twosh.mkv", 2, 100)

	// Append a second SeekHead at the tail of the Segment, pointing at the Cues
	// (the chained-SeekHead layout).
	var sh bytes.Buffer
	if err := writer.WriteSeekHead(&sh, []writer.SeekEntry{{ID: mkv.IDCues, Pos: 4096}}); err != nil {
		t.Fatal(err)
	}
	writeAll(t, path, append(readAll(t, path), sh.Bytes()...))
	resealSegmentSize(t, path)

	err := ReindexInPlace(ctx, path)
	if err == nil {
		t.Fatal("a second SeekHead must be refused, not silently left pointing at a voided index")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("second SeekHead")) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReindexInPlace_RefusesUnknownSizeSegment: the crash-safety window rests on
// the Segment declaring where it ends (the appended Cues and the journal live
// past that end, invisible until the size is extended last). An unknown-size
// Segment has no end at all.
func TestReindexInPlace_RefusesUnknownSizeSegment(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := delayedAudioFixture(t, dir, "unk.mkv", 2, 100)

	data := readAll(t, path)
	h1, n1, err := ebml.ReadElementHeader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	segIDOff := int(int64(n1) + h1.Size)
	// The writer seals the Segment size in a fixed 8-byte VINT: overwrite it
	// with the unknown-size marker.
	copy(data[segIDOff+4:segIDOff+12], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	writeAll(t, path, data)

	err = ReindexInPlace(ctx, path)
	if err == nil {
		t.Fatal("an unknown-size Segment must be refused: the journal would live inside the Segment")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("unknown-size Segment")) {
		t.Errorf("unexpected error: %v", err)
	}
}
