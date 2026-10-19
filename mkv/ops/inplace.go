package ops

import (
	"bytes"
	"context"
	"encoding/json"
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
// entries to elements outside the head, e.g. the Cues). Tags, Chapters or
// Attachments sitting after the clusters (mux statistics tags, chapters some
// muxers write there) stay there when the edit leaves them as they are; edited, they are
// folded into the head metadata and the old bytes voided, or, Tags and
// Chapters, rewritten in the old element's place when the head cannot hold them.
func EditInPlace(ctx context.Context, path string, edit func(*mkv.Container), opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return err
	}

	before, err := metaForms(c)
	if err != nil {
		return err
	}
	applyEdit(c, edit)
	syncDurationMs(c)

	region, err := findMetadataRegion(path, fs)
	if err != nil {
		return fmt.Errorf("scan metadata: %w", err)
	}
	if err := regionWithinMedia(path, fs, c, region); err != nil {
		return err
	}
	after, err := metaForms(c)
	if err != nil {
		return err
	}
	for _, id := range tailIDs {
		if region.tailUnknown && !region.inHead[id] && len(before[id]) > 0 {
			return fmt.Errorf("the %s sit after the clusters, which no SeekHead indexes and which cannot be stepped through: %w", tailNames[id], ErrNoRoomInPlace)
		}
	}
	// raw: the head elements the edit left as they were, copied as the file has them rather than written again.
	raw, err := unchangedHead(path, fs, region, before, after)
	if err != nil {
		return err
	}
	// stay: the tail elements the edit left as they were, and whose part in the head, if any, is copied as it was.
	stay := map[uint32]bool{}
	for id := range region.tail {
		stay[id] = bytes.Equal(before[id], after[id]) && (!region.inHead[id] || raw[id] != nil)
	}
	kept := region.keptSeekEntries
	lay := func() ([]byte, []writer.SeekEntry, int64, []byte, error) {
		region.keptSeekEntries = kept
		for id, t := range region.tail {
			if stay[id] {
				region.keptSeekEntries = append(region.keptSeekEntries, t.entry)
			}
		}
		metaBytes, entries, err := serializeHead(c, stay, raw)
		if err != nil {
			return nil, nil, 0, nil, err
		}
		reserve, seekData, err := layoutRegion(region, &metaBytes, &entries, region.end-region.start)
		return metaBytes, entries, reserve, seekData, err
	}
	metaBytes, _, reserve, seekData, err := lay()
	rewrite := map[uint32]bool{} // edited tail elements the head cannot hold, back in their old place
	if err != nil {
		for id, t := range region.tail {
			if slack := t.size - int64(len(after[id])); !stay[id] && !region.inHead[id] && id != mkv.IDAttachments && len(after[id]) > 0 && (slack == 0 || slack >= 2) {
				rewrite[id], stay[id] = true, true
			}
		}
		if len(rewrite) > 0 {
			metaBytes, _, reserve, seekData, err = lay()
		}
	}
	if err != nil {
		return err
	}
	available := region.end - region.start
	newSize := int64(len(metaBytes))

	f, err := fs.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	// The region is overwritten from its first byte, so a Close that fails to
	// flush leaves the file's head half-rewritten. Reporting nil there would
	// say the edit landed. This is about the write being SEEN, not about crash
	// safety: the Sync below stays best-effort, as documented.
	defer closeWithErr(f, &err)

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

	// Tail elements folded into the head are voided where they were, so they are not read twice; those the head
	// could not hold are rewritten there.
	for id, t := range region.tail {
		if stay[id] && !rewrite[id] {
			continue
		}
		if _, err := f.Seek(t.pos, io.SeekStart); err != nil {
			return err
		}
		voided := t.size
		if rewrite[id] {
			if _, err := f.Write(after[id]); err != nil {
				return err
			}
			voided -= int64(len(after[id]))
		}
		if voided > 0 {
			if err := writer.WriteVoid(f, int(voided)); err != nil {
				return err
			}
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

// tailIDs are the metadata elements a muxer may leave after the clusters.
var tailIDs = []uint32{mkv.IDTags, mkv.IDChapters, mkv.IDAttachments}

var tailNames = map[uint32]string{mkv.IDTags: "tags", mkv.IDChapters: "chapters", mkv.IDAttachments: "attachments"}

// metaForms serialises what each metadata element would hold, to tell whether an edit changed it (attachments by
// their description: their payloads stay where they are).
func metaForms(c *mkv.Container) (map[uint32][]byte, error) {
	var info, tracks, tags, chapters bytes.Buffer
	if err := writer.WriteSegmentInfo(&info, &c.Info, c.DurationMs); err != nil {
		return nil, err
	}
	if err := writer.WriteTracks(&tracks, c.Tracks); err != nil {
		return nil, err
	}
	if err := writer.WriteTags(&tags, c.Tags); err != nil {
		return nil, err
	}
	if len(c.Chapters) > 0 || len(c.OtherEditions) > 0 {
		if err := writer.WriteEditions(&chapters, c.Chapters, c.OtherEditions); err != nil {
			return nil, err
		}
	}
	attachments, err := json.Marshal(c.Attachments)
	if err != nil {
		return nil, err
	}
	if len(c.Attachments) == 0 {
		attachments = nil
	}
	return map[uint32][]byte{mkv.IDInfo: info.Bytes(), mkv.IDTracks: tracks.Bytes(), mkv.IDTags: tags.Bytes(),
		mkv.IDChapters: chapters.Bytes(), mkv.IDAttachments: attachments}, nil
}

// unchangedHead reads the head elements whose form the edit left as it was (one of a kind in the head), as the file
// holds them.
func unchangedHead(path string, fs *mkv.FS, region metadataRegion, before, after map[uint32][]byte) (map[uint32][]byte, error) {
	raw := map[uint32][]byte{}
	var f mkv.ReadSeekCloser
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for id, h := range region.headElems {
		if region.headTwice[id] || !bytes.Equal(before[id], after[id]) {
			continue
		}
		if f == nil {
			var err error
			if f, err = fs.DoOpen(path); err != nil {
				return nil, err
			}
		}
		b := make([]byte, h.size)
		if _, err := f.Seek(h.pos, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(f, b); err != nil {
			return nil, err
		}
		raw[id] = b
	}
	return raw, nil
}

// serializeHead serialises the head metadata elements, recording each one's offset for the rebuilt SeekHead: those in
// raw as the file had them, those in skip left after the clusters, the others written from c.
func serializeHead(c *mkv.Container, skip map[uint32]bool, raw map[uint32][]byte) ([]byte, []writer.SeekEntry, error) {
	var newMeta bytes.Buffer
	var entries []writer.SeekEntry
	put := func(id uint32, present bool, write func() error) error {
		switch {
		case raw[id] != nil:
			entries = append(entries, writer.SeekEntry{ID: id, Pos: int64(newMeta.Len())})
			newMeta.Write(raw[id])
		case present && !skip[id]:
			entries = append(entries, writer.SeekEntry{ID: id, Pos: int64(newMeta.Len())})
			return write()
		}
		return nil
	}
	err := put(mkv.IDInfo, true, func() error { return writer.WriteSegmentInfo(&newMeta, &c.Info, c.DurationMs) })
	if err == nil {
		err = put(mkv.IDTracks, len(c.Tracks) > 0, func() error { return writer.WriteTracks(&newMeta, c.Tracks) })
	}
	if err == nil {
		err = put(mkv.IDChapters, len(c.Chapters) > 0 || len(c.OtherEditions) > 0, func() error {
			return writer.WriteEditions(&newMeta, c.Chapters, c.OtherEditions)
		})
	}
	if err == nil {
		err = put(mkv.IDAttachments, len(c.Attachments) > 0, func() error { return writer.WriteAttachments(&newMeta, c.Attachments) })
	}
	if err == nil {
		err = put(mkv.IDTags, len(c.Tags) > 0, func() error { return writer.WriteTags(&newMeta, c.Tags) })
	}
	if err != nil {
		return nil, nil, err
	}
	return newMeta.Bytes(), entries, nil
}

// layoutRegion lays the metadata out in the region (see fitRegion), widening it a byte when one would be left over.
func layoutRegion(region metadataRegion, meta *[]byte, entries *[]writer.SeekEntry, available int64) (int64, []byte, error) {
	// A SeekHead slot (when the file had one) then the metadata, the leftover voided. The old SeekHead sits at the
	// region start, so ANY write here destroys it: rebuilding it is mandatory, or the Cues past the clusters would
	// only be findable by a full walk. A single byte left over, which no EBML element can fill, is absorbed into the
	// metadata by re-encoding the first element's data size a byte wider, as EBML allows.
	reserve, seekData, err := fitRegion(region, *entries, *meta, available)
	if errors.Is(err, errOddByte) {
		var perr error
		if *meta, *entries, perr = widenFirstDataSize(*meta, *entries); perr != nil {
			return 0, nil, fmt.Errorf("new metadata (%d bytes) leaves exactly 1 byte of the %d available, which no EBML element can fill: %w", len(*meta), available, perr)
		}
		reserve, seekData, err = fitRegion(region, *entries, *meta, available)
	}
	return reserve, seekData, err
}

// ErrNoRoomInPlace is what EditInPlace returns (errors.Is) when the edited metadata does not fit where the file
// keeps it, before anything is written: EditMetadata rewrites the file instead.
var ErrNoRoomInPlace = errors.New("use full rewrite instead")

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
		return 0, nil, fmt.Errorf("new metadata (%d bytes) exceeds available space (%d bytes) - %w", newSize, available, ErrNoRoomInPlace)
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
	return 0, nil, fmt.Errorf("new metadata (%d bytes) plus the rebuilt SeekHead exceeds available space (%d bytes) - %w", newSize, available, ErrNoRoomInPlace)
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
	// tail holds the Tags, Chapters and Attachments elements after the clusters, by ID, with the old SeekHead
	// entry kept when they stay where they are.
	tail map[uint32]tailElement
	// inHead holds the metadata element IDs the head region holds; headElems where each sits, headTwice those met
	// twice (copied as they were only when one of a kind).
	inHead    map[uint32]bool
	headElems map[uint32]tailElement
	headTwice map[uint32]bool
	// tailUnknown is set when a file with no SeekHead could not be stepped through to its end.
	tailUnknown bool
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
	region := metadataRegion{start: -1, tail: map[uint32]tailElement{}, inHead: map[uint32]bool{}, headElems: map[uint32]tailElement{}, headTwice: map[uint32]bool{}}
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
		eh, hdrLen, err := ebml.ReadElementHeader(f)
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
			region.end = pos + int64(hdrLen) + eh.Size // the size field as wide as the file has it
			region.inHead[eh.ID] = true
			if eh.ID != mkv.IDSeekHead && eh.ID != mkv.IDVoid {
				if _, twice := region.headElems[eh.ID]; twice {
					region.headTwice[eh.ID] = true
				}
				region.headElems[eh.ID] = tailElement{pos: pos, size: int64(hdrLen) + eh.Size}
			}
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
				return region, nil
			}
			return region, scanTail(f, pos, segEnd, &region)
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

// regionWithinMedia refuses a head whose declared sizes run past the file or over the first cluster the Cues name:
// the edit would fill that span with its Void, zeroing the media.
func regionWithinMedia(path string, fs *mkv.FS, c *mkv.Container, region metadataRegion) error {
	st, err := fs.DoStat(path)
	if err != nil {
		return err
	}
	end := st.Size()
	for _, cp := range c.Cues {
		if cp.ClusterPos > 0 {
			end = min(end, region.segDataStart+cp.ClusterPos)
		}
	}
	if region.end > end {
		return fmt.Errorf("the metadata before the clusters declares sizes reaching byte %d, past the media at byte %d: repair the file with mkvgo reindex, then edit it", region.end, end)
	}
	return nil
}

// tailElement is a metadata element after the clusters: where it starts, its full size, its SeekHead entry.
type tailElement struct {
	pos, size int64
	entry     writer.SeekEntry
}

// isTailID reports a metadata element a muxer may leave after the clusters.
func isTailID(id uint32) bool {
	return id == mkv.IDTags || id == mkv.IDChapters || id == mkv.IDAttachments
}

// scanTail finds, in a file with no SeekHead to point at them, the tail elements after the clusters by stepping
// over the top-level elements; a cluster of unknown size, or a second element of one kind, leaves the tail unknown.
func scanTail(f mkv.ReadSeekCloser, pos, segEnd int64, region *metadataRegion) error {
	for segEnd < 0 || pos < segEnd {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		eh, hdrLen, err := ebml.ReadElementHeader(f)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil || eh.Size < 0 {
			region.tailUnknown = true
			return nil
		}
		if isTailID(eh.ID) {
			if _, twice := region.tail[eh.ID]; twice {
				region.tailUnknown = true // more than one: not one element to keep or rewrite
				return nil
			}
			region.tail[eh.ID] = tailElement{pos: pos, size: int64(hdrLen) + eh.Size}
		}
		pos += int64(hdrLen) + eh.Size
	}
	return nil
}

// classifyTailEntries re-reads the old SeekHead and sorts its entries: those
// pointing outside the head region are preserved for the rebuilt SeekHead  -
// except a post-cluster Tags, Chapters or Attachments element, recorded with its
// location and full size, which EditInPlace keeps, folds or rewrites.
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
		if isTailID(e.ID) {
			if _, err := f.Seek(abs, io.SeekStart); err != nil {
				return err
			}
			th, hdrLen, err := ebml.ReadElementHeader(f)
			if err != nil || th.ID != e.ID || th.Size < 0 {
				continue // stale entry: leave it out, void nothing
			}
			if _, twice := region.tail[e.ID]; twice {
				region.tailUnknown = true
				continue
			}
			region.tail[e.ID] = tailElement{pos: abs, size: int64(hdrLen) + th.Size, entry: e}
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
	if err != nil || sh.ID != mkv.IDSeekHead || sh.Size < 0 || sh.Size > maxSeekHeadBytes {
		return nil, nil
	}
	body := make([]byte, sh.Size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, nil
	}
	return parseSeekHeadBody(body), nil
}

// maxSeekHeadBytes bounds the SeekHead an in-place edit reads whole: a real one is a few dozen entries; the size a
// file declares is not believed past it.
const maxSeekHeadBytes = 1 << 20

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
		if eh.Size > int64(br.Len()) {
			return out
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
