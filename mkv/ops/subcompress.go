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
	"github.com/gravity-zero/mkvgo/mkv"
)

// decompressSubtitleBlock returns the block's payload as content: inflated when
// the track says its payloads are compressed, untouched otherwise.
//
// Header stripping is deliberately NOT applied here. It removes bytes that the
// consumer of a given codec knows how to put back (the MP4 path does exactly
// that), and no subtitle codec mkvgo extracts uses it - so silently prepending
// nothing would be a lie either way.
// maxDecompressedBlock is the inflated-block bound the model enforces.
const maxDecompressedBlock = mkv.MaxDecodedPayload

func decompressSubtitleBlock(t *mkv.Track, data []byte) ([]byte, error) {
	if t == nil || t.Compression == mkv.CompressionHeaderStrip {
		return data, nil
	}
	return t.DecodePayload(data)
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
