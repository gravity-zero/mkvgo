package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// beforeFirstCluster returns the fixture with extra spliced in ahead of its first Cluster, a sized Segment grown by
// as much.
func beforeFirstCluster(t *testing.T, o livefixture.Options, extra []byte) []byte {
	t.Helper()
	data := livefixture.Build(o)
	return spliceAt(t, data, bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75}), extra)
}

// spliceAt returns data with extra spliced in at offset at, a sized Segment grown by as much.
func spliceAt(t *testing.T, data []byte, at int, extra []byte) []byte {
	t.Helper()
	if at < 0 {
		t.Fatal("nowhere to splice in the fixture")
	}
	r := bytes.NewReader(data)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	segAt := int64(n) + h.Size
	if _, err := r.Seek(segAt, 0); err != nil {
		t.Fatal(err)
	}
	seg, segHdr, err := ebml.ReadElementHeader(r)
	if err != nil || seg.ID != mkv.IDSegment {
		t.Fatalf("Segment header: 0x%X, %v", seg.ID, err)
	}
	out := append([]byte(nil), data[:at]...)
	out = append(out, extra...)
	out = append(out, data[at:]...)
	if seg.Size >= 0 {
		var size bytes.Buffer
		if _, err := ebml.WriteDataSizeWidth(&size, seg.Size+int64(len(extra)), segHdr-4); err != nil {
			t.Fatal(err)
		}
		copy(out[segAt+4:], size.Bytes())
	}
	return out
}

// TestHeadJunkThatReadsAsAnElement: bytes left ahead of the first Cluster that happen to decode as an element header
// (the tail of an old tag string, "0000" then two null bytes: an unknown ID declaring 1 MiB) are junk to every reader,
// the block walk included, and so is a Void that declares more than it holds. It used to skip them by the size they declare: past the end of the file, reported as a
// source cut short, or into the media, losing the Clusters it landed past without a word.
func TestHeadJunkThatReadsAsAnElement(t *testing.T) {
	ctx := context.Background()
	shapes := []struct {
		name string
		opts livefixture.Options
	}{
		{"live recording", livefixture.Options{}},
		{"finished file", livefixture.Options{SizedClusters: true, PayloadBytes: 64}},
	}
	junks := []struct {
		name string
		junk []byte
	}{
		{"declares more than the file holds", []byte("0000\x00\x00")},
		{"declares a few bytes into the Cluster", []byte{'0', '0', '0', 0x88, 0x00, 0x00}},
		{"declares a size that ends inside a later Cluster", []byte{'0', '0', '0', 0x40, 0x60, 0x00}},
		{"a Void declaring 6 bytes too many", []byte{0xEC, 0x8A, 0x00, 0x00, 0x00, 0x00}},
		{"a CRC-32 declaring 6 bytes too many", []byte{0xBF, 0x8A, 0x00, 0x00, 0x00, 0x00}},
	}
	for _, s := range shapes {
		for _, j := range junks {
			t.Run(s.name+", "+j.name, func(t *testing.T) {
				data := beforeFirstCluster(t, s.opts, j.junk)
				want := int64(len(j.junk))

				br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
				if err != nil {
					t.Fatalf("NewBlockReader: %v", err)
				}
				if n, first, last := drainBlocks(t, br); n != livefixture.Blocks || first != livefixture.FirstTimestampMs || last != livefixture.LastBlockMs {
					t.Errorf("blocks = %d [%d..%d], want %d [%d..%d]", n, first, last,
						livefixture.Blocks, livefixture.FirstTimestampMs, livefixture.LastBlockMs)
				}

				tolerant, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
				if err != nil {
					t.Fatalf("NewBlockReader: %v", err)
				}
				tolerant.SetSkipDamage(true)
				if n, _, _ := drainBlocks(t, tolerant); n != livefixture.Blocks || tolerant.SkippedBytes() != want {
					t.Errorf("tolerant walk: %d blocks, %d bytes skipped; want %d blocks and the %d junk bytes",
						n, tolerant.SkippedBytes(), livefixture.Blocks, want)
				}

				sound, err := ReadMeta(ctx, bytes.NewReader(livefixture.Build(s.opts)), "sound.mkv", WithKeyframeWalk())
				if err != nil || len(sound.Keyframes) == 0 {
					t.Fatalf("keyframe walk of the sound fixture: %d keyframes, %v", len(sound.Keyframes), err)
				}
				walked, err := ReadMeta(ctx, bytes.NewReader(data), "junk.mkv", WithKeyframeWalk())
				if err != nil || !slices.Equal(walked.Keyframes, sound.Keyframes) {
					t.Errorf("keyframe walk: %v (%v), want the sound file's %v", walked.Keyframes, err, sound.Keyframes)
				}

				full, err := Read(ctx, bytes.NewReader(data), "junk.mkv")
				if err != nil {
					t.Fatalf("Read: %v", err)
				}
				if full.ResyncedBytes != want {
					t.Errorf("Read: ResyncedBytes %d, want %d", full.ResyncedBytes, want)
				}
			})
		}
	}
}

// TestHeadUnknownElementIsNotJunk: an element of an ID Matroska does not name, whole and followed by the first
// Cluster, is skipped by its size as before: nothing is reported as passed over.
func TestHeadUnknownElementIsNotJunk(t *testing.T) {
	for _, opts := range []livefixture.Options{{}, {SizedClusters: true}} {
		data := beforeFirstCluster(t, opts, []byte{'0', '0', '0', 0x84, 1, 2, 3, 4})
		br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
		if err != nil {
			t.Fatalf("NewBlockReader: %v", err)
		}
		br.SetSkipDamage(true)
		if n, _, _ := drainBlocks(t, br); n != livefixture.Blocks || br.SkippedBytes() != 0 {
			t.Errorf("sized %v: %d blocks, %d bytes skipped; want %d blocks and none skipped", opts.SizedClusters, n, br.SkippedBytes(), livefixture.Blocks)
		}
		stream, err := NewStreamBlockReader(bytes.NewReader(data), 1_000_000)
		if err != nil {
			t.Fatalf("NewStreamBlockReader: %v", err)
		}
		if n, _, _ := drainBlocks(t, stream); n != livefixture.Blocks {
			t.Errorf("sized %v: forward-only walk: %d blocks, want %d", opts.SizedClusters, n, livefixture.Blocks)
		}
		c, err := Read(context.Background(), bytes.NewReader(data), "unknown.mkv")
		if err != nil || c.ResyncedBytes != 0 {
			t.Errorf("sized %v: Read: ResyncedBytes %d, err %v; want a sound file", opts.SizedClusters, c.ResyncedBytes, err)
		}
	}
}

// TestTailJunkIsNotACut: a few bytes that are no element between the last Cluster and the Tags closing a finished
// file (what an in-place rewrite can leave) end the block walk as the end of the media they are. They used to be
// skipped by the size they happen to declare, past the end of the file: a complete file reported as cut short. A run
// long enough to hide a Cluster stays an error.
func TestTailJunkIsNotACut(t *testing.T) {
	opts := livefixture.Options{SizedClusters: true, TailTags: true}
	sound := livefixture.Build(opts)
	tags := bytes.LastIndex(sound, []byte{0x12, 0x54, 0xC3, 0x67})
	for name, junk := range map[string][]byte{
		"declares more than the file holds":  []byte("0000\x00\x00"),
		"declares a few bytes into the Tags": {'0', '0', '0', 0x88, 0x00, 0x00},
		"does not decode":                    bytes.Repeat([]byte{livefixture.JunkByte}, 6),
		"a Void declaring 6 bytes too many":  {0xEC, 0x8A, 0x00, 0x00, 0x00, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			data := spliceAt(t, sound, tags, junk)
			br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
			if err != nil {
				t.Fatalf("NewBlockReader: %v", err)
			}
			if n, _, last := drainBlocks(t, br); n != livefixture.Blocks || last != livefixture.LastBlockMs || br.StoppedShort() {
				t.Errorf("blocks = %d, last %d, stopped short %v; want %d, %d and a whole file", n, last, br.StoppedShort(), livefixture.Blocks, livefixture.LastBlockMs)
			}
			tolerant, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
			if err != nil {
				t.Fatalf("NewBlockReader: %v", err)
			}
			tolerant.SetSkipDamage(true)
			if n, _, _ := drainBlocks(t, tolerant); n != livefixture.Blocks || tolerant.SkippedBytes() != int64(len(junk)) || tolerant.StoppedShort() {
				t.Errorf("tolerant walk: %d blocks, %d bytes skipped, stopped short %v; want %d blocks, the %d junk bytes, a whole file",
					n, tolerant.SkippedBytes(), tolerant.StoppedShort(), livefixture.Blocks, len(junk))
			}
		})
	}

	long := spliceAt(t, sound, tags, bytes.Repeat([]byte{livefixture.JunkByte}, maxTailJunk+1))
	br, err := NewBlockReader(bytes.NewReader(long), 1_000_000)
	if err != nil {
		t.Fatalf("NewBlockReader: %v", err)
	}
	for err == nil {
		_, err = br.Next()
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("a run of %d bytes that are no element ended the walk as a clean end: it may hide a Cluster", maxTailJunk+1)
	}
}

// headIndexAndTags is a Cues element of one CuePoint and a Tags element of one tag: what lets a head read asked for
// both stop before the first Cluster.
var headIndexAndTags = []byte{
	0x1C, 0x53, 0xBB, 0x6B, 0x8D, 0xBB, 0x8B, 0xB3, 0x81, 0x00, 0xB7, 0x86, 0xF7, 0x81, 0x01, 0xF1, 0x81, 0x00,
	0x12, 0x54, 0xC3, 0x67, 0x8E, 0x73, 0x73, 0x8B, 0x67, 0xC8, 0x88, 0x45, 0xA3, 0x81, 0x41, 0x44, 0x87, 0x81, 0x42,
}

// TestWholeHeadCountsJunkBehindWhatWasAsked: a head read stops at the last element it was asked for, so junk
// between that element and the first Cluster can go uncounted (measured on a muxer's file whose tracks state their
// frame duration; this fixture's do not, and the plain read goes on to the first Cluster for a frame rate).
// WithWholeHead walks on to the first Cluster whatever the file, and counts it.
func TestWholeHeadCountsJunkBehindWhatWasAsked(t *testing.T) {
	ctx := context.Background()
	junk := []byte("0000\x00\x00")
	indexed := headIndexAndTags
	data := beforeFirstCluster(t, livefixture.Options{SizedClusters: true}, append(append([]byte(nil), indexed...), junk...))

	early, err := ReadMeta(ctx, bytes.NewReader(data), "junk.mkv", WithCues(), WithTags())
	if err != nil || len(early.Cues) != 1 || early.Tags == nil {
		t.Fatalf("head read: %d cues, tags %v, %v; the fixture must give it both ahead of the junk", len(early.Cues), early.Tags != nil, err)
	}
	whole, err := ReadMeta(ctx, bytes.NewReader(data), "junk.mkv", WithCues(), WithTags(), WithWholeHead())
	if err != nil || whole.ResyncedBytes != int64(len(junk)) {
		t.Errorf("WithWholeHead: ResyncedBytes %d (%v), want the %d junk bytes", whole.ResyncedBytes, err, len(junk))
	}
	sound := beforeFirstCluster(t, livefixture.Options{SizedClusters: true}, indexed)
	if c, err := ReadMeta(ctx, bytes.NewReader(sound), "sound.mkv", WithCues(), WithTags(), WithWholeHead()); err != nil || c.ResyncedBytes != 0 {
		t.Errorf("WithWholeHead on a sound head: ResyncedBytes %d, %v", c.ResyncedBytes, err)
	}
}
