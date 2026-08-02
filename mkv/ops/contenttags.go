package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strings"

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

// withStatistics turns the statistics family on whether or not the source
// carried it. The counters come free with a walk the op is doing anyway - bytes
// and frames it already touches - so there is no reason for a part or a joined
// file to go without the bitrate and frame count that describe it. Metadata-only
// readers depend on them: matroska.WithBitrate has no other source of a track's
// bitrate on a Matroska file.
//
// The content hash is deliberately NOT treated this way: it costs a SHA-256 over
// every payload byte, which an op that was never asked to certify anything
// should not spend.
func (p contentTagPlan) withStatistics() contentTagPlan {
	p.wantStats = true
	return p
}

// planContentTags sorts a source's tags. A tag left with no simple tags after
// the content-derived ones are taken out is dropped entirely.
func planContentTags(tags []mkv.Tag) contentTagPlan {
	var plan contentTagPlan
	for _, tag := range tags {
		var kept []mkv.SimpleTag
		for _, st := range tag.SimpleTags {
			// Matched case-insensitively because the consumer is:
			// promoteTrackBitrate compares with EqualFold and takes the first
			// hit, so a source spelling it "Bps" kept its whole-film value AND
			// gained a recomputed "BPS" - and the stale one won.
			name := strings.ToUpper(st.Name)
			switch {
			case name == ContentHashTag:
				plan.wantHashes = true
			case contentDerivedTags[name]:
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
				// A declared track that received no block still belongs to the
				// certified set: WriteContentHashes stamps it with the digest of
				// the empty stream, and skipping it here made the track drop out
				// of verification silently instead of being checked.
				h = sha256.New()
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

// upperBoundTags is the shape tagsForOutput will produce, with every measured
// value at its widest, so MKVWriter.ReserveTags can book a slot no real write
// can overflow. The hash is a fixed 64 hex characters; the statistics are
// decimal integers that cannot exceed an int64's 19 digits, and the duration
// is a fixed-width HH:MM:SS.nnnnnnnnn (hours widened for good measure).
func (p contentTagPlan) upperBoundTags(tracks []mkv.Track) []mkv.Tag {
	out := append([]mkv.Tag{}, p.kept...)
	const digits = "9999999999999999999"
	for i := range tracks {
		uid := trackUID(&tracks[i])
		if p.wantHashes {
			out = append(out, mkv.Tag{TargetID: uid, SimpleTags: []mkv.SimpleTag{
				{Name: ContentHashTag, Value: strings.Repeat("0", 64)},
			}})
		}
		if p.wantStats {
			out = append(out, mkv.Tag{TargetID: uid, SimpleTags: []mkv.SimpleTag{
				{Name: "BPS", Value: digits},
				{Name: "DURATION", Value: "99999999:99:99.999999999"},
				{Name: "NUMBER_OF_FRAMES", Value: digits},
				{Name: "NUMBER_OF_BYTES", Value: digits},
				{Name: "_STATISTICS_TAGS", Value: "BPS DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES"},
				{Name: "_STATISTICS_WRITING_APP", Value: "mkvgo"},
			}})
		}
	}
	return out
}
