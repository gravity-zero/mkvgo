package ops

import (
	"math"

	"github.com/gravity-zero/mkvgo/mkv"
)

// metaForNewDuration returns a copy of c whose Segment Info no longer carries
// the source's declared Duration, for an op that writes an output of a DIFFERENT
// length than its input: Join concatenates its sources, Split cuts one range out
// of one.
//
// The writer treats a non-zero Info.Duration as authoritative and writes it
// verbatim (that is what preserves it through a metadata-only rewrite, and what
// lets an EditMetadata callback change it). An op that copies the source
// container wholesale therefore inherits a declaration that no longer matches
// what it is about to write, and the duration it computed itself would be
// silently dropped as a mere fallback. Clearing the field is how the op says
// "the length I pass is the length of this file" - the same thing
// writer.NewStreamWriter does for a stream of unknown length.
//
// Only Info.Duration is dropped; the copy is otherwise the caller's own to
// adjust (chapters, tracks) without touching c.
func metaForNewDuration(c *mkv.Container) mkv.Container {
	meta := *c
	meta.Info.Duration = 0
	return meta
}

// metaForMergedSubs sizes a subtitle merge's output: the file runs as long as
// its source OR its last injected cue, whichever ends later. A cue that
// outlasts the source is injected all the same, so left as copied the source's
// authoritative Info.Duration declared a length the file plays past - AddTrack
// already handles the same situation for a longer track.
func metaForMergedSubs(c *mkv.Container, subBlocks []mkv.Block) (mkv.Container, int64) {
	durationMs := c.DurationMs
	for _, b := range subBlocks {
		if end := b.Timecode + b.Duration; end > durationMs {
			durationMs = end
		}
	}
	if durationMs > c.DurationMs {
		return metaForNewDuration(c), durationMs
	}
	return *c, durationMs
}

// syncDurationMs re-derives c.DurationMs from c.Info.Duration exactly as the
// reader does when opening a file, and is called after an edit callback has had
// the container.
//
// Restating a file's length means setting Info.Duration, the raw stored field.
// DurationMs would otherwise keep the value read from disk, leaving one
// container answering two different lengths depending on which field is read.
// A callback that clears Info.Duration keeps DurationMs as it was, which is
// what makes the writer fall back to it.
func syncDurationMs(c *mkv.Container) {
	scale := c.Info.TimecodeScale
	if c.Info.Duration <= 0 || scale <= 0 {
		return
	}
	ms := c.Info.Duration * float64(scale) / 1e6
	if ms > float64(math.MaxInt64) {
		return
	}
	c.DurationMs = int64(ms)
}
