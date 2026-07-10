package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
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

// TrackFingerprint is one track's content identity: SHA256 is the same
// payload digest (in decode order) CompareBlocks computes to prove a
// round-trip byte-identical.
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
// MP4/MOV sources are supported by remuxing to a temporary Matroska file
// first (see digestTracksMP4) and hashing that: RemuxFromMP4 copies every
// audio/video sample's compressed bytes verbatim, so the digest is over the
// exact same bytes CompareBlocks and this function hash for a native
// Matroska/WebM source, and an MP4 carrying the same encode as an MKV
// fingerprints identically. The one normalization is subtitles: RemuxFromMP4
// decodes MP4 subtitle samples (tx3g/WebVTT) to plain UTF-8 text rather than
// carrying their raw MP4 sample framing, so a subtitle track's digest is over
// that decoded text, not the MP4 container bytes. Tracks in a codec
// RemuxFromMP4 does not carry (see its doc) are absent from the report, the
// same way CompareBlocks/RemuxFromMP4 drop them.
func Fingerprint(ctx context.Context, path string, opts ...mkv.Options) (*FingerprintReport, error) {
	fs := mkv.FSFrom(opts)
	c, digests, err := digestTracksAny(ctx, path, fs, mkv.ProgressFrom(opts))
	if err != nil {
		return nil, fmt.Errorf("digest %s: %w", path, err)
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

// digestTracksAny is digestTracks extended to MP4/MOV sources: it dispatches
// by extension first and, for a mislabeled file, falls back on content
// sniffing (reader.ErrNotMatroska), mirroring Playability's openAnyMeta.
func digestTracksAny(ctx context.Context, path string, fs *mkv.FS, progress mkv.ProgressFunc) (*mkv.Container, []trackDigest, error) {
	if isMP4Ext(path) {
		return digestTracksMP4(ctx, path, fs, progress)
	}
	c, digests, err := digestTracks(ctx, path, fs, progress)
	if err != nil && errors.Is(err, reader.ErrNotMatroska) {
		return digestTracksMP4(ctx, path, fs, progress)
	}
	return c, digests, err
}

// digestTracksMP4 hashes an MP4/MOV source's tracks the same way digestTracks
// hashes a Matroska one, by remuxing it to a temporary Matroska file (raw
// sample bytes copied verbatim for every codec RemuxFromMP4 supports) and
// running the exact same digest engine on that file. The temporary file
// always lives on local disk regardless of fs, since it is throwaway scratch
// space, not the caller's data; only the read of the MP4 source goes through
// fs (so a remote MP4 fingerprints like a remote MKV).
func digestTracksMP4(ctx context.Context, path string, fs *mkv.FS, progress mkv.ProgressFunc) (*mkv.Container, []trackDigest, error) {
	tmp, err := os.CreateTemp("", "mkvgo-fingerprint-*.mkv")
	if err != nil {
		return nil, nil, err
	}
	tmpPath := tmp.Name()
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(tmpPath)
		return nil, nil, cerr
	}
	defer os.Remove(tmpPath)

	srcFS := &mkv.FS{Open: fs.DoOpen, Stat: fs.DoStat}
	if err := mp4.RemuxFromMP4(ctx, path, tmpPath, mp4.Options{FS: srcFS, Progress: progress}); err != nil {
		return nil, nil, fmt.Errorf("remux to temporary matroska: %w", err)
	}
	return digestTracks(ctx, tmpPath, nil, nil)
}
