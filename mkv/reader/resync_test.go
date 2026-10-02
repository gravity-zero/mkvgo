package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// liveVariants are the shapes of a live recording every reader must open: the
// clean one, and the same file with junk in its head, the way test4.mkv of the
// official Matroska test suite carries 134 undecodable bytes right after its
// Segment header.
var liveVariants = []struct {
	name string
	opts livefixture.Options
	tags bool
}{
	{"clean", livefixture.Options{}, false},
	{"short unknown sizes", livefixture.Options{ShortUnknown: true}, false},
	{"junk after the Segment header", livefixture.Options{JunkHead: 134, ShortUnknown: true}, false},
	{"junk after the Segment header, 8-byte sizes", livefixture.Options{JunkHead: 134}, false},
	{"junk between Info and Tracks", livefixture.Options{JunkMid: 134}, false},
	{"Tags after the last cluster", livefixture.Options{TailTags: true}, true},
	{"junk and tail Tags", livefixture.Options{JunkHead: 134, TailTags: true}, true},
	{"sized Segment", livefixture.Options{SizedSegment: true}, false},
	{"sized Segment, junk and tail Tags", livefixture.Options{SizedSegment: true, JunkHead: 134, TailTags: true}, true},
}

// drainBlocks walks br to its end and returns the block count and the first
// and last timecodes.
func drainBlocks(t *testing.T, br *BlockReader) (n int, first, last int64) {
	t.Helper()
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			return n, first, last
		}
		if err != nil {
			t.Fatalf("block %d: %v", n, err)
		}
		if n == 0 {
			first = b.Timecode
		}
		last = b.Timecode
		n++
	}
}

func checkLiveTracks(t *testing.T, c *mkv.Container) {
	t.Helper()
	if len(c.Tracks) != livefixture.Tracks {
		t.Fatalf("tracks = %d, want %d", len(c.Tracks), livefixture.Tracks)
	}
	if c.Tracks[0].Codec != "vp8" || c.Tracks[1].Type != mkv.AudioTrack {
		t.Errorf("tracks = %q (%v), %q (%v); want the VP8 video and the PCM audio",
			c.Tracks[0].Codec, c.Tracks[0].Type, c.Tracks[1].Codec, c.Tracks[1].Type)
	}
}

// TestLiveRecordingRead: the full reader opens an unknown-size Segment of
// unknown-size Clusters (it used to refuse the first Cluster it had to skip),
// finds each Cluster's end by its children - so an element FOLLOWING the last
// Cluster is still parsed - and resyncs past junk ahead of the metadata onto
// Info and Tracks instead of onto the first Cluster.
func TestLiveRecordingRead(t *testing.T) {
	for _, v := range liveVariants {
		t.Run(v.name, func(t *testing.T) {
			c, err := Read(context.Background(), bytes.NewReader(livefixture.Build(v.opts)), "live.mkv")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			checkLiveTracks(t, c)
			if got := c.Tags != nil; got != v.tags {
				t.Errorf("tail Tags parsed = %v, want %v", got, v.tags)
			}
		})
	}
}

// TestLiveRecordingReadMeta: the head-only reader used to stop silently on the
// junk and return a Container with no track and no error.
func TestLiveRecordingReadMeta(t *testing.T) {
	for _, v := range liveVariants {
		t.Run(v.name, func(t *testing.T) {
			c, err := ReadMeta(context.Background(), bytes.NewReader(livefixture.Build(v.opts)), "live.mkv")
			if err != nil {
				t.Fatalf("ReadMeta: %v", err)
			}
			checkLiveTracks(t, c)
		})
	}
}

// TestLiveRecordingReadStream: the forward-only reader resyncs the same way,
// without a Seek, and hands back a block reader that yields every block.
func TestLiveRecordingReadStream(t *testing.T) {
	for _, v := range liveVariants {
		t.Run(v.name, func(t *testing.T) {
			// io.MultiReader hides the Seek method: a true forward-only source.
			c, br, err := ReadStream(context.Background(), io.MultiReader(bytes.NewReader(livefixture.Build(v.opts))))
			if err != nil {
				t.Fatalf("ReadStream: %v", err)
			}
			checkLiveTracks(t, c)
			if n, first, last := drainBlocks(t, br); n != livefixture.Blocks || first != livefixture.FirstTimestampMs || last != livefixture.LastBlockMs {
				t.Errorf("blocks = %d [%d..%d], want %d [%d..%d]", n, first, last,
					livefixture.Blocks, livefixture.FirstTimestampMs, livefixture.LastBlockMs)
			}
		})
	}
}

// TestLiveRecordingBlockReader: the block walk from the start of the file
// passes the head junk too; without that every block of such a file is
// unreachable even once its metadata reads.
func TestLiveRecordingBlockReader(t *testing.T) {
	for _, v := range liveVariants {
		t.Run(v.name, func(t *testing.T) {
			br, err := NewBlockReader(bytes.NewReader(livefixture.Build(v.opts)), 1_000_000)
			if err != nil {
				t.Fatalf("NewBlockReader: %v", err)
			}
			if n, first, last := drainBlocks(t, br); n != livefixture.Blocks || first != livefixture.FirstTimestampMs || last != livefixture.LastBlockMs {
				t.Errorf("blocks = %d [%d..%d], want %d [%d..%d]", n, first, last,
					livefixture.Blocks, livefixture.FirstTimestampMs, livefixture.LastBlockMs)
			}
		})
	}
}

// TestResyncedBytes: a read that had to resync says so, with the exact number
// of bytes it skipped - what lets a caller tell a sound file from one a strict
// rewrite will refuse. Every reader counts the same.
func TestResyncedBytes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		opts livefixture.Options
		want int64
	}{
		{"clean", livefixture.Options{}, 0},
		{"junk after the Segment header", livefixture.Options{JunkHead: 134, ShortUnknown: true}, 134},
		{"junk between Info and Tracks", livefixture.Options{JunkMid: 61}, 61},
		{"both", livefixture.Options{JunkHead: 134, JunkMid: 61}, 195},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := livefixture.Build(tc.opts)
			full, err := Read(ctx, bytes.NewReader(data), "live.mkv")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			meta, err := ReadMeta(ctx, bytes.NewReader(data), "live.mkv")
			if err != nil {
				t.Fatalf("ReadMeta: %v", err)
			}
			stream, _, err := ReadStream(ctx, io.MultiReader(bytes.NewReader(data)))
			if err != nil {
				t.Fatalf("ReadStream: %v", err)
			}
			if full.ResyncedBytes != tc.want || meta.ResyncedBytes != tc.want || stream.ResyncedBytes != tc.want {
				t.Errorf("ResyncedBytes: Read %d, ReadMeta %d, ReadStream %d; want %d",
					full.ResyncedBytes, meta.ResyncedBytes, stream.ResyncedBytes, tc.want)
			}
		})
	}
}

// TestDamageInsideTheFileIsNotATruncatedTail: an element declaring more bytes
// than the file holds surfaces as io.ErrUnexpectedEOF - the signature of a
// truncated tail, which the packaging paths tolerate by stopping there. When a
// valid Cluster follows it is not the tail at all: taken for one, a remux
// delivered the first second of a 37 s file and reported success. The walk
// now says which it is.
func TestDamageInsideTheFileIsNotATruncatedTail(t *testing.T) {
	data := livefixture.Build(livefixture.Options{Overrun: true})
	overrunAt := int64(bytes.Index(data, []byte{0xEC, 0x01, 0x00, 0x00, 0x00, 0x7F}))
	thirdCluster := int64(bytes.LastIndex(data, []byte{0x1F, 0x43, 0xB6, 0x75}))
	readable := livefixture.Blocks - livefixture.OverrunLostBlocks

	t.Run("refused by default, and not as an EOF", func(t *testing.T) {
		br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for {
			if _, err = br.Next(); err != nil {
				break
			}
			n++
		}
		var de *DamageError
		if !errors.As(err, &de) || !errors.Is(err, ErrDamagedRegion) {
			t.Fatalf("after %d blocks: %v, want a DamageError", n, err)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			t.Error("the damage still reads as an end of file")
		}
		if de.Offset != overrunAt || de.Resume != thirdCluster {
			t.Errorf("damage at %d, media resuming at %d; want %d and %d", de.Offset, de.Resume, overrunAt, thirdCluster)
		}
		if !strings.Contains(err.Error(), "mkvgo reindex --resync") {
			t.Errorf("the refusal does not name the repair: %v", err)
		}
	})

	t.Run("walked past on request", func(t *testing.T) {
		br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		br.SetSkipDamage(true)
		n, _, last := drainBlocks(t, br)
		if n != readable || last != livefixture.LastBlockMs {
			t.Errorf("%d blocks up to %d ms, want %d up to %d", n, last, readable, livefixture.LastBlockMs)
		}
		if got, want := br.SkippedBytes(), thirdCluster-overrunAt; got != want {
			t.Errorf("skipped %d bytes, want %d (from the element to the next cluster)", got, want)
		}
	})

	t.Run("a real truncated tail stays one", func(t *testing.T) {
		whole := livefixture.Build(livefixture.Options{})
		br, err := NewBlockReader(bytes.NewReader(whole[:len(whole)-2]), 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		for {
			if _, err = br.Next(); err != nil {
				break
			}
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrDamagedRegion) {
			t.Fatalf("a file cut inside its last block: %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("a forward-only source keeps its error", func(t *testing.T) {
		_, br, err := ReadStream(context.Background(), io.MultiReader(bytes.NewReader(data)))
		if err != nil {
			t.Fatal(err)
		}
		for {
			if _, err = br.Next(); err != nil {
				break
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, ErrDamagedRegion) {
			t.Fatalf("a stream cannot look ahead: %v, want the read error as it is", err)
		}
	})
}

// TestBlockGroupKeyframe: a Block carries no keyframe flag - in a BlockGroup the
// frame is a keyframe exactly when the group has no ReferenceBlock. The block
// walk used to report every such frame as a non-keyframe, so a file storing
// its video in BlockGroups had no keyframe at all: none counted, and no point
// to cut or seek at.
func TestBlockGroupKeyframe(t *testing.T) {
	br, err := NewBlockReader(bytes.NewReader(livefixture.Build(livefixture.Options{BlockGroups: true})), 1_000_000)
	if err != nil {
		t.Fatalf("NewBlockReader: %v", err)
	}
	var videoKeys, videoOthers, audioKeys, audioOthers int
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch {
		case b.TrackNumber == livefixture.VideoTrack && b.Keyframe:
			videoKeys++
		case b.TrackNumber == livefixture.VideoTrack:
			videoOthers++
		case b.Keyframe:
			audioKeys++
		default:
			audioOthers++
		}
	}
	perTrack := livefixture.Clusters * livefixture.BlocksPerTrack
	if videoKeys != livefixture.Clusters || videoOthers != perTrack-livefixture.Clusters {
		t.Errorf("video: %d keyframes, %d others; want %d (one per cluster, no ReferenceBlock) and %d",
			videoKeys, videoOthers, livefixture.Clusters, perTrack-livefixture.Clusters)
	}
	if audioKeys != perTrack || audioOthers != 0 {
		t.Errorf("audio: %d keyframes, %d others; want %d and 0 (no group references another frame)", audioKeys, audioOthers, perTrack)
	}
}

// TestBlockReaderAtStalePositionDoesNotResync: the head resync belongs to a
// walk from the start of the file. A reader seated at a recorded offset that
// turns out to hold garbage must keep failing - resuming on whatever element
// comes next would turn a stale position into a plausible wrong answer.
func TestBlockReaderAtStalePositionDoesNotResync(t *testing.T) {
	opts := livefixture.Options{JunkHead: 134}
	data := livefixture.Build(opts)
	br, err := NewBlockReaderAt(bytes.NewReader(data), 1_000_000, livefixture.SegmentBodyOffset(opts))
	if err != nil {
		t.Fatalf("NewBlockReaderAt: %v", err)
	}
	if _, err := br.Next(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Next at a junk offset = %v, want the decode error", err)
	}
}

// TestReadMetaHeadResyncIsBounded: the head scan looks headResyncCap bytes past
// the junk and no further - beyond that it returns what it has, as documented,
// while the full reader still finds the metadata.
func TestReadMetaHeadResyncIsBounded(t *testing.T) {
	data := livefixture.Build(livefixture.Options{JunkHead: headResyncCap + 4096})
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "live.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Tracks) != 0 {
		t.Errorf("ReadMeta found %d tracks past a junk run longer than the cap, want 0", len(c.Tracks))
	}
	full, err := Read(context.Background(), bytes.NewReader(data), "live.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	checkLiveTracks(t, full)
}

// TestReadStreamJunkOnlyKeepsDecodeError: a stream with nothing valid behind
// the junk still reports the decode error, not an empty Container.
func TestReadStreamJunkOnlyKeepsDecodeError(t *testing.T) {
	opts := livefixture.Options{JunkHead: 134}
	data := livefixture.Build(opts)[:livefixture.SegmentBodyOffset(opts)+60]
	_, _, err := ReadStream(context.Background(), io.MultiReader(bytes.NewReader(data)))
	if err == nil || !strings.Contains(err.Error(), "invalid element ID") {
		t.Fatalf("ReadStream on junk only = %v, want the element ID decode error", err)
	}
}

// TestResyncToSegmentElement pins the anchor validation: a segment-level ID
// occurring by chance is not trusted, a real element is, and the declared size
// must fit the limit.
func TestResyncToSegmentElement(t *testing.T) {
	info := []byte{0x15, 0x49, 0xA9, 0x66, 0x87, 0x2A, 0xD7, 0xB1, 0x83, 0x0F, 0x42, 0x40}
	junk := bytes.Repeat([]byte{livefixture.JunkByte}, 40)
	cases := []struct {
		name  string
		data  []byte
		limit int64
		want  int64
	}{
		{"real Info after junk", join(junk, info), -1, 40},
		{"nothing but junk", junk, -1, -1},
		{"ID with an unknown size", join(junk, []byte{0x15, 0x49, 0xA9, 0x66, 0xFF, 0x2A, 0xD7, 0xB1, 0x83, 0x0F, 0x42, 0x40}), -1, -1},
		{"ID whose first child is not one of its own", join(junk, []byte{0x15, 0x49, 0xA9, 0x66, 0x87, 0xAE, 0x85, 0, 0, 0, 0, 0}), -1, -1},
		{"ID whose child overruns it", join(junk, []byte{0x15, 0x49, 0xA9, 0x66, 0x83, 0x2A, 0xD7, 0xB1, 0x83, 0x0F, 0x42, 0x40}), -1, -1},
		{"chance ID, then the real element", join(junk, []byte{0x16, 0x54, 0xAE, 0x6B, 0x0A, 0x0A}, info), -1, 46},
		{"real element past the limit", join(junk, info), 45, -1},
		{"real element overrunning the limit", join(junk, info), 48, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(tc.data)
			got, err := ResyncToSegmentElement(r, tc.limit)
			if err != nil {
				t.Fatalf("ResyncToSegmentElement: %v", err)
			}
			if got != tc.want {
				t.Fatalf("offset = %d, want %d", got, tc.want)
			}
			if pos, _ := r.Seek(0, io.SeekCurrent); got >= 0 && pos != got {
				t.Errorf("reader left at %d, want it on the element at %d", pos, got)
			}
		})
	}
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
