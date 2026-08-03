package ops

import (
	"crypto/sha256"

	"github.com/gravity-zero/mkvgo/mkv"
)

// derivedSegmentUID gives an op's output its own segment identity, derived from
// the source's and the op's name.
//
// An op whose output holds DIFFERENT content than its source - a track removed
// or added, a container converted - must not copy the source's SegmentUID over:
// two different files would then claim to be one segment, which is the same
// defect Split had when every part wore the source's identity. Derived rather
// than random for the same reason as splitSegmentUIDs: running the op twice
// writes the same bytes, so byte-for-byte comparisons keep comparing.
//
// PrevUID/NextUID are the caller's to keep: adding or removing a track does not
// move the timeline, so a hard link into it stays true - and it is the EDITED
// file's PrevUID, still naming its unchanged predecessor, that linkedTo reads
// at a join. (The predecessor's NextUID now names the identity the file had
// before the edit; that direction is not consulted.)
//
// Ops that repair or restate a file without touching its content - Reindex,
// Salvage, RetimeTracks, EditMetadata - keep the identity untouched: same
// content, same segment.
func derivedSegmentUID(info *mkv.SegmentInfo, srcPath, op string) []byte {
	seed := info.SegmentUID
	if len(seed) == 0 {
		seed = []byte(srcPath)
	}
	h := sha256.New()
	h.Write([]byte("mkvgo.derived.segment-uid.v1"))
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(seed)
	return h.Sum(nil)[:16] // a SegmentUID is 128 bits
}
