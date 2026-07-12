package mp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/gravity-zero/mkvgo/mkv"
)

// editshift.go - RetimeTracks for MP4/MOV: cancel a constant A/V desync by
// shifting tracks' PRESENTATION through the edit list (edts/elst), the
// container's native mechanism for exactly this. Where the Matroska engine
// patches two bytes per block, MP4 needs no block touched at all: the empty
// edit at the head of a track's edit list IS its presentation delay, so the
// repair is a moov-only rewrite - a few bytes, no sample walk, whatever the
// file size. The signature mirrors the Matroska op (track number -> shift in
// nanoseconds, negative = earlier), so a caller repairs either container
// through the same shape.

// RetimeTracks shifts the presentation of the given tracks (track number ->
// shift in nanoseconds, negative = earlier) by editing each track's edit
// list in the moov - the MP4 counterpart of the Matroska RetimeTracks. The
// samples and their decode times never move: an empty edit is grown, shrunk
// or created, and the track/movie durations follow. Track numbers are the
// 1-based positions the probe reports.
//
// The write is moov-only and needs write permission on the file alone:
//   - the rewritten moov has the same size -> patched in place;
//   - the moov is the last top-level box -> the tail is rewritten;
//   - otherwise (moov at the head, faststart) the new moov is appended and
//     the old one is renamed to a free box - 4 bytes patched, crash-ordered
//     (the new moov is synced to disk before the flip, so an interrupted run
//     leaves the original semantics intact).
//
// Explicit refusals: a shift that would present a track before the
// presentation start (MP4 cannot - that would trim media content), zero or
// unknown tracks, and fragmented MP4 (players do not apply edit lists to
// fragment timelines consistently).
func RetimeTracks(ctx context.Context, path string, shift map[uint64]int64, opts ...Options) error {
	o := optionsFrom(opts)
	if len(shift) == 0 {
		return errf("retime: no track shifts given")
	}
	for track, ns := range shift {
		if ns == 0 {
			return errf("retime: track %d: a zero shift does nothing", track)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f, err := o.FS.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return errf("retime: %w", err)
	}
	defer f.Close()

	lay, err := scanTopLevelLayout(f)
	if err != nil {
		return errf("retime: %s: %w", path, err)
	}
	moovRaw, err := readRange(f, lay.moovOff, lay.moovSize)
	if err != nil {
		return errf("retime: read moov: %w", err)
	}
	newMoov, err := rewriteMoovEditShifts(moovRaw[lay.moovHdr:], shift)
	if err != nil {
		return errf("retime: %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeMoovInPlace(f, lay, newMoov)
}

// topLayout locates the moov among the top-level boxes.
type topLayout struct {
	moovOff  int64 // absolute offset of the moov box header
	moovSize int64 // full box size, header included
	moovHdr  int64 // header length (8, or 16 for the largesize form)
	fileSize int64
}

// scanTopLevelLayout walks the top-level boxes and returns the moov's
// location. Only well-formed layouts qualify: an edit-list repair must not
// guess on a file whose box walk desyncs (repair or remux that first).
func scanTopLevelLayout(f mkv.ReadWriteSeekCloser) (*topLayout, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	lay := &topLayout{fileSize: size, moovOff: -1}
	var off int64
	hdr := make([]byte, 16)
	for off+8 <= size {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(f, hdr[:8]); err != nil {
			return nil, fmt.Errorf("box header at %d: %w", off, err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hdrLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(f, hdr[8:16]); err != nil {
				return nil, fmt.Errorf("box largesize at %d: %w", off, err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hdrLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < hdrLen || off+boxSize > size {
			return nil, fmt.Errorf("box %q at %d has invalid size %d", typ, off, boxSize)
		}
		if typ == "moov" {
			lay.moovOff, lay.moovSize, lay.moovHdr = off, boxSize, hdrLen
		}
		off += boxSize
	}
	if lay.moovOff < 0 {
		return nil, fmt.Errorf("no moov box found")
	}
	return lay, nil
}

func readRange(f io.ReadSeeker, off, n int64) ([]byte, error) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(f, buf)
	return buf, err
}

// elstEntry is one raw edit-list entry; rate is the 16.16 media_rate verbatim.
type elstEntry struct {
	segDur    int64
	mediaTime int64
	rate      uint32
}

// rewriteMoovEditShifts rebuilds the moov payload with each shifted track's
// empty edit adjusted (and the tkhd/mvhd durations following), returning the
// complete new moov box bytes.
func rewriteMoovEditShifts(payload []byte, shift map[uint64]int64) ([]byte, error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, err
	}
	if _, fragmented := findMemBox(boxes, "mvex"); fragmented {
		return nil, fmt.Errorf("fragmented MP4: fragment timelines do not honour edit lists consistently; repack first")
	}
	mvhd, ok := findMemBox(boxes, "mvhd")
	if !ok {
		return nil, fmt.Errorf("moov without mvhd")
	}
	movieTS, _ := parseMovieHeader(mvhd.payload)
	if movieTS == 0 {
		return nil, fmt.Errorf("mvhd declares no timescale")
	}

	pending := make(map[uint64]int64, len(shift))
	for track, ns := range shift {
		pending[track] = ns
	}
	var trakNum uint64
	children := make([][]byte, 0, len(boxes))
	trakDurs := make([]int64, 0, 4) // updated tkhd durations, for the mvhd recompute
	mvhdIdx := -1
	for _, b := range boxes {
		if b.typ != "trak" {
			if b.typ == "mvhd" {
				mvhdIdx = len(children)
			}
			children = append(children, box(b.typ, b.payload))
			continue
		}
		trakNum++
		ns, wants := pending[trakNum]
		if !wants {
			children = append(children, box(b.typ, b.payload))
			if d, err := trakDuration(b.payload); err == nil {
				trakDurs = append(trakDurs, d)
			}
			continue
		}
		delete(pending, trakNum)
		newTrak, newDur, err := rewriteTrakEditShift(b.payload, trakNum, ns, movieTS)
		if err != nil {
			return nil, err
		}
		children = append(children, newTrak)
		trakDurs = append(trakDurs, newDur)
	}
	if len(pending) > 0 {
		for track := range pending {
			return nil, fmt.Errorf("track %d not found (the file has %d)", track, trakNum)
		}
	}

	// The movie duration is the longest track's presentation.
	var maxDur int64
	for _, d := range trakDurs {
		if d > maxDur {
			maxDur = d
		}
	}
	patched, err := patchMvhdDuration(mvhd.payload, maxDur)
	if err != nil {
		return nil, err
	}
	children[mvhdIdx] = box("mvhd", patched)

	var w bw
	for _, c := range children {
		w.bytes(c)
	}
	return box("moov", w.b), nil
}

// rewriteTrakEditShift adjusts one trak's presentation delay by ns and its
// tkhd duration by the same amount, returning the rebuilt trak box and the
// new duration (movie timescale).
func rewriteTrakEditShift(payload []byte, track uint64, ns int64, movieTS uint32) ([]byte, int64, error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, 0, err
	}
	tkhd, ok := findMemBox(boxes, "tkhd")
	if !ok {
		return nil, 0, fmt.Errorf("track %d: trak without tkhd", track)
	}
	oldDur, err := trakDuration(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("track %d: %w", track, err)
	}

	entries, err := trakElstEntries(boxes)
	if err != nil {
		return nil, 0, fmt.Errorf("track %d: %w", track, err)
	}
	var empty int64
	kept := make([]elstEntry, 0, len(entries))
	for _, e := range entries {
		if e.mediaTime < 0 {
			empty += e.segDur // collapse every empty edit into one
			continue
		}
		kept = append(kept, e)
	}
	shiftTicks := roundTicks(ns, movieTS)
	newEmpty := empty + shiftTicks
	if newEmpty < 0 {
		return nil, 0, fmt.Errorf("track %d: cannot start %dms before the presentation start (only %dms of delay to remove; MP4 cannot present media earlier than 0 without trimming it)",
			track, ticksToMs(-newEmpty, movieTS), ticksToMs(empty, movieTS))
	}
	if len(kept) == 0 {
		// No media edit yet (typical for a file without an elst): the whole
		// media, from its start, at rate 1.0. Its duration is the track's
		// current presentation minus the delay already accounted for.
		mediaDur := oldDur - empty
		if mediaDur < 0 {
			mediaDur = 0
		}
		kept = append(kept, elstEntry{segDur: mediaDur, mediaTime: 0, rate: 1 << 16})
	}
	final := kept
	if newEmpty > 0 {
		final = append([]elstEntry{{segDur: newEmpty, mediaTime: -1, rate: 1 << 16}}, kept...)
	}

	newDur := oldDur + (newEmpty - empty)
	if newDur < 0 {
		newDur = 0
	}
	patchedTkhd, err := patchTkhdDuration(tkhd.payload, newDur)
	if err != nil {
		return nil, 0, fmt.Errorf("track %d: %w", track, err)
	}

	// Reassemble: tkhd patched, edts rebuilt right after it (its spec slot),
	// any previous edts dropped, everything else verbatim.
	children := make([][]byte, 0, len(boxes)+1)
	for _, b := range boxes {
		switch b.typ {
		case "edts":
			continue
		case "tkhd":
			children = append(children, box("tkhd", patchedTkhd), buildElstBox(final))
		default:
			children = append(children, box(b.typ, b.payload))
		}
	}
	var w bw
	for _, c := range children {
		w.bytes(c)
	}
	return box("trak", w.b), newDur, nil
}

// trakElstEntries returns the raw edit-list entries of a trak's children
// (nil when it has no edts/elst).
func trakElstEntries(trakBoxes []memBox) ([]elstEntry, error) {
	edts, ok := findMemBox(trakBoxes, "edts")
	if !ok {
		return nil, nil
	}
	eb, err := iterBoxes(edts.payload)
	if err != nil {
		return nil, err
	}
	elst, ok := findMemBox(eb, "elst")
	if !ok {
		return nil, nil
	}
	p := elst.payload
	if len(p) < 8 {
		return nil, fmt.Errorf("elst too short")
	}
	version := p[0]
	count := binary.BigEndian.Uint32(p[4:8])
	entries := make([]elstEntry, 0, count)
	off := 8
	for i := uint32(0); i < count; i++ {
		var e elstEntry
		if version == 1 {
			if off+20 > len(p) {
				return nil, fmt.Errorf("elst v1 truncated")
			}
			e.segDur = int64(binary.BigEndian.Uint64(p[off : off+8]))
			e.mediaTime = int64(binary.BigEndian.Uint64(p[off+8 : off+16]))
			e.rate = binary.BigEndian.Uint32(p[off+16 : off+20])
			off += 20
		} else {
			if off+12 > len(p) {
				return nil, fmt.Errorf("elst truncated")
			}
			e.segDur = int64(binary.BigEndian.Uint32(p[off : off+4]))
			e.mediaTime = int64(int32(binary.BigEndian.Uint32(p[off+4 : off+8])))
			e.rate = binary.BigEndian.Uint32(p[off+8 : off+12])
			off += 12
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// buildElstBox frames entries as edts/elst, choosing v1 only when a value
// does not fit the 32-bit form.
func buildElstBox(entries []elstEntry) []byte {
	version := uint8(0)
	for _, e := range entries {
		if e.segDur > 0xFFFFFFFF || e.mediaTime > 0x7FFFFFFF {
			version = 1
			break
		}
	}
	elst := fullBox("elst", version, 0, func(w *bw) {
		w.u32(uint32(len(entries)))
		for _, e := range entries {
			if version == 1 {
				w.u64(uint64(e.segDur))
				w.u64(uint64(e.mediaTime))
			} else {
				w.u32(uint32(e.segDur))
				w.u32(uint32(e.mediaTime))
			}
			w.u32(e.rate)
		}
	})
	return container("edts", elst)
}

// trakDuration reads a trak's tkhd duration (movie timescale).
func trakDuration(trakPayload []byte) (int64, error) {
	boxes, err := iterBoxes(trakPayload)
	if err != nil {
		return 0, err
	}
	tkhd, ok := findMemBox(boxes, "tkhd")
	if !ok {
		return 0, fmt.Errorf("trak without tkhd")
	}
	_, dur, err := tkhdDurationField(tkhd.payload)
	return dur, err
}

// tkhdDurationField locates the duration inside a tkhd payload: its byte
// offset and current value. v0 stores u32 at 20, v1 u64 at 28 (after the
// fullbox header, creation/modification times and track_ID/reserved).
func tkhdDurationField(p []byte) (off int, dur int64, err error) {
	if len(p) < 4 {
		return 0, 0, fmt.Errorf("tkhd too short")
	}
	if p[0] == 1 {
		off = 4 + 8 + 8 + 4 + 4
		if len(p) < off+8 {
			return 0, 0, fmt.Errorf("tkhd v1 too short")
		}
		return off, int64(binary.BigEndian.Uint64(p[off : off+8])), nil
	}
	off = 4 + 4 + 4 + 4 + 4
	if len(p) < off+4 {
		return 0, 0, fmt.Errorf("tkhd too short")
	}
	return off, int64(binary.BigEndian.Uint32(p[off : off+4])), nil
}

func patchTkhdDuration(p []byte, dur int64) ([]byte, error) {
	off, _, err := tkhdDurationField(p)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), p...)
	if p[0] == 1 {
		binary.BigEndian.PutUint64(out[off:off+8], uint64(dur))
	} else {
		if dur > 0xFFFFFFFF {
			dur = 0xFFFFFFFF
		}
		binary.BigEndian.PutUint32(out[off:off+4], uint32(dur))
	}
	return out, nil
}

// patchMvhdDuration writes the movie duration (v0 u32 at 16, v1 u64 at 24).
func patchMvhdDuration(p []byte, dur int64) ([]byte, error) {
	if len(p) < 4 {
		return nil, fmt.Errorf("mvhd too short")
	}
	out := append([]byte(nil), p...)
	if p[0] == 1 {
		off := 4 + 8 + 8 + 4
		if len(p) < off+8 {
			return nil, fmt.Errorf("mvhd v1 too short")
		}
		binary.BigEndian.PutUint64(out[off:off+8], uint64(dur))
		return out, nil
	}
	off := 4 + 4 + 4 + 4
	if len(p) < off+4 {
		return nil, fmt.Errorf("mvhd too short")
	}
	if dur > 0xFFFFFFFF {
		dur = 0xFFFFFFFF
	}
	binary.BigEndian.PutUint32(out[off:off+4], uint32(dur))
	return out, nil
}

// roundTicks converts nanoseconds to timescale ticks, rounding half away
// from zero.
func roundTicks(ns int64, ts uint32) int64 {
	prod := ns * int64(ts)
	if prod >= 0 {
		return (prod + 500_000_000) / 1_000_000_000
	}
	return -((-prod + 500_000_000) / 1_000_000_000)
}

// writeMoovInPlace lands the rewritten moov with file-only permission:
// same-size overwrite, tail rewrite, or append-and-retire (the old moov
// becomes a free box AFTER the new one is on disk, so a crash at any point
// leaves a readable file - the first surviving moov wins).
func writeMoovInPlace(f mkv.ReadWriteSeekCloser, lay *topLayout, newMoov []byte) error {
	syncer, canSync := f.(interface{ Sync() error })
	switch {
	case int64(len(newMoov)) == lay.moovSize:
		if _, err := f.Seek(lay.moovOff, io.SeekStart); err != nil {
			return err
		}
		if _, err := f.Write(newMoov); err != nil {
			return errf("retime: patch moov: %w", err)
		}
	case lay.moovOff+lay.moovSize == lay.fileSize:
		// The moov is the tail: rewrite it there.
		truncer, ok := f.(interface{ Truncate(int64) error })
		if !ok {
			return errf("retime: the file handle does not support Truncate (required to rewrite the trailing moov)")
		}
		if _, err := f.Seek(lay.moovOff, io.SeekStart); err != nil {
			return err
		}
		if _, err := f.Write(newMoov); err != nil {
			return errf("retime: write moov: %w", err)
		}
		if err := truncer.Truncate(lay.moovOff + int64(len(newMoov))); err != nil {
			return errf("retime: truncate: %w", err)
		}
	default:
		// moov at the head (faststart): append the new moov, sync it to disk,
		// THEN retire the old one to a free box - the crash-safe order.
		if !canSync {
			return errf("retime: the file handle does not support Sync (required for the crash-safe moov swap)")
		}
		if _, err := f.Seek(lay.fileSize, io.SeekStart); err != nil {
			return err
		}
		if _, err := f.Write(newMoov); err != nil {
			return errf("retime: append moov: %w", err)
		}
		if err := syncer.Sync(); err != nil {
			return errf("retime: sync appended moov: %w", err)
		}
		if _, err := f.Seek(lay.moovOff+4, io.SeekStart); err != nil {
			return err
		}
		if _, err := f.Write([]byte("free")); err != nil {
			return errf("retime: retire old moov: %w", err)
		}
	}
	if canSync {
		if err := syncer.Sync(); err != nil {
			return errf("retime: sync: %w", err)
		}
	}
	return nil
}
