package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// graftCRC32 rebuilds the element at off with a CRC-32 first child covering its
// current body, and returns the rebuilt file. That is the shape a muxer that
// guards its level-1 elements with a checksum produces.
func graftCRC32(t *testing.T, data []byte, off int64, id uint32) []byte {
	t.Helper()
	h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(data[off:]))
	if err != nil || h.ID != id || h.Size < 0 {
		t.Fatalf("element 0x%X at %d does not parse: %v", id, off, err)
	}
	body := data[off+int64(hdrLen) : off+int64(hdrLen)+h.Size]

	crcVal := make([]byte, 4)
	binary.LittleEndian.PutUint32(crcVal, crc32.ChecksumIEEE(body))
	newBody := append([]byte{0xBF, 0x84}, crcVal...)
	newBody = append(newBody, body...)

	var hdr bytes.Buffer
	if _, err := ebml.WriteElementHeader(&hdr, id, int64(len(newBody))); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), data[:off]...)
	out = append(out, hdr.Bytes()...)
	out = append(out, newBody...)
	return append(out, data[off+int64(hdrLen)+h.Size:]...)
}

// TestRetime_CuesCRCRecomputed: the in-place engine patches the CueTime of every
// cue keyed on a shifted track. When the Cues element guards itself with a
// CRC-32 first child, that checksum then covers bytes that no longer exist -
// and every CRC-checking reader rejects the very index the repair just fixed.
// retimeCluster has always recomputed the cluster's CRC; the Cues went without.
func TestRetime_CuesCRCRecomputed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "cues.mkv", 3, 900)
	data := readAll(t, target)

	// The Cues ID also appears inside the SeekHead (as a SeekID payload), so the
	// element itself is the LAST occurrence - it sits past the clusters.
	offs := findAll(data, []byte{0x1C, 0x53, 0xBB, 0x6B})
	if len(offs) == 0 {
		t.Fatal("fixture has no Cues element")
	}
	cuesOff := offs[len(offs)-1]

	path := filepath.Join(dir, "cues.grafted.mkv")
	writeAll(t, path, graftCRC32(t, data, cuesOff, mkv.IDCues))
	resealSegmentSize(t, path)

	// Shift the track the cues are keyed on (the video keyframes), or no CueTime
	// moves and the checksum stays valid by accident. Force the patching engine
	// too: the auto mode may fall back to a rewrite, which regenerates the index
	// - and with it the checksum - instead of patching it.
	if err := RetimeTracksInPlace(ctx, path, map[uint64]int64{1: 200_000_000}); err != nil {
		t.Fatalf("RetimeTracksInPlace: %v", err)
	}

	got := readAll(t, path)
	gOffs := findAll(got, []byte{0x1C, 0x53, 0xBB, 0x6B})
	gCues := gOffs[len(gOffs)-1]
	h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(got[gCues:]))
	if err != nil {
		t.Fatal(err)
	}
	body := got[gCues+int64(hdrLen) : gCues+int64(hdrLen)+h.Size]
	if !bytes.Equal(body[0:2], []byte{0xBF, 0x84}) {
		t.Fatal("the Cues CRC-32 child vanished")
	}

	// Guard against a vacuous test: the CueTimes must actually have been patched
	// (otherwise the checksum would stay valid without any recompute at all).
	oldBody := data[cuesOff:]
	if bytes.Equal(body[6:], oldBody[len(oldBody)-len(body[6:]):]) {
		t.Fatal("no CueTime was patched: the test is not exercising the recompute")
	}

	if got, want := binary.LittleEndian.Uint32(body[2:6]), crc32.ChecksumIEEE(body[6:]); got != want {
		t.Errorf("Cues CRC-32 = %#08x, want %#08x: it was not recomputed over the patched CueTimes",
			got, want)
	}
}
