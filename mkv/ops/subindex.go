package ops

// subindex.go - a seek index for subtitle tracks. Matroska's own Cues index the
// video track, so reaching one subtitle track's blocks means walking every
// block header in the file: on a 15.8 GB 2160p source that is ~12 M headers and
// tens of seconds, for a few KiB of cues. An MP4 pays none of this because its
// sample table already records where every sample of every track is; this is
// that table, for Matroska, built once and served many times.
//
// mkvgo builds, encodes and decodes the index; it never stores one. Where the
// bytes live and when they are thrown away is the caller's - so the index
// carries a fingerprint of the file it was built from, and a serve refuses a
// mismatch rather than seeking to a stale offset and emitting whatever it
// finds there.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// ErrIndexStale is returned when an index does not describe the file it is
// being used against - a different size or Segment UID, or a recorded position
// that does not hold the block it claims. The remedy is to rebuild the index.
var ErrIndexStale = errors.New("the subtitle index does not match this file")

// ErrTrackNotIndexed is returned when the requested track was not among the
// tracks the index was built for.
var ErrTrackNotIndexed = errors.New("the track is not in this subtitle index")

const (
	// maxLaceFrames is the most frames one block can hold: a lace stores its
	// frame count in a single byte, as count-minus-one.
	maxLaceFrames = 256

	subIndexMagic   = "MKVGOSIX"
	subIndexVersion = 2
)

// subEntry is one indexed BLOCK - not one frame. A laced block holds several
// frames behind a single position (BlockPos names the block element, and every
// frame of the lace shares it), so the entry carries how many frames to take
// once the reader is seated there. frames is always >= 1.
type subEntry struct {
	pos    reader.BlockPos
	timeMs int64
	frames int64
}

// SubtitleIndex records the position of every block of the subtitle tracks it
// was built for, alongside a fingerprint of the source. It is built by
// BuildSubtitleIndex, carried by the caller in whatever store it likes (it
// marshals to a compact byte slice), and consumed by ExtractSubtitleWebVTTFrom.
//
// The zero value is not usable; a decoded or freshly built index is.
type SubtitleIndex struct {
	fileSize   int64
	tcScale    int64
	segmentUID []byte
	order      []uint64 // track IDs, in the order they were indexed
	entries    map[uint64][]subEntry
}

// Tracks returns the track IDs the index covers, in index order.
func (ix *SubtitleIndex) Tracks() []uint64 {
	if ix == nil {
		return nil
	}
	out := make([]uint64, len(ix.order))
	copy(out, ix.order)
	return out
}

// Blocks returns how many blocks the index holds for a track (0 when the track
// is not covered).
func (ix *SubtitleIndex) Blocks(trackID uint64) int {
	if ix == nil {
		return 0
	}
	return len(ix.entries[trackID])
}

// SourceSize returns the size of the file the index was built from - the first
// half of the fingerprint a caller keying its own cache will want to compare.
func (ix *SubtitleIndex) SourceSize() int64 {
	if ix == nil {
		return 0
	}
	return ix.fileSize
}

// BuildSubtitleIndex walks srcPath once and records the position of every block
// of the named tracks; passing no track IDs indexes every subtitle track, which
// costs the same walk and lets one build serve them all. The walk reads block
// headers only - no payload of any track is read - so its cost is the file's
// block count, not its size.
//
// This is the expensive half: it is the same single pass a direct extraction
// makes. It pays for itself from the second extraction of the file onwards.
func BuildSubtitleIndex(ctx context.Context, srcPath string, trackIDs []uint64, opts ...mkv.Options) (*SubtitleIndex, error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs)
	if err != nil {
		return nil, err
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		return nil, err
	}

	keep, err := resolveIndexTracks(c, trackIDs)
	if err != nil {
		return nil, err
	}

	ix := &SubtitleIndex{
		fileSize:   st.Size(),
		tcScale:    c.Info.TimecodeScale,
		segmentUID: append([]byte(nil), c.Info.SegmentUID...),
		order:      keep,
		entries:    make(map[uint64][]subEntry, len(keep)),
	}
	// Seed every kept track, so a subtitle track that holds no block at all is
	// "indexed, empty" rather than "not indexed" - the same answer a decoded
	// index gives, which reconstructs the key unconditionally.
	for _, id := range keep {
		ix.entries[id] = nil
	}

	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return nil, err
	}
	br.KeepTracks(keep...)
	br.SetHeaderOnly(true) // positions only: not one subtitle payload is read either

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// Every frame of a laced block reports the same position, so a naive
		// append would record N entries pointing at one block - and serving
		// them would replay frame 0 N times. Coalesce them into one entry and
		// count the frames instead.
		at := br.Pos()
		list := ix.entries[blk.TrackNumber]
		if n := len(list); n > 0 && list[n-1].pos == at {
			list[n-1].frames++
			ix.entries[blk.TrackNumber] = list
			continue
		}
		ix.entries[blk.TrackNumber] = append(list, subEntry{pos: at, timeMs: blk.Timecode, frames: 1})
	}
	return ix, nil
}

// resolveIndexTracks turns the caller's track list into the set to keep: the
// file's subtitle tracks when the list is empty, else exactly those asked for,
// validated against the file.
func resolveIndexTracks(c *mkv.Container, trackIDs []uint64) ([]uint64, error) {
	if len(trackIDs) == 0 {
		var all []uint64
		for _, t := range c.Tracks {
			if t.Type == mkv.SubtitleTrack {
				all = append(all, t.ID)
			}
		}
		if len(all) == 0 {
			return nil, fmt.Errorf("no subtitle track to index")
		}
		return all, nil
	}
	seen := make(map[uint64]bool, len(trackIDs))
	out := make([]uint64, 0, len(trackIDs))
	for _, id := range trackIDs {
		if seen[id] {
			continue
		}
		found := false
		for _, t := range c.Tracks {
			if t.ID != id {
				continue
			}
			if t.Type != mkv.SubtitleTrack {
				// Indexing a video track would record a position per frame -
				// hundreds of megabytes on a feature film - for an index no
				// extractor can serve.
				return nil, fmt.Errorf("track %d is not a subtitle track", id)
			}
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("subtitle track %d not found", id)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// ExtractSubtitleWebVTTFrom is ExtractSubtitleWebVTT served from a prebuilt
// index: instead of walking the file it seeks straight to each recorded block.
// The index must describe srcPath - size and Segment UID are checked up front,
// and every block read must carry the track and timecode the index recorded -
// or ErrIndexStale is returned before any cue is written.
func ExtractSubtitleWebVTTFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, w io.Writer, opts ...mkv.Options) error {
	if ix == nil {
		return fmt.Errorf("nil subtitle index")
	}
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		return err
	}
	if err := ix.checkAgainst(c, st.Size()); err != nil {
		return err
	}

	codec, err := textSubtitleCodec(c, trackID)
	if err != nil {
		return err
	}
	entries, ok := ix.entries[trackID]
	if !ok {
		return fmt.Errorf("%w: track %d", ErrTrackNotIndexed, trackID)
	}

	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var br *reader.BlockReader
	cues := make([]subtitle.Cue, 0, len(entries))
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &entries[i]
		if br == nil {
			// The first position seats the reader; the rest re-seat it, so the
			// read window is allocated once for the whole track.
			br, err = reader.NewBlockReaderFrom(f, ix.tcScale, e.pos)
			if err != nil {
				return err
			}
			br.KeepTracks(trackID)
			// A mid-file reader never walks over the Tracks element, so it
			// cannot pick the per-frame strides up on its own: without them a
			// laced block's frames would all carry the block timecode, and the
			// output would differ from the walking extractor's.
			br.SetTrackDefaultDurations(reader.TrackDefaultDurations(c.Tracks))
		} else if err := br.SeekTo(e.pos); err != nil {
			return err
		}
		// One entry, e.frames frames: an unlaced block takes exactly one turn
		// here, a laced one takes its whole lace without re-seeking.
		for f := int64(0); f < e.frames; f++ {
			blk, err := br.Next()
			if err != nil {
				return fmt.Errorf("%w: reading the block at offset %d: %w", ErrIndexStale, e.pos.Off, err)
			}
			if blk.TrackNumber != trackID {
				return fmt.Errorf("%w: offset %d holds track %d, the index recorded track %d",
					ErrIndexStale, e.pos.Off, blk.TrackNumber, trackID)
			}
			// Only the block's first frame carries the recorded timecode; the
			// rest are strided by the track's DefaultDuration, which a mid-file
			// reader cannot know - so they are not checked against the index.
			if f == 0 && blk.Timecode != e.timeMs {
				return fmt.Errorf("%w: offset %d holds track %d at %d ms, the index recorded %d ms",
					ErrIndexStale, e.pos.Off, trackID, blk.Timecode, e.timeMs)
			}
			text := decodeSubtitleCue(codec, blk.Data)
			if text == "" {
				continue
			}
			end := int64(0)
			if blk.Duration > 0 {
				end = blk.Timecode + blk.Duration
			}
			cues = append(cues, subtitle.Cue{StartMs: blk.Timecode, EndMs: end, Text: text})
		}
	}
	subtitle.ResolveCueEnds(cues, defaultSubDurationMs)
	return subtitle.WriteWebVTT(w, cues)
}

// checkAgainst rejects an index built from a different file. The size and the
// Segment UID are what a Matroska file cheaply offers; neither is proof on its
// own, which is why every block is verified as it is read.
func (ix *SubtitleIndex) checkAgainst(c *mkv.Container, size int64) error {
	if ix.fileSize != size {
		return fmt.Errorf("%w: built for %d bytes, this file is %d", ErrIndexStale, ix.fileSize, size)
	}
	if len(ix.segmentUID) > 0 && len(c.Info.SegmentUID) > 0 && !bytes.Equal(ix.segmentUID, c.Info.SegmentUID) {
		return fmt.Errorf("%w: a different Segment UID", ErrIndexStale)
	}
	if ix.tcScale != c.Info.TimecodeScale {
		return fmt.Errorf("%w: a different timecode scale", ErrIndexStale)
	}
	return nil
}

// textSubtitleCodec resolves the codec of a track that must exist, be a
// subtitle track, and be convertible to WebVTT - the same checks
// ExtractSubtitleWebVTT makes before it reads anything.
func textSubtitleCodec(c *mkv.Container, trackID uint64) (string, error) {
	for _, t := range c.Tracks {
		if t.ID != trackID || t.Type != mkv.SubtitleTrack {
			continue
		}
		if !isTextSubtitle(t.Codec) {
			return "", fmt.Errorf("subtitle track %d codec %q is not text (cannot convert to WebVTT)", trackID, t.Codec)
		}
		return t.Codec, nil
	}
	return "", fmt.Errorf("subtitle track %d not found", trackID)
}

// MarshalBinary encodes the index. Positions and timecodes are monotonic per
// track, so each field is stored as a zigzag varint of its delta from the
// previous entry - a 10 331-block index over eight tracks comes to ~150 KiB
// rather than the ~400 KiB the raw int64s would take.
func (ix *SubtitleIndex) MarshalBinary() ([]byte, error) {
	if ix == nil {
		return nil, fmt.Errorf("nil subtitle index")
	}
	var b []byte
	b = append(b, subIndexMagic...)
	b = append(b, subIndexVersion)
	b = binary.AppendVarint(b, ix.fileSize)
	b = binary.AppendVarint(b, ix.tcScale)
	b = binary.AppendUvarint(b, uint64(len(ix.segmentUID)))
	b = append(b, ix.segmentUID...)
	b = binary.AppendUvarint(b, uint64(len(ix.order)))
	for _, id := range ix.order {
		entries := ix.entries[id]
		b = binary.AppendUvarint(b, id)
		b = binary.AppendUvarint(b, uint64(len(entries)))
		var prev subEntry
		for i := range entries {
			e := &entries[i]
			b = binary.AppendVarint(b, e.pos.Off-prev.pos.Off)
			b = binary.AppendVarint(b, e.pos.ClusterStart-prev.pos.ClusterStart)
			b = binary.AppendVarint(b, e.pos.ClusterEnd-prev.pos.ClusterEnd)
			b = binary.AppendVarint(b, e.pos.ClusterTS-prev.pos.ClusterTS)
			b = binary.AppendVarint(b, e.timeMs-prev.timeMs)
			b = binary.AppendUvarint(b, uint64(e.frames))
			prev = *e
		}
	}
	return b, nil
}

// UnmarshalBinary decodes an index produced by MarshalBinary. A buffer that is
// not one, or that carries a version this build does not know, is rejected -
// never partially decoded.
func (ix *SubtitleIndex) UnmarshalBinary(data []byte) error {
	r := bytes.NewReader(data)
	magic := make([]byte, len(subIndexMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != subIndexMagic {
		return fmt.Errorf("not a mkvgo subtitle index")
	}
	ver, err := r.ReadByte()
	if err != nil {
		return fmt.Errorf("truncated subtitle index")
	}
	if ver != subIndexVersion {
		return fmt.Errorf("subtitle index version %d is not supported (this build writes %d)", ver, subIndexVersion)
	}
	out := &SubtitleIndex{}
	if out.fileSize, err = binary.ReadVarint(r); err != nil {
		return errTruncatedIndex(err)
	}
	if out.tcScale, err = binary.ReadVarint(r); err != nil {
		return errTruncatedIndex(err)
	}
	uidLen, err := binary.ReadUvarint(r)
	if err != nil {
		return errTruncatedIndex(err)
	}
	if uidLen > uint64(r.Len()) {
		return fmt.Errorf("corrupt subtitle index: segment UID declares %d bytes, %d remain", uidLen, r.Len())
	}
	if uidLen > 0 {
		out.segmentUID = make([]byte, uidLen)
		if _, err := io.ReadFull(r, out.segmentUID); err != nil {
			return errTruncatedIndex(err)
		}
	}
	// Each track record costs at least two bytes on the wire (its ID and its
	// entry count), so a declared track count past that is corrupt - and must
	// be refused BEFORE it sizes a map.
	nTracks, err := binary.ReadUvarint(r)
	if err != nil {
		return errTruncatedIndex(err)
	}
	if nTracks > uint64(r.Len())/2 {
		return fmt.Errorf("corrupt subtitle index: %d tracks declared, %d bytes remain", nTracks, r.Len())
	}
	out.entries = make(map[uint64][]subEntry, nTracks)
	for t := uint64(0); t < nTracks; t++ {
		id, err := binary.ReadUvarint(r)
		if err != nil {
			return errTruncatedIndex(err)
		}
		n, err := binary.ReadUvarint(r)
		// Each entry is six varints, so at least six bytes on the wire: a
		// declared count that cannot fit in what is left is corrupt, and must
		// not be allocated for. Bounding by the bytes actually remaining is
		// what keeps a small malformed buffer from asking for a large slice.
		if err != nil {
			return errTruncatedIndex(err)
		}
		if n > uint64(r.Len())/6 {
			return fmt.Errorf("corrupt subtitle index: track %d declares %d entries, %d bytes remain", id, n, r.Len())
		}
		entries := make([]subEntry, 0, n)
		var prev subEntry
		for i := uint64(0); i < n; i++ {
			var e subEntry
			for _, field := range []*int64{&e.pos.Off, &e.pos.ClusterStart, &e.pos.ClusterEnd, &e.pos.ClusterTS, &e.timeMs} {
				d, err := binary.ReadVarint(r)
				if err != nil {
					return errTruncatedIndex(err)
				}
				*field = d
			}
			e.pos.Off += prev.pos.Off
			e.pos.ClusterStart += prev.pos.ClusterStart
			e.pos.ClusterEnd += prev.pos.ClusterEnd
			e.pos.ClusterTS += prev.pos.ClusterTS
			e.timeMs += prev.timeMs
			frames, err := binary.ReadUvarint(r)
			if err != nil {
				return errTruncatedIndex(err)
			}
			// frames counts frames inside the file's block, not bytes on the
			// wire, so the honest bound is the format's: a lace stores its
			// count in one byte, so 256 frames is the most a block can hold.
			// Zero would make the entry unservable.
			if frames == 0 || frames > maxLaceFrames {
				return fmt.Errorf("corrupt subtitle index: entry declares %d frames", frames)
			}
			e.frames = int64(frames)
			entries = append(entries, e)
			prev = e
		}
		if _, dup := out.entries[id]; dup {
			return fmt.Errorf("corrupt subtitle index: track %d appears twice", id)
		}
		out.order = append(out.order, id)
		out.entries[id] = entries
	}
	*ix = *out
	return nil
}

func errTruncatedIndex(err error) error {
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("corrupt subtitle index: %w", err)
	}
	return fmt.Errorf("truncated subtitle index")
}
