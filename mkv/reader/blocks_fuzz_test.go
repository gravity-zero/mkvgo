package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// clusterWithSimpleBlock wraps a raw SimpleBlock payload (track + relTC + flags +
// lacing data) in a cluster (Timestamp + SimpleBlock) for buildBlockReaderInput.
func clusterWithSimpleBlock(blockPayload []byte) []byte {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)
	return cluster.Bytes()
}

// TestLacedBlockMalformedNoPanic covers the two lacing panics the audit found:
// a laced SimpleBlock with zero payload (raw[0] out of range) and a lacing header
// longer than the data (raw[headerBytes:] out of range). The parser must return
// an error, never panic.
func TestLacedBlockMalformedNoPanic(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		// track=1, relTC=0, lacing flag set, then ZERO lacing bytes -> dataSize==0.
		{"xiph zero payload", []byte{0x81, 0x00, 0x00, 0x02}},
		{"fixed zero payload", []byte{0x81, 0x00, 0x00, 0x04}},
		{"ebml zero payload", []byte{0x81, 0x00, 0x00, 0x06}},
		// frame-count byte present but the lacing header overruns the data.
		{"xiph header overflow", []byte{0x81, 0x00, 0x00, 0x02, 0x02, 0xFF, 0xFF}},
		{"ebml header overflow", []byte{0x81, 0x00, 0x00, 0x06, 0x05}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parseBlock panicked on %q: %v", tc.name, r)
				}
			}()
			br, err := NewBlockReader(buildBlockReaderInput(clusterWithSimpleBlock(tc.payload)), 1000000)
			if err != nil {
				return
			}
			for {
				if _, err := br.Next(); err != nil {
					return // an error is the expected, safe outcome
				}
			}
		})
	}
}

// TestLacedBlockKeyframeFlag verifies that only the FIRST frame of a laced
// keyframe block carries the keyframe flag (guards parseBlock's `keyframe && i == 0`).
func TestLacedBlockKeyframeFlag(t *testing.T) {
	// track=1, relTC=0, flags=keyframe(0x80)|fixed-lacing(0x04), frameCount byte=1
	// (=> 2 frames), then 2 bytes of data (1 byte per frame).
	payload := []byte{0x81, 0x00, 0x00, 0x84, 0x01, 0xAA, 0xBB}
	br, err := NewBlockReader(buildBlockReaderInput(clusterWithSimpleBlock(payload)), 1000000)
	if err != nil {
		t.Fatal(err)
	}
	b0, err := br.Next()
	if err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	b1, err := br.Next()
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if !b0.Keyframe {
		t.Error("frame 0 of a keyframe laced block should be a keyframe")
	}
	if b1.Keyframe {
		t.Error("frame 1 of a laced block must not be a keyframe")
	}
	if len(b0.Data) != 1 || b0.Data[0] != 0xAA || len(b1.Data) != 1 || b1.Data[0] != 0xBB {
		t.Errorf("frame data wrong: b0=%v b1=%v", b0.Data, b1.Data)
	}
}

// FuzzBlockReader drives the block/lacing parser (parseBlock/decodeLacingSizes)
// with arbitrary cluster payloads -- the path FuzzRead does not reach. Contract:
// never panic; return blocks or an error.
func FuzzBlockReader(f *testing.F) {
	f.Add(clusterWithSimpleBlock([]byte{0x81, 0x00, 0x00, 0x02}))
	f.Add(clusterWithSimpleBlock([]byte{0x81, 0x00, 0x00, 0x06, 0x05}))
	f.Add(clusterWithSimpleBlock([]byte{0x81, 0x00, 0x00, 0x00, 0xAA})) // unlaced
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, clusterPayload []byte) {
		br, err := NewBlockReader(buildBlockReaderInput(clusterPayload), 1000000)
		if err != nil {
			return
		}
		for i := 0; i < 1000; i++ { // bound iterations against a degenerate loop
			if _, err := br.Next(); err != nil {
				return
			}
		}
	})
}

// TestReaderNormalizesZeroTimecodeScale: a file declaring TimecodeScale=0 must be
// normalised to the default, otherwise downstream WriteCluster/Cues divide by zero.
func TestReaderNormalizesZeroTimecodeScale(t *testing.T) {
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 1)
	ebml.WriteUint(&info, 0, 1) // explicit TimecodeScale = 0 (malformed)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())

	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, int64(seg.Len()))
	buf.Write(seg.Bytes())

	c, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "zerots.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if c.Info.TimecodeScale != 1000000 {
		t.Errorf("TimecodeScale = %d, want normalised to 1000000", c.Info.TimecodeScale)
	}
}
