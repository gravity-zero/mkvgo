package ops

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ContentHashTag is the per-track tag WriteContentHashes stores: the SHA-256
// (hex) over the track's block payloads in stream order. VerifyContentHashes
// recomputes it, so a file carries its own integrity check - bit-rot or
// transfer corruption is detectable with no external checksum file.
const ContentHashTag = "CONTENT_SHA256"

// WriteContentHashes computes each track's content digest and stores it as a
// CONTENT_SHA256 tag keyed by track UID. With dstPath == "" the tags are
// written in place (instant on mkvgo-written files thanks to the metadata
// reserve); otherwise a new file is written. Existing CONTENT_SHA256 tags are
// replaced, so the operation is idempotent.
func WriteContentHashes(ctx context.Context, srcPath, dstPath string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, digests, err := digestTracks(ctx, srcPath, fs, mkv.ProgressFrom(opts))
	if err != nil {
		return err
	}
	tags := hashTags(c.Tracks, digests)
	apply := func(c *mkv.Container) {
		c.Tags = append(dropContentHashTags(c.Tags), tags...)
	}
	if dstPath == "" {
		if err := EditInPlace(ctx, srcPath, apply, opts...); err != nil {
			return fmt.Errorf("%w (in-place; write to a new file instead to force a full rewrite)", err)
		}
		return nil
	}
	return EditMetadata(ctx, srcPath, dstPath, apply, opts...)
}

// HashMismatch describes one track whose recomputed content digest does not
// match its stored CONTENT_SHA256 tag.
type HashMismatch struct {
	TrackID uint64
	Stored  string
	Actual  string
}

func (m HashMismatch) String() string {
	return fmt.Sprintf("track %d: content hash mismatch (stored %.12s…, actual %.12s…)", m.TrackID, m.Stored, m.Actual)
}

// VerifyContentHashes recomputes every track's content digest and compares it
// with the stored CONTENT_SHA256 tags. It returns one entry per mismatching
// track (nil = all hashed tracks intact) and errors when the file carries no
// content hashes at all (run WriteContentHashes first).
func VerifyContentHashes(ctx context.Context, path string, opts ...mkv.Options) ([]HashMismatch, error) {
	fs := mkv.FSFrom(opts)
	c, digests, err := digestTracks(ctx, path, fs, mkv.ProgressFrom(opts))
	if err != nil {
		return nil, err
	}

	stored := map[uint64]string{} // track UID → stored hex digest
	for _, tag := range c.Tags {
		if tag.TargetID == 0 {
			continue
		}
		for _, st := range tag.SimpleTags {
			if st.Name == ContentHashTag && st.Value != "" {
				stored[tag.TargetID] = st.Value
			}
		}
	}
	if len(stored) == 0 {
		return nil, fmt.Errorf("no %s tags in %s - hash it first", ContentHashTag, path)
	}

	var mismatches []HashMismatch
	for i, t := range c.Tracks {
		want, ok := stored[trackUID(&t)]
		if !ok {
			continue
		}
		got := hex.EncodeToString(digests[i].hash[:])
		if got != want {
			mismatches = append(mismatches, HashMismatch{TrackID: t.ID, Stored: want, Actual: got})
		}
	}
	return mismatches, nil
}

// hashTags builds one CONTENT_SHA256 tag per track, keyed by track UID.
func hashTags(tracks []mkv.Track, digests []trackDigest) []mkv.Tag {
	tags := make([]mkv.Tag, 0, len(tracks))
	for i := range tracks {
		tags = append(tags, mkv.Tag{
			TargetID: trackUID(&tracks[i]),
			SimpleTags: []mkv.SimpleTag{{
				Name:  ContentHashTag,
				Value: hex.EncodeToString(digests[i].hash[:]),
			}},
		})
	}
	return tags
}

// dropContentHashTags removes previously-stored content hash tags (and the
// tags left empty by that removal), so re-hashing replaces instead of stacking.
func dropContentHashTags(tags []mkv.Tag) []mkv.Tag {
	out := tags[:0]
	for _, tag := range tags {
		kept := tag.SimpleTags[:0]
		for _, st := range tag.SimpleTags {
			if st.Name == ContentHashTag {
				continue
			}
			kept = append(kept, st)
		}
		tag.SimpleTags = kept
		if len(tag.SimpleTags) > 0 {
			out = append(out, tag)
		}
	}
	return out
}

// trackUID returns the UID a tag targeting this track uses (the writer
// defaults a zero UID to the track ID).
func trackUID(t *mkv.Track) uint64 {
	if t.UID != 0 {
		return t.UID
	}
	return t.ID
}
