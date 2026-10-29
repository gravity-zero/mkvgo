package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// FingerprintReport is a container-independent content identity for a file:
// the same audio/video/subtitle payloads produce the same Presentation hash
// whether the source is Matroska, WebM, MP4 or MOV, so a media library can
// detect that two files are re-muxes of the same content (different title,
// muxing app, container, or track order) without a byte-for-byte comparison
// of the containers themselves. See Fingerprint.
type FingerprintReport struct {
	// Presentation is a stable hex SHA-256 identity for the whole file's
	// content, independent of track order and container metadata. See
	// Fingerprint for the exact recipe.
	Presentation string `json:"presentation"`
	// Tracks is one digest per track, in Container.Tracks order (the order a
	// caller can correlate with TrackID from info/tracks/analyze).
	Tracks []TrackFingerprint `json:"tracks"`
}

// TrackFingerprint is one track's content identity: SHA256 digests the
// frames as CONTENT - a compressed track inflated, a stripped header put back
// - so the same frames hash the same whatever the container did to them. It
// equals the stored-bytes digest CompareBlocks and the CONTENT_SHA256 tags use
// only for a track stored as is.
type TrackFingerprint struct {
	TrackID uint64 `json:"track_id"`
	Type    string `json:"type"`
	Codec   string `json:"codec"`
	SHA256  string `json:"sha256"`
}

// Fingerprint computes a container-independent content identity for path: a
// per-track SHA-256 over the track's frame payloads in decode order (the
// exact digest CompareBlocks proves round-trips with), plus a Presentation
// hash over all of them that a media library can use to dedup re-muxes of
// identical content regardless of container metadata or track order.
//
// Presentation recipe (reproducible independently of this implementation):
//  1. For each track, take hex(SHA-256 over its frame payloads in decode
//     order) - TrackFingerprint.SHA256.
//  2. Build a sort key "type|codec|sha256hex" for each track and sort the
//     tracks by that key, ascending, byte-wise. Sorting by content (not by
//     TrackID or file order) means a remux that reorders tracks produces the
//     same sorted sequence.
//  3. Concatenate the sorted tracks' raw 32-byte SHA-256 sums (not their hex
//     form) in that order, and take the SHA-256 of the concatenation.
//  4. Presentation is that final hash, hex-encoded.
//
// This is a FULL read, unlike Analyze: every track's frame payload is read
// and hashed, so the cost is proportional to the media volume, not just the
// block-header count.
//
// MP4/MOV sources are hashed straight from their samples as the blocks a
// remux to Matroska would hold (see digestTracksMP4), one sample at a time:
// RemuxFromMP4 copies every audio/video sample's compressed bytes verbatim, so the digest is over the
// exact same bytes CompareBlocks and this function hash for a native
// Matroska/WebM source, and an MP4 carrying the same encode as an MKV
// fingerprints identically. The one normalization is subtitles: RemuxFromMP4
// decodes MP4 subtitle samples (tx3g/WebVTT) to plain UTF-8 text rather than
// carrying their raw MP4 sample framing, so a subtitle track's digest is over
// that decoded text, not the MP4 container bytes. Tracks in a codec
// RemuxFromMP4 does not carry (see its doc) are absent from the report, the
// same way CompareBlocks/RemuxFromMP4 drop them.
//
// A source cut short is refused - a Matroska that
// stops inside its media, an MP4 whose sample tables place media past the end
// of the file (ErrSourceTruncated) - rather than digested over what arrived:
// that digest would pass for the complete file's.
func Fingerprint(ctx context.Context, path string, opts ...mkv.Options) (*FingerprintReport, error) {
	if err := oneopt.Check(opts); err != nil {
		return nil, err
	}
	fs := mkv.FSFrom(opts)
	var cut *mp4.Truncation
	c, digests, err := digestTracksAny(ctx, path, fs, mkv.ProgressFrom(opts), func(t mp4.Truncation) { cut = &t })
	if err == nil && cut != nil {
		err = fmt.Errorf("%w: it ends at byte %d, its media at %s: fingerprint it once the file is complete", ErrSourceTruncated, cut.FileSize, cut.Reach())
	}
	// A Matroska cut inside a Cluster fails the walk, one cut between two ends it cleanly: either way the Segment
	// declares more than the file holds (surplus bytes past its end are trailing junk, not a cut).
	if (err == nil && cut == nil) || errors.Is(err, io.ErrUnexpectedEOF) {
		if declared, known, derr := segmentDeclaredEndOf(path, fs); derr == nil && known {
			if st, serr := fs.DoStat(path); serr == nil && declared > st.Size() {
				err = fmt.Errorf("%w: it ends at byte %d of the %d its Segment declares: fingerprint it once the file is complete", ErrSourceTruncated, st.Size(), declared)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("digest %s: %w", shown.Path(path), err)
	}

	fp := &FingerprintReport{Tracks: make([]TrackFingerprint, len(c.Tracks))}
	type sortEntry struct {
		key string
		sum [sha256.Size]byte
	}
	entries := make([]sortEntry, len(c.Tracks))
	for i, t := range c.Tracks {
		hexSum := hex.EncodeToString(digests[i].hash[:])
		fp.Tracks[i] = TrackFingerprint{TrackID: t.ID, Type: string(t.Type), Codec: t.Codec, SHA256: hexSum}
		entries[i] = sortEntry{key: string(t.Type) + "|" + t.Codec + "|" + hexSum, sum: digests[i].hash}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	h := sha256.New()
	for _, e := range entries {
		h.Write(e.sum[:])
	}
	fp.Presentation = hex.EncodeToString(h.Sum(nil))

	return fp, nil
}

// digestTracksAny is digestTracks extended to MP4/MOV sources, the reader
// chosen by the first bytes (opensLikeMP4), as Playability's openAnyMeta.
func digestTracksAny(ctx context.Context, path string, fs *mkv.FS, progress mkv.ProgressFunc, onCut func(mp4.Truncation)) (*mkv.Container, []trackDigest, error) {
	isMP4, fs, err := opensLikeMP4(ctx, path, fs)
	if err != nil {
		return nil, nil, err
	}
	if isMP4 {
		return digestTracksMP4(ctx, path, fs, progress, onCut)
	}
	return digestTracks(ctx, path, fs, progress, true)
}

// digestTracksMP4 hashes an MP4/MOV source's tracks the way digestTracks
// hashes a Matroska one: each block its remux would write (mp4.ForEachBlock:
// sample bytes verbatim, subtitles decoded to text) folded into its track's
// digest, read straight from the source through fs, one sample held at a time.
func digestTracksMP4(ctx context.Context, path string, fs *mkv.FS, progress mkv.ProgressFunc, onCut func(mp4.Truncation)) (*mkv.Container, []trackDigest, error) {
	byID := map[uint64]*digestAcc{}
	srcFS := &mkv.FS{Open: fs.DoOpen, OpenContext: fs.DoOpenContext, Stat: fs.DoStat}
	c, err := mp4.ForEachBlock(ctx, path, func(b mkv.Block) error {
		a := byID[b.TrackNumber]
		if a == nil {
			a = &digestAcc{h: sha256.New()}
			byID[b.TrackNumber] = a
		}
		a.d.blocks++
		a.d.bytes += int64(len(b.Data))
		_, _ = a.h.Write(b.Data) // hash.Hash.Write never errors
		return nil
	}, mp4.Options{FS: srcFS, Progress: progress, OnTruncated: onCut})
	if err != nil {
		return nil, nil, err
	}
	accs := newTrackAccs(len(c.Tracks))
	for i, t := range c.Tracks {
		if a := byID[t.ID]; a != nil {
			accs[i] = a
		}
	}
	return c, sealTrackAccs(accs), nil
}
