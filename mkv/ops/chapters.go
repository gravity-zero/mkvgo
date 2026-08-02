package ops

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// concatChapters lays every source's chapters onto the joined timeline: the
// chapters of source i shifted by offsets[i], the offset at which that source's
// blocks were actually written. Chapters are timeline data, so they must ride
// on the SAME offsets as the blocks - the ones measured at the seams, not the
// boundaries a split was asked for. A part cut on a keyframe holds slightly
// more than its nominal range, and by the twelfth part that difference has
// grown to tens of seconds.
//
// A nil offsets (or one shorter than the sources) treats the missing entries as
// 0: that is how the caller sizes the element before the offsets are known.
//
// Deliberate limits, all of them narrow:
//
//   - Editions. The reader flattens every EditionEntry of a file into one list
//     and the writer emits exactly one, so mkvgo carries a single edition end to
//     end. A multi-edition source arrives here already flattened; picking the
//     default edition would require the reader to keep them apart. Out of scope.
//   - Linked chapters. An atom with a ChapterSegmentUID points at ANOTHER
//     segment, and the join has just made that reference meaningless. They are
//     dropped rather than carried over as dangling links. Rare, and out of scope.
//   - The seam. Two consecutive sources can each carry a chapter around the cut,
//     because the GOP straddling it belongs to both. BOTH are kept: their
//     timestamps differ (the second starts where the first part really ended),
//     and dropping either would be an editorial decision this has no business
//     making. A rejoined split therefore gives back exactly the chapters it was
//     split on, one per part.
//   - ChapterUID. Unique within a file; two sources cut from the same original
//     carry the same UIDs. The first occurrence keeps its UID, any repeat gets
//     the lowest free one - deterministic, so the same join always numbers them
//     the same way.
func concatChapters(sources []*mkv.Container, offsets []int64) []mkv.Chapter {
	var out []mkv.Chapter
	uids := &chapterUIDs{seen: map[uint64]bool{}, next: 1}
	for i, c := range sources {
		var off int64
		if i < len(offsets) {
			off = offsets[i]
		}
		out = append(out, shiftChapters(c.Chapters, off, uids)...)
	}
	return out
}

// shiftChapters moves one source's chapters onto the output timeline, dropping
// the linked ones and re-numbering UID collisions. Sub-chapters are shifted with
// their parent, at every depth.
func shiftChapters(chapters []mkv.Chapter, offsetMs int64, uids *chapterUIDs) []mkv.Chapter {
	var out []mkv.Chapter
	for _, ch := range chapters {
		if len(ch.SegmentUID) > 0 {
			continue // links out to another segment: see concatChapters
		}
		shifted := ch
		shifted.ID = uids.unique(ch.ID)
		shifted.StartMs = ch.StartMs + offsetMs
		if ch.EndMs > 0 {
			shifted.EndMs = ch.EndMs + offsetMs
		}
		shifted.SubChapters = shiftChapters(ch.SubChapters, offsetMs, uids)
		out = append(out, shifted)
	}
	return out
}

// chapterUIDs hands out ChapterUIDs that stay unique across merged sources,
// leaving the first claimant of a UID untouched.
type chapterUIDs struct {
	seen map[uint64]bool
	next uint64
}

func (u *chapterUIDs) unique(id uint64) uint64 {
	if !u.seen[id] {
		u.seen[id] = true
		return id
	}
	for u.seen[u.next] {
		u.next++
	}
	u.seen[u.next] = true
	return u.next
}

// mergeAttachments pools every source's attachments, keeping one copy of each
// distinct FILE and renumbering the IDs so the output has no collision.
//
// Identity is the content, not the name: a split leaves the same font in every
// part (one copy survives), while two parts can carry different files under one
// name - matching on name and size alone silently kept the first and threw the
// other away, which for a font means subtitles rendering wrong in half the
// joined film.
//
// The conventional cover art is the exception. Matroska defines at most one
// cover.jpg (plus one small_cover.*), so joining twelve episodes must not
// produce twelve of them: the first one wins, by name.
func mergeAttachments(sources []*mkv.Container) []mkv.Attachment {
	var out []mkv.Attachment
	seen := map[string]bool{}
	for _, c := range sources {
		for _, a := range c.Attachments {
			key := attachmentKey(a)
			if seen[key] {
				continue
			}
			seen[key] = true
			a.ID = uint64(len(out) + 1)
			out = append(out, a)
		}
	}
	return out
}

// coverNames are the attachment names Matroska reserves for cover art, of which
// a file carries at most one each.
var coverNames = map[string]bool{
	"cover.jpg": true, "cover.png": true,
	"small_cover.jpg": true, "small_cover.png": true,
}

func attachmentKey(a mkv.Attachment) string {
	name := strings.ToLower(a.Name)
	if coverNames[name] {
		return "cover:" + name
	}
	if a.Data != nil {
		return "sha:" + string(sha256Sum(a.Data))
	}
	// No payload in hand (a metadata-only read): fall back to the weaker
	// name+size identity rather than duplicating everything.
	return fmt.Sprintf("meta:%s\x00%d", a.Name, a.Size)
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
