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
// room — files written by mkvgo reserve some on purpose). For precious or
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

	// Rebuild the head SeekHead when the file had one: reserve a fixed slot at
	// the region start (like MKVWriter does), shift the metadata offsets past
	// it, and keep the old entries that point outside the head (e.g. Cues).
	newSize := int64(newMeta.Len())
	available := region.end - region.start
	reserve := int64(0)
	var seekData []byte
	if region.hadSeekHead && newSize+writer.SeekHeadReserve <= available {
		reserve = writer.SeekHeadReserve
		relBase := region.start - region.segDataStart + reserve
		for i := range entries {
			entries[i].Pos += relBase
		}
		entries = append(entries, region.keptSeekEntries...)
		var shBuf bytes.Buffer
		if err := writer.WriteSeekHead(&shBuf, entries); err != nil {
			return err
		}
		if int64(shBuf.Len()) > reserve {
			reserve, seekData = 0, nil // does not fit the slot: keep the old layout
		} else {
			seekData = shBuf.Bytes()
		}
	}

	if newSize+reserve > available {
		return fmt.Errorf("new metadata (%d bytes) exceeds available space (%d bytes) — use full rewrite instead", newSize+reserve, available)
	}

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
	if _, err := f.Write(newMeta.Bytes()); err != nil {
		return err
	}

	remaining := int(available - newSize - reserve)
	if remaining >= 2 {
		if err := writer.WriteVoid(f, remaining); err != nil {
			return err
		}
	} else if remaining == 1 {
		if _, err := f.Write([]byte{0}); err != nil {
			return fmt.Errorf("write padding byte: %w", err)
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

	// Flush to stable storage. NOTE: the write above is in-place and NOT atomic —
	// a crash mid-write can corrupt the source file. Use EditMetadata (full
	// rewrite to a new file) when crash safety matters.
	if s, ok := f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
	}
	return nil
}

type metadataRegion struct {
	start int64
	end   int64
	// segDataStart is the absolute offset of the Segment data (SeekHead
	// positions are relative to it).
	segDataStart int64
	hadSeekHead  bool
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
				region.hadSeekHead = true
				body := make([]byte, eh.Size)
				if _, err := io.ReadFull(f, body); err != nil {
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
// pointing outside the head region are preserved for the rebuilt SeekHead —
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
	if _, err := f.Seek(region.start, io.SeekStart); err != nil {
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
