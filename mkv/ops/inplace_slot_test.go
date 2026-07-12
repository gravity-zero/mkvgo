package ops

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// metaSizeWithTitle is the exact number of bytes EditInPlace serialises for a
// file whose only metadata is Info+Tracks, once the title is set to title.
func metaSizeWithTitle(t *testing.T, c *mkv.Container, title string) int64 {
	t.Helper()
	old := c.Info.Title
	c.Info.Title = title
	defer func() { c.Info.Title = old }()

	var b bytes.Buffer
	if err := writer.WriteSegmentInfo(&b, &c.Info, c.DurationMs); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTracks(&b, c.Tracks); err != nil {
		t.Fatal(err)
	}
	return int64(b.Len())
}

// assertSeekHeadIntact re-reads path and checks the head index survived the
// edit: the metadata region still opens with a SeekHead, every entry resolves
// to an element of the advertised ID (a one-byte slip in the reserved slot
// would break this), and the Cues past the clusters are still pointed at.
func assertSeekHeadIntact(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	h, _, err := ebml.ReadElementHeader(f) // EBML header
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(h.Size, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	seg, _, err := ebml.ReadElementHeader(f)
	if err != nil || seg.ID != mkv.IDSegment {
		t.Fatalf("no Segment after the EBML header: %v", err)
	}
	segDataStart, _ := f.Seek(0, io.SeekCurrent)

	sh, _, err := ebml.ReadElementHeader(f)
	if err != nil {
		t.Fatalf("read first Segment child: %v", err)
	}
	if sh.ID != mkv.IDSeekHead {
		t.Fatalf("first Segment child is 0x%X, want a SeekHead (0x%X): the edit destroyed the head index",
			sh.ID, mkv.IDSeekHead)
	}
	body := make([]byte, sh.Size)
	if _, err := io.ReadFull(f, body); err != nil {
		t.Fatal(err)
	}
	entries := parseSeekHeadBody(body)
	if len(entries) == 0 {
		t.Fatal("rebuilt SeekHead has no entries")
	}

	var sawCues bool
	for _, e := range entries {
		if e.ID == mkv.IDCues {
			sawCues = true
		}
		if _, err := f.Seek(segDataStart+e.Pos, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		eh, _, err := ebml.ReadElementHeader(f)
		if err != nil {
			t.Fatalf("SeekHead entry 0x%X points at %d, which does not parse: %v", e.ID, e.Pos, err)
		}
		if eh.ID != e.ID {
			t.Errorf("SeekHead entry 0x%X points at %d, which holds a 0x%X: the entries are off",
				e.ID, e.Pos, eh.ID)
		}
	}
	if !sawCues {
		t.Error("rebuilt SeekHead lost the Cues entry: the index is unreachable head-only")
	}
}

// TestEditInPlace_NeverDropsTheSeekHead sweeps every metadata size the region
// can host. The old code rebuilt the SeekHead only while the fixed 256-byte
// slot still fitted; past that it wrote the metadata OVER the SeekHead and
// returned nil, leaving the Cues reachable only by a full walk. The invariant
// now: EditInPlace either succeeds with an intact SeekHead, or it refuses and
// leaves the file untouched.
func TestEditInPlace_NeverDropsTheSeekHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1)}

	probe := buildMinimalMKV(t, dir, "probe.mkv", tracks, testBlocks(1), 300)
	region, err := findMetadataRegion(probe, nil)
	if err != nil {
		t.Fatal(err)
	}
	available := region.end - region.start
	c, err := reader.OpenWithFS(ctx, probe, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Titles leaving 0..280 bytes of slack after the metadata. The band below
	// SeekHeadReserve (256) is where the old code dropped the SeekHead; the
	// exact-fit and one-byte-gap boundaries live in there too.
	var edits, refusals int
	for slack := int64(0); slack <= 280; slack++ {
		titleLen := 1
		for available-metaSizeWithTitle(t, c, strings.Repeat("A", titleLen)) > slack &&
			int64(titleLen) < available {
			titleLen++
		}
		title := strings.Repeat("A", titleLen)
		if available-metaSizeWithTitle(t, c, title) != slack {
			continue // no title length lands exactly on this slack (VINT width jumps)
		}

		path := buildMinimalMKV(t, dir, "sweep.mkv", tracks, testBlocks(1), 300)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if err := EditInPlace(ctx, path, func(ct *mkv.Container) { ct.Info.Title = title }); err != nil {
			after, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("slack=%d: EditInPlace failed (%v) but wrote to the file anyway", slack, err)
			}
			if !strings.Contains(err.Error(), "exceeds available space") {
				t.Fatalf("slack=%d: unexpected error: %v", slack, err)
			}
			refusals++
			continue
		}

		edits++
		assertSeekHeadIntact(t, path)

		got, err := reader.OpenWithFS(ctx, path, nil)
		if err != nil {
			t.Fatalf("slack=%d: file unreadable after the edit: %v", slack, err)
		}
		if got.Info.Title != title {
			t.Fatalf("slack=%d: title not applied", slack)
		}
		if len(got.Tracks) != 1 {
			t.Fatalf("slack=%d: tracks lost", slack)
		}
		if len(got.Cues) == 0 {
			t.Fatalf("slack=%d: cues lost", slack)
		}
	}

	if edits == 0 || refusals == 0 {
		t.Fatalf("sweep exercised only one branch: %d edits, %d refusals", edits, refusals)
	}
	t.Logf("sweep: %d edits (SeekHead intact), %d refusals (file untouched)", edits, refusals)
}

// TestEditInPlace_ShrinksTheSeekHeadSlot pins what the fix adds: when the grown
// metadata no longer leaves room for the standard 256-byte reserve, the SeekHead
// goes into a slot sized to fit it exactly, rather than being dropped. The old
// code returned nil here, with no SeekHead left in the file.
func TestEditInPlace_ShrinksTheSeekHeadSlot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1)}

	probe := buildMinimalMKV(t, dir, "probe.mkv", tracks, testBlocks(1), 300)
	region, err := findMetadataRegion(probe, nil)
	if err != nil {
		t.Fatal(err)
	}
	available := region.end - region.start
	c, err := reader.OpenWithFS(ctx, probe, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Leave 100 bytes: too tight for the 256-byte reserve, wide enough for the
	// three-entry SeekHead (Info, Tracks, Cues).
	titleLen := 1
	for available-metaSizeWithTitle(t, c, strings.Repeat("A", titleLen)) > 100 {
		titleLen++
	}
	title := strings.Repeat("A", titleLen)
	if newSize := metaSizeWithTitle(t, c, title); newSize+writer.SeekHeadReserve <= available {
		t.Fatalf("test does not exercise the shrink path: %d + %d <= %d",
			newSize, writer.SeekHeadReserve, available)
	}

	path := buildMinimalMKV(t, dir, "shrink.mkv", tracks, testBlocks(1), 300)
	if err := EditInPlace(ctx, path, func(ct *mkv.Container) { ct.Info.Title = title }); err != nil {
		t.Fatalf("EditInPlace refused an edit that fits next to a shrunk SeekHead: %v", err)
	}
	assertSeekHeadIntact(t, path)

	got, err := reader.OpenWithFS(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.Title != title {
		t.Error("title not applied")
	}
}
