package ops

import (
	"encoding/hex"
	"hash"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Tags whose value describes the media itself rather than the work: they stop
// being true the moment an op writes a different set of frames than it read.
// Copying them onto a cut or a concatenation - which is what carrying the
// source's Tags verbatim does - leaves the output certifying content it does
// not hold. VerifyContentHashes then reports mkvgo's own output as corrupt.
var contentDerivedTags = map[string]bool{
	ContentHashTag:                 true,
	"BPS":                          true,
	"DURATION":                     true,
	"NUMBER_OF_FRAMES":             true,
	"NUMBER_OF_BYTES":              true,
	"_STATISTICS_TAGS":             true,
	"_STATISTICS_WRITING_APP":      true,
	"_STATISTICS_WRITING_DATE_UTC": true,
}

// contentTagPlan is what an op has to know before streaming: which tags survive
// untouched, and which content-derived families the source carried and the
// output therefore has to state for itself.
type contentTagPlan struct {
	kept       []mkv.Tag // everything that is not derived from the media
	wantHashes bool      // the source carried CONTENT_SHA256
	wantStats  bool      // the source carried the statistics family
}

// recompute reports whether anything has to be measured while streaming.
func (p contentTagPlan) recompute() bool { return p.wantHashes || p.wantStats }

// planContentTags sorts a source's tags. A tag left with no simple tags after
// the content-derived ones are taken out is dropped entirely.
func planContentTags(tags []mkv.Tag) contentTagPlan {
	var plan contentTagPlan
	for _, tag := range tags {
		var kept []mkv.SimpleTag
		for _, st := range tag.SimpleTags {
			switch {
			case st.Name == ContentHashTag:
				plan.wantHashes = true
			case contentDerivedTags[st.Name]:
				plan.wantStats = true
			default:
				kept = append(kept, st)
			}
		}
		if len(kept) > 0 {
			tag.SimpleTags = kept
			plan.kept = append(plan.kept, tag)
		}
	}
	return plan
}

// digestsFor and statsFor allocate the accumulators the plan calls for, or nil
// when the source carried no such tags - an op that was not asked to certify
// anything should not start hashing every byte it copies.
func (p contentTagPlan) digestsFor() map[uint64]hash.Hash {
	if !p.wantHashes {
		return nil
	}
	return map[uint64]hash.Hash{}
}

func (p contentTagPlan) statsFor() map[uint64]*trackStats {
	if !p.wantStats {
		return nil
	}
	return map[uint64]*trackStats{}
}

// tagsForOutput assembles the tags to write once the media is on disk: the ones
// that survived, plus the recomputed families measured while streaming. The
// digests and stats are keyed by OUTPUT track number, the tags by track UID.
func (p contentTagPlan) tagsForOutput(tracks []mkv.Track, digests map[uint64]hash.Hash, stats map[uint64]*trackStats) []mkv.Tag {
	out := append([]mkv.Tag{}, p.kept...)
	if p.wantHashes {
		for i := range tracks {
			h := digests[tracks[i].ID]
			if h == nil {
				continue // no block of this track reached the output
			}
			out = append(out, mkv.Tag{
				TargetID:   trackUID(&tracks[i]),
				SimpleTags: []mkv.SimpleTag{{Name: ContentHashTag, Value: hex.EncodeToString(h.Sum(nil))}},
			})
		}
	}
	if p.wantStats {
		out = append(out, statsTags(tracks, stats)...)
	}
	return out
}
