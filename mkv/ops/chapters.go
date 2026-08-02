package ops

import (
	"crypto/sha256"
	"fmt"
	"io"
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
		var off, extent int64
		if i < len(offsets) {
			off = offsets[i]
			// How much of the timeline this source actually filled: the next
			// source starts where it stopped. A file that DECLARES more than it
			// holds (a truncated download, a verdict this project names) carries
			// chapters in that phantom tail, and shifting them without clipping
			// drops them onto the next source's frames.
			if i+1 < len(offsets) {
				extent = offsets[i+1] - off
			}
		}
		out = append(out, shiftChapters(c.Chapters, off, extent, uids)...)
	}
	return out
}

// shiftChapters moves one source's chapters onto the output timeline, dropping
// the linked ones and re-numbering UID collisions. Sub-chapters are shifted with
// their parent, at every depth.
// extentMs is how far this source's timeline reaches; 0 means "unbounded", which
// is the last source and the sizing pass.
func shiftChapters(chapters []mkv.Chapter, offsetMs, extentMs int64, uids *chapterUIDs) []mkv.Chapter {
	var out []mkv.Chapter
	for _, ch := range chapters {
		if len(ch.SegmentUID) > 0 {
			continue // links out to another segment: see concatChapters
		}
		if extentMs > 0 && ch.StartMs >= extentMs {
			continue // past what this source actually wrote
		}
		shifted := ch
		shifted.ID = uids.unique(ch.ID)
		shifted.StartMs = ch.StartMs + offsetMs
		if ch.EndMs > 0 {
			shifted.EndMs = ch.EndMs + offsetMs
		}
		shifted.SubChapters = shiftChapters(ch.SubChapters, offsetMs, extentMs, uids)
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
func mergeAttachments(sources []*mkv.Container, fs *mkv.FS) []mkv.Attachment {
	var out []mkv.Attachment
	seen := map[string]bool{}
	for _, c := range sources {
		for _, a := range c.Attachments {
			key := attachmentKey(a, fs)
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

func attachmentKey(a mkv.Attachment, fs *mkv.FS) string {
	name := strings.ToLower(a.Name)
	if coverNames[name] {
		return "cover:" + name
	}
	if a.Data != nil {
		sum := sha256.Sum256(a.Data)
		return "sha:" + string(sum[:])
	}
	// The payload was left on disk (reader.WithoutAttachmentData): hash it by
	// reading it through a fixed buffer rather than loading it, so identity
	// stays the content without the content ever becoming resident.
	if sum, err := hashAttachmentAt(a, fs); err == nil {
		return "sha:" + string(sum)
	}
	// Unreadable: fall back to the weaker name+size identity rather than
	// dropping the attachment or duplicating every one of them.
	return fmt.Sprintf("meta:%s\x00%d", a.Name, a.Size)
}

func hashAttachmentAt(a mkv.Attachment, fs *mkv.FS) ([]byte, error) {
	if a.DataPath == "" || a.Size <= 0 {
		return nil, fmt.Errorf("attachment %q has no payload location", a.Name)
	}
	f, err := fs.DoOpen(a.DataPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(a.DataOffset, io.SeekStart); err != nil {
		return nil, err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, a.Size); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
