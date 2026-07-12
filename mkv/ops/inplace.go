package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// EditInPlace rewrites only the metadata region of path on disk (instant, no
// cluster copy). It writes directly over the source via Seek+Write, so it is
// NOT crash-safe: a crash mid-write can corrupt the file, and it fails if the
// new metadata does not fit the existing region (adjacent Void space counts as
// room - files written by mkvgo reserve some on purpose). For precious or
// untrusted data, prefer EditMetadata, which writes a fresh file the caller
// can atomically rename.
//
// The head SeekHead is rebuilt to point at the edited elements (keeping its
// entries to elements outside the head, e.g. the Cues). A Tags element sitting
// after the clusters (as Mux writes statistics tags) is folded into the head
// metadata and its old bytes voided, so tags are not duplicated.
func EditInPlace(ctx context.Context, path string, edit func(*mkv.Container), opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return err
	}

	edit(c)

	region, err := findMetadataRegion(path, fs)
	if err != nil {
		return fmt.Errorf("scan metadata: %w", err)
	}

	// Serialise the metadata elements, recording each element's offset within
	// the buffer so the rebuilt SeekHead can point at them.
	var newMeta bytes.Buffer
	var entries []writer.SeekEntry
	mark := func(id uint32) { entries = append(entries, writer.SeekEntry{ID: id, Pos: int64(newMeta.Len())}) }

	mark(mkv.IDInfo)
	if err := writer.WriteSegmentInfo(&newMeta, &c.Info, c.DurationMs); err != nil {
		return err
	}
	if len(c.Tracks) > 0 {
		mark(mkv.IDTracks)
		if err := writer.WriteTracks(&newMeta, c.Tracks); err != nil {
			return err
		}
	}
	if len(c.Chapters) > 0 {
		mark(mkv.IDChapters)
		if err := writer.WriteChapters(&newMeta, c.Chapters); err != nil {
			return err
		}
	}
	if len(c.Attachments) > 0 {
		mark(mkv.IDAttachments)
		if err := writer.WriteAttachments(&newMeta, c.Attachments); err != nil {
			return err
		}
	}
	if len(c.Tags) > 0 {
		mark(mkv.IDTags)
		if err := writer.WriteTags(&newMeta, c.Tags); err != nil {
			return err
		}
	}

	// Lay the region out: a SeekHead slot (when the file had one) then the
	// metadata, with the leftover voided. The old SeekHead sits at the region
	// start, so ANY write here destroys it - rebuilding it is mandatory, never
	// optional, or the Cues past the clusters would only be findable by a full
	// walk.
	//
	// A layout that leaves exactly ONE byte over is unwritable: the smallest
	// EBML element spans 2 bytes (a 1-byte ID and a 1-byte size). Rather than
	// refuse an otherwise valid edit over a single byte, absorb it into the
	// metadata - the first element's data size is re-encoded one byte wider,
	// which EBML explicitly allows - and lay the region out again.
	metaBytes := newMeta.Bytes()
	available := region.end - region.start
	reserve, seekData, err := fitRegion(region, entries, metaBytes, available)
	if errors.Is(err, errOddByte) {
		var perr error
		if metaBytes, entries, perr = widenFirstDataSize(metaBytes, entries); perr != nil {
			return fmt.Errorf("new metadata (%d bytes) leaves exactly 1 byte of the %d available, which no EBML element can fill: %w", len(metaBytes), available, perr)
		}
		reserve, seekData, err = fitRegion(region, entries, metaBytes, available)
	}
	if err != nil {
		return err
	}
	newSize := int64(len(metaBytes))

	f, err := fs.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(region.start, io.SeekStart); err != nil {
		return err
	}
	if reserve > 0 {
		if _, err := f.Write(seekData); err != nil {
			return err
		}
		if pad := int(reserve) - len(seekData); pad >= 2 {
			if err := writer.WriteVoid(f, pad); err != nil {
				return err
			}
		}
	}
	if _, err := f.Write(metaBytes); err != nil {
		return err
	}

	// The layout above leaves either nothing or a paddable gap: a 1-byte
	// leftover was refused before the first write.
	if remaining := int(available - newSize - reserve); remaining >= 2 {
		if err := writer.WriteVoid(f, remaining); err != nil {
			return err
		}
	}

	// The tags now live in the head region: void the old post-cluster Tags
	// element so they are not read twice.
	if region.tailTags.size > 0 {
		if _, err := f.Seek(region.tailTags.pos, io.SeekStart); err != nil {
			return err
		}
		if err := writer.WriteVoid(f, int(region.tailTags.size)); err != nil {
			return err
		}
	}

	// Flush to stable storage. NOTE: the write above is in-place and NOT atomic  -
	// a crash mid-write can corrupt the source file. Use EditMetadata (full
	// rewrite to a new file) when crash safety matters.
	if s, ok := f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
	}
	return nil
}

// errOddByte reports a layout that would leave exactly one byte of the region
// unfilled - unwritable, since the smallest EBML element spans two bytes. The
// caller resolves it by growing the metadata a byte (widenFirstDataSize).
var errOddByte = errors.New("one byte would be left over")

// fitRegion lays the metadata out inside the region: it returns the size of the
// SeekHead slot reserved at the region start (0 when the file had no SeekHead,
// the only case where nothing has to be rebuilt) and the SeekHead bytes to put
// in it.
func fitRegion(region metadataRegion, entries []writer.SeekEntry, meta []byte, available int64) (int64, []byte, error) {
	newSize := int64(len(meta))
	if newSize > available {
		return 0, nil, fmt.Errorf("new metadata (%d bytes) exceeds available space (%d bytes) - use full rewrite instead", newSize, available)
	}
	if !region.hadSeekHead {
		if available-newSize == 1 {
			return 0, nil, errOddByte
		}
		return 0, nil, nil
	}
	return fitSeekHead(region, entries, newSize, available)
}

// widenFirstDataSize re-encodes the first element's data size one byte wider,
// growing the serialised metadata by exactly one byte without changing what it
// means (EBML allows a Data Size in more bytes than strictly necessary). The
// elements after the first one shift by a byte, so their SeekHead entries do
// too - the first element itself still starts at offset 0.
func widenFirstDataSize(meta []byte, entries []writer.SeekEntry) ([]byte, []writer.SeekEntry, error) {
	h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(meta))
	if err != nil {
		return meta, entries, fmt.Errorf("read the first metadata element: %w", err)
	}
	idLen := ebml.ElementIDLen(h.ID)
	width := hdrLen - idLen + 1
	if h.Size < 0 || width > 8 {
		return meta, entries, fmt.Errorf("the first metadata element's size cannot be widened")
	}

	var out bytes.Buffer
	out.Write(meta[:idLen]) // the ID, unchanged
	if _, err := ebml.WriteDataSizeWidth(&out, h.Size, width); err != nil {
		return meta, entries, err
	}
	out.Write(meta[hdrLen:]) // the body, and every element after it

	shifted := make([]writer.SeekEntry, len(entries))
	for i, e := range entries {
		if e.Pos > 0 { // the first element stays at 0; the rest moved a byte
			e.Pos++
		}
		shifted[i] = e
	}
	return out.Bytes(), shifted, nil
}

// fitSeekHead sizes the SeekHead slot placed at the start of the rewritten
// region and serialises the SeekHead for it. The entry positions are relative
// to the Segment data and therefore depend on the slot size itself, so the two
// are solved by iteration: start from the standard reserve, shrink it to what
// the region can host, grow it back to the exact encoded size when the entries
// need more.
//
// Every gap it leaves must be paddable, because the smallest EBML element is
// 2 bytes (a 1-byte ID and a 1-byte size): neither the slot's own padding
// (slot - len(SeekHead)) nor the region's tail (available - newSize - slot) may
// come out at exactly 1. A 1-byte gap left unwritten would shift the metadata
// off the very offsets the SeekHead advertises.
//
// It fails - rather than dropping the SeekHead - when no slot fits: the caller
// writes over the old SeekHead either way, so a file that cannot host a new one
// must be rewritten in full instead.
func fitSeekHead(region metadataRegion, head []writer.SeekEntry, newSize, available int64) (int64, []byte, error) {
	slot := int64(writer.SeekHeadReserve)
	if room := available - newSize; slot > room {
		slot = room
	}
	for i := 0; i < 8 && slot >= 2; i++ {
		sh, err := buildSeekHead(region, head, slot)
		if err != nil {
			return 0, nil, err
		}
		n := int64(len(sh))
		switch slack := available - newSize - n; {
		case n > slot: // slot too small: grow it to what the entries need
			if newSize+n > available {
				slot = 0 // even an exact fit overflows the region
				continue
			}
			slot = n
		case slack == 1:
			// However the split goes, one byte is left over and no element can
			// fill it. The caller absorbs it into the metadata and comes back.
			return 0, nil, errOddByte
		case slot-n == 1: // unpaddable gap inside the slot: shrink to an exact fit
			slot = n // the leftover moves to the tail, where slack >= 2 covers it
		case available-newSize-slot == 1: // unpaddable 1-byte tail: absorb it into the slot
			slot++ // the slot's own padding was >= 2, so it stays paddable
		default:
			return slot, sh, nil
		}
	}
	return 0, nil, fmt.Errorf("new metadata (%d bytes) plus the rebuilt SeekHead exceeds available space (%d bytes) - use full rewrite instead", newSize, available)
}

// buildSeekHead serialises the head SeekHead for a region rewritten with slot
// bytes reserved at its start: the head elements move past the slot, and the
// old entries pointing outside the head (e.g. the Cues) are kept verbatim.
func buildSeekHead(region metadataRegion, head []writer.SeekEntry, slot int64) ([]byte, error) {
	relBase := region.start - region.segDataStart + slot
	all := make([]writer.SeekEntry, 0, len(head)+len(region.keptSeekEntries))
	for _, e := range head {
		all = append(all, writer.SeekEntry{ID: e.ID, Pos: e.Pos + relBase})
	}
	all = append(all, region.keptSeekEntries...)
	var buf bytes.Buffer
	if err := writer.WriteSeekHead(&buf, all); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type metadataRegion struct {
	start int64
	end   int64
	// segDataStart is the absolute offset of the Segment data (SeekHead
	// positions are relative to it).
	segDataStart int64
	hadSeekHead  bool
	// seekHeadPos is the absolute offset of the head SeekHead. It is NOT always
	// region.start: a file may reserve a Void before it.
	seekHeadPos int64
	// keptSeekEntries are the old SeekHead entries pointing OUTSIDE the head
	// region (e.g. the Cues after the clusters), preserved verbatim.
	keptSeekEntries []writer.SeekEntry
	// tailTags is a Tags element sitting after the head region (Mux writes the
	// statistics tags there); its bytes are voided after an in-place edit.
	tailTags struct{ pos, size int64 }
}

func findMetadataRegion(path string, fs *mkv.FS) (metadataRegion, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return metadataRegion{}, err
	}
	defer f.Close()

	h, _, err := ebml.ReadElementHeader(f)
	if err != nil {
		return metadataRegion{}, err
	}
	if _, err := f.Seek(h.Size, io.SeekCurrent); err != nil {
		return metadataRegion{}, err
	}

	h, _, err = ebml.ReadElementHeader(f)
	if err != nil {
		return metadataRegion{}, err
	}
	if h.ID != mkv.IDSegment {
		return metadataRegion{}, fmt.Errorf("expected Segment")
	}

	var segEnd int64 = -1
	region := metadataRegion{start: -1}
	region.segDataStart, _ = f.Seek(0, io.SeekCurrent)
	if h.Size >= 0 {
		segEnd = region.segDataStart + h.Size
	}

	for {
		if segEnd >= 0 {
			cur, _ := f.Seek(0, io.SeekCurrent)
			if cur >= segEnd {
				break
			}
		}

		pos, _ := f.Seek(0, io.SeekCurrent)
		eh, _, err := ebml.ReadElementHeader(f)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return metadataRegion{}, err
		}

		switch eh.ID {
		case mkv.IDInfo, mkv.IDTracks, mkv.IDChapters, mkv.IDAttachments, mkv.IDTags, mkv.IDSeekHead, mkv.IDVoid:
			if eh.Size < 0 { // unknown-size metadata: a negative Seek would corrupt the scan
				return metadataRegion{}, fmt.Errorf("unknown-size metadata element 0x%X", eh.ID)
			}
			if region.start < 0 {
				region.start = pos
			}
			region.end = pos + int64(ebml.ElementIDLen(eh.ID)) + int64(ebml.DataSizeLen(eh.Size)) + eh.Size
			if eh.ID == mkv.IDSeekHead {
				if !region.hadSeekHead {
					region.hadSeekHead, region.seekHeadPos = true, pos
				}
				if _, err := f.Seek(eh.Size, io.SeekCurrent); err != nil {
					return metadataRegion{}, err
				}
				continue
			}
			if _, err := f.Seek(eh.Size, io.SeekCurrent); err != nil {
				return metadataRegion{}, err
			}
		case mkv.IDCluster:
			if region.start < 0 {
				region.start = pos
			}
			if region.end == 0 {
				region.end = pos
			}
			if region.hadSeekHead {
				if err := classifyTailEntries(f, path, fs, &region); err != nil {
					return metadataRegion{}, err
				}
			}
			return region, nil
		default:
			if eh.Size < 0 {
				return metadataRegion{}, fmt.Errorf("unknown-size element 0x%X", eh.ID)
			}
			if _, err := f.Seek(eh.Size, io.SeekCurrent); err != nil {
				return metadataRegion{}, err
			}
		}
	}

	if region.start < 0 {
		return metadataRegion{}, fmt.Errorf("no metadata found")
	}
	return region, nil
}

// classifyTailEntries re-reads the old SeekHead and sorts its entries: those
// pointing outside the head region are preserved for the rebuilt SeekHead  -
// except a post-cluster Tags element, which the in-place edit folds into the
// head (its location and full size are recorded so it can be voided).
func classifyTailEntries(f mkv.ReadSeekCloser, path string, fs *mkv.FS, region *metadataRegion) error {
	entries, err := readSeekHeadEntries(path, fs, region)
	if err != nil || entries == nil {
		return err
	}
	for _, e := range entries {
		abs := region.segDataStart + e.Pos
		if abs >= region.start && abs < region.end {
			continue // head element: rewritten, gets a fresh entry
		}
		if e.ID == mkv.IDTags {
			// Measure the element so its bytes can be voided.
			if _, err := f.Seek(abs, io.SeekStart); err != nil {
				return err
			}
			th, hdrLen, err := ebml.ReadElementHeader(f)
			if err != nil || th.ID != mkv.IDTags || th.Size < 0 {
				continue // stale entry: leave it out, void nothing
			}
			region.tailTags.pos = abs
			region.tailTags.size = int64(hdrLen) + th.Size
			continue
		}
		region.keptSeekEntries = append(region.keptSeekEntries, e)
	}
	return nil
}

// readSeekHeadEntries parses the first SeekHead's entries (SeekID +
// SeekPosition pairs). A malformed SeekHead yields nil entries, not an error:
// the edit then simply rebuilds without preserved entries.
func readSeekHeadEntries(path string, fs *mkv.FS, region *metadataRegion) ([]writer.SeekEntry, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(region.seekHeadPos, io.SeekStart); err != nil {
		return nil, err
	}
	sh, _, err := ebml.ReadElementHeader(f)
	if err != nil || sh.ID != mkv.IDSeekHead || sh.Size < 0 {
		return nil, nil
	}
	body := make([]byte, sh.Size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, nil
	}
	return parseSeekHeadBody(body), nil
}

// parseSeekHeadBody parses a SeekHead element's raw body into SeekEntry pairs
// (SeekID + SeekPosition). A malformed or truncated entry simply stops the
// scan and returns whatever was parsed so far, rather than erroring: callers
// treat a partial/empty result as "no preserved entries", not a hard failure.
func parseSeekHeadBody(body []byte) []writer.SeekEntry {
	var out []writer.SeekEntry
	br := bytes.NewReader(body)
	for {
		eh, _, err := ebml.ReadElementHeader(br)
		if err != nil {
			break
		}
		if eh.ID != mkv.IDSeek || eh.Size < 0 {
			if eh.Size < 0 {
				return out
			}
			if _, err := br.Seek(eh.Size, io.SeekCurrent); err != nil {
				return out
			}
			continue
		}
		inner := make([]byte, eh.Size)
		if _, err := io.ReadFull(br, inner); err != nil {
			return out
		}
		ir := bytes.NewReader(inner)
		var entry writer.SeekEntry
		for {
			ih, _, err := ebml.ReadElementHeader(ir)
			if err != nil {
				break
			}
			switch ih.ID {
			case mkv.IDSeekID:
				raw, err := ebml.ReadBytes(ir, ih.Size)
				if err != nil {
					return out
				}
				var id uint32
				for _, b := range raw {
					id = id<<8 | uint32(b)
				}
				entry.ID = id
			case mkv.IDSeekPosition:
				v, err := ebml.ReadUint(ir, ih.Size)
				if err != nil {
					return out
				}
				entry.Pos = int64(v)
			default:
				if ih.Size < 0 {
					return out
				}
				if _, err := ir.Seek(ih.Size, io.SeekCurrent); err != nil {
					return out
				}
			}
		}
		if entry.ID != 0 {
			out = append(out, entry)
		}
	}
	return out
}
