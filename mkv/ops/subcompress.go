package ops

// subcompress.go - block payload decompression for subtitle extraction.
//
// Matroska lets a track declare that its block payloads are compressed
// (ContentEncodings > ContentEncoding > ContentCompression). mkvmerge does this
// for subtitle tracks routinely, and writes it in its most compact form: an
// EMPTY ContentCompression element, whose ContentCompAlgo therefore takes the
// spec's default value of 0 - zlib. Nothing on the wire says "zlib" in words.
//
// The reader hands block payloads back RAW, by policy: an op that copies blocks
// into a new file also copies the track's ContentEncodings, so a reader that
// decompressed silently would produce files that declare compression and hold
// plain bytes. Decompression therefore belongs where a payload stops being
// bytes to copy and becomes content to interpret - here, at extraction.

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

// maxDecompressedBlock bounds one inflated subtitle block. A PGS display set
// runs to a few hundred KiB and a text cue to a few hundred bytes, so this is
// generous - but a compressed stream can claim any expansion ratio, and mkvgo
// has a memory budget to hold to.
const maxDecompressedBlock = 8 << 20

// decompressSubtitleBlock returns the block's payload as content: inflated when
// the track says its payloads are compressed, untouched otherwise.
//
// Header stripping is deliberately NOT applied here. It removes bytes that the
// consumer of a given codec knows how to put back (the MP4 path does exactly
// that), and no subtitle codec mkvgo extracts uses it - so silently prepending
// nothing would be a lie either way.
func decompressSubtitleBlock(t *mkv.Track, data []byte) ([]byte, error) {
	if t == nil {
		return data, nil
	}
	switch t.Compression {
	case mkv.CompressionNone, mkv.CompressionHeaderStrip:
		return data, nil
	case mkv.CompressionZlib:
		return inflateBlock(data)
	default:
		// bzlib and lzo1x have no decompressor in the standard library and
		// mkvgo takes no dependencies. Say which scheme, so the operator knows
		// this is a missing feature and not a broken file.
		return nil, fmt.Errorf("subtitle track %d is compressed with %s, which mkvgo cannot decode (only zlib is supported)",
			t.ID, t.Compression)
	}
}

func inflateBlock(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the block is declared zlib-compressed but does not start a zlib stream: %w", err)
	}
	defer zr.Close()
	// Read one byte past the ceiling: that is what distinguishes "exactly at the
	// limit" from "over it", without allocating for the overrun.
	out, err := io.ReadAll(io.LimitReader(zr, maxDecompressedBlock+1))
	if err != nil {
		return nil, fmt.Errorf("inflating the block: %w", err)
	}
	if len(out) > maxDecompressedBlock {
		return nil, fmt.Errorf("a subtitle block inflates past %d bytes, which no subtitle cue does", maxDecompressedBlock)
	}
	return out, nil
}

// subtitleTrackByID finds a track for the decompression decision. Extractors
// already validate the track; this is the lookup that gives them the record.
func subtitleTrackByID(c *mkv.Container, trackID uint64) *mkv.Track {
	for i := range c.Tracks {
		if c.Tracks[i].ID == trackID {
			return &c.Tracks[i]
		}
	}
	return nil
}
