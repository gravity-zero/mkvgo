package mp4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// HashMismatch describes one MP4 track whose recomputed content digest does
// not match its stored CONTENT_SHA256_<track_ID> freeform atom.
type HashMismatch struct {
	TrackID uint32
	Stored  string
	Actual  string
}

func (m HashMismatch) String() string {
	return fmt.Sprintf("track %d: content hash mismatch (stored %.12s…, actual %.12s…)", m.TrackID, m.Stored, m.Actual)
}

// VerifyContentHashes recomputes each track's content SHA-256 from the sample
// table and compares it with the hashes RemuxToMP4 stored under
// Options.ContentHashes. It returns one entry per mismatching track (nil =
// every hashed track is byte-intact) and errors when the file carries no
// content hashes (produce a hashed MP4 with `to-mp4 --hash`).
func VerifyContentHashes(ctx context.Context, path string, opts ...Options) ([]HashMismatch, error) {
	o := optionsFrom(opts)
	src, err := o.FS.DoOpen(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	fi, err := o.FS.DoStat(path)
	if err != nil {
		return nil, err
	}

	mv, err := parseMP4(src, fi.Size(), sampleFull)
	if err != nil {
		return nil, err
	}
	if len(mv.hashes) == 0 {
		return nil, errf("no CONTENT_SHA256 atoms in %s — produce a hashed MP4 with `to-mp4 --hash`", path)
	}

	var mismatches []HashMismatch
	var processed int64
	for i := range mv.tracks {
		t := &mv.tracks[i]
		want, ok := mv.hashes[t.trackID]
		if !ok {
			continue
		}
		h := sha256.New()
		for j := range t.samples {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			s := &t.samples[j]
			data, err := readSample(src, s.offset, s.size)
			if err != nil {
				return nil, errf("track %d sample %d: %w", t.trackID, j, err)
			}
			h.Write(data)
			processed += int64(len(data))
			if o.Progress != nil && j%64 == 0 {
				o.Progress(processed, fi.Size())
			}
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != want {
			mismatches = append(mismatches, HashMismatch{TrackID: t.trackID, Stored: want, Actual: got})
		}
	}
	return mismatches, nil
}
