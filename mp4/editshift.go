package mp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/gravity-zero/mkvgo/internal/shown"
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
// The write is moov-only, needs write permission on the file alone, and is
// crash-ordered whatever the layout: the new moov is appended at the end of
// the file and synced to disk FIRST, then the old moov is retired to a free
// box - a single 4-byte type flip. At every instant the file carries one
// intact, authoritative moov: before the flip the original one, after it the
// new one; an interrupted run keeps the original semantics. The retired moov
// stays behind as a free box (a repair is rare and a moov is small - the
// growth is bounded and harmless); note that a faststart source is no longer
// faststart afterwards, since the live moov now sits at the tail - re-run
// `to-mp4 --faststart` if progressive HTTP serving needs it back.
//
// Explicit refusals: a shift that would present a track before the
// presentation start (MP4 cannot - that would trim media content), zero or
// unknown tracks, and fragmented MP4 (players do not apply edit lists to
// fragment timelines consistently).
//
// There is deliberately no KeepBackup option (unlike the Matroska engine's
// mkv.Options.KeepBackup): copying a whole file to guard a moov-only patch
// would contradict the constant-cost promise. The safety model is the
// crash-ordered write itself - every landing path leaves a readable file at
// every instant - which is atomicity, NOT a restorable backup: after a
// successful run there is no built-in undo. A caller whose workflow demands
// a restorable copy makes one before calling.
func RetimeTracks(ctx context.Context, path string, shift map[uint64]int64, opts ...Options) error {
	o := optionsFrom(opts)
	if len(shift) == 0 {
		return errf("retime: no track shifts given")
	}
	// In track order, here and below: a map walk would name a different track
	// from one call to the next when several are at fault.
	for _, track := range slices.Sorted(maps.Keys(shift)) {
		if shift[track] == 0 {
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
		return errf("retime: %s: %w", shown.Path(path), err)
	}
	moovRaw, err := readRange(f, lay.moovOff, lay.moovSize)
	if err != nil {
		return errf("retime: read moov: %w", err)
	}
	traks, names, err := shiftsByTrak(moovRaw[lay.moovHdr:], lay.fileSize, shift)
	if err != nil {
		return errf("retime: %s: %w", shown.Path(path), err)
	}
	newMoov, err := rewriteMoovEditShiftsNamed(moovRaw[lay.moovHdr:], traks, names)
	if err != nil {
		return errf("retime: %s: %w", shown.Path(path), err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeMoovInPlace(f, lay, newMoov)
}

// topLayout locates the moov among the top-level boxes.
type topLayout struct {
	// moovOff is the FIRST moov: the one a forward box walk reads, hence the
	// one that is authoritative for players (and for mp4.Open).
	moovOff  int64 // absolute offset of the moov box header
	moovSize int64 // full box size, header included
	moovHdr  int64 // header length (8, or 16 for the largesize form)
	// strayMoovs are any FURTHER moov boxes. An interrupted repair leaves its
	// appended moov behind, so they exist in the wild; they are retired with
	// the live one, or the next forward walk lands on a stale movie header.
	strayMoovs []int64
	fileSize   int64
	mdats      [][3]int64 // each top-level mdat: box offset, payload start, end
}

// scanTopLevelLayout walks the top-level boxes and returns the moov's
// location. Only well-formed layouts qualify: an edit-list repair must not
// guess on a file whose box walk desyncs (repair or remux that first).
//
// Two layouts are refused outright because the repair lands its new moov by
// APPENDING at the end of the file, which they would silently swallow:
//   - a last box declaring size 0 (it runs to the end of the file, so it would
//     simply grow over the appended moov, turning it into payload);
//   - a tail of junk bytes past the last box (the appended moov would sit
//     behind it, and a strict forward walk desyncs on the junk).
//
// Both are shapes mp4.Diagnose already reports, with "remux" as the remedy.
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
			return nil, fmt.Errorf("box %q at %d declares size 0 (it runs to the end of the file): "+
				"appending a moov would extend it - remux the file first", typ, off)
		}
		if boxSize < hdrLen || off+boxSize > size {
			return nil, fmt.Errorf("box %q at %d has invalid size %d: rewrite the file first (to-mp4 in.mp4 out.mp4) and edit the copy", typ, off, boxSize)
		}
		if typ == "mdat" {
			lay.mdats = append(lay.mdats, [3]int64{off, off + hdrLen, off + boxSize})
		}
		if typ == "moov" {
			if lay.moovOff < 0 {
				lay.moovOff, lay.moovSize, lay.moovHdr = off, boxSize, hdrLen
			} else {
				lay.strayMoovs = append(lay.strayMoovs, off)
			}
		}
		off += boxSize
	}
	if off != size {
		return nil, fmt.Errorf("%d byte(s) of junk beyond the last box: rewrite the file first (to-mp4 in.mp4 out.mp4) and edit the copy", size-off)
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
	// readExact, not make+ReadFull: n is a size the FILE declared, and the
	// "offset + box size <= file size" guard that vetted it is only as
	// trustworthy as the reported size - which over httpfs is a remote
	// server's Content-Length.
	return readExact(f, n)
}

// elstEntry is one raw edit-list entry; rate is the 16.16 media_rate verbatim.
type elstEntry struct {
	segDur    int64
	mediaTime int64
	rate      uint32
}

// shiftsByTrak rekeys shift from probe track IDs to 1-based trak positions (a trak the probe drops does not count), with the way back.
func shiftsByTrak(payload []byte, size int64, shift map[uint64]int64) (map[uint64]int64, map[uint64]uint64, error) {
	mv, err := parseMoov(payload, size, sampleNone)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[uint64]int64, len(shift))
	names := make(map[uint64]uint64, len(shift))
	for _, track := range slices.Sorted(maps.Keys(shift)) {
		if track == 0 || track > uint64(len(mv.tracks)) {
			return nil, nil, fmt.Errorf("track %d not found (the file has %d)", track, len(mv.tracks))
		}
		pos := uint64(mv.tracks[track-1].trakIndex) + 1
		out[pos], names[pos] = shift[track], track
	}
	return out, names, nil
}

// rewriteMoovEditShifts rebuilds the moov payload with each shifted track's
// empty edit adjusted (and the tkhd/mvhd durations following), returning the
// complete new moov box bytes.
func rewriteMoovEditShifts(payload []byte, shift map[uint64]int64) ([]byte, error) {
	return rewriteMoovEditShiftsNamed(payload, shift, nil)
}

// rewriteMoovEditShiftsNamed is rewriteMoovEditShifts naming each trak by names[position] in its refusals.
func rewriteMoovEditShiftsNamed(payload []byte, shift map[uint64]int64, names map[uint64]uint64) ([]byte, error) {
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
		name := trakNum
		if n, ok := names[trakNum]; ok {
			name = n
		}
		newTrak, newDur, err := rewriteTrakEditShift(b.payload, name, ns, movieTS)
		if err != nil {
			return nil, err
		}
		children = append(children, newTrak)
		trakDurs = append(trakDurs, newDur)
	}
	if len(pending) > 0 {
		track := slices.Min(slices.Collect(maps.Keys(pending)))
		return nil, fmt.Errorf("track %d not found (the file has %d)", track, trakNum)
	}

	// The movie duration is the longest track's presentation - recomputed
	// only when every trak's duration was readable, so a foreign trak whose
	// tkhd does not parse can never shrink the movie duration by absence.
	if int64(len(trakDurs)) == int64(trakNum) {
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
	}

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
		if e.mediaTime < 0 && len(kept) == 0 {
			empty += e.segDur // the empty edits ahead of the media are the delay a shift moves; a pause after it stays
			continue
		}
		kept = append(kept, e)
	}
	shiftTicks := roundTicks(ns, movieTS)
	newEmpty := empty + shiftTicks
	if newEmpty < 0 {
		return nil, 0, fmt.Errorf("track %d: cannot start %dms before the presentation start (only %dms of delay to remove; MP4 cannot present media earlier than 0 without trimming it): delay the other tracks by %dms instead",
			track, ticksToMs(-newEmpty, movieTS), ticksToMs(empty, movieTS), ticksToMs(-newEmpty, movieTS))
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
	width := uint64(12)
	if version == 1 {
		width = 20
	}
	if uint64(count)*width > uint64(len(p)-8) {
		return nil, fmt.Errorf("elst declares %d entries but holds %d bytes", count, len(p)-8)
	}
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
	if len(p) >= 24 && p[0] == 0 && dur > 0xFFFFFFFF {
		p = widenTimes(p, 8)
	}
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
	if len(p) >= 20 && p[0] == 0 && dur > 0xFFFFFFFF {
		p = widenTimes(p, 4)
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

// patchMdhdDuration writes a media duration: mdhd shares mvhd's leading layout.
func patchMdhdDuration(p []byte, dur int64) ([]byte, error) { return patchMvhdDuration(p, dur) }

// widenTimes turns a v0 mvhd/mdhd/tkhd payload into v1 (64-bit times and duration); mid is the width kept between them.
func widenTimes(p []byte, mid int) []byte {
	var w bw
	w.u8(1)
	w.bytes(p[1:4])
	w.u64(uint64(binary.BigEndian.Uint32(p[4:])))
	w.u64(uint64(binary.BigEndian.Uint32(p[8:])))
	w.bytes(p[12 : 12+mid])
	w.u64(uint64(binary.BigEndian.Uint32(p[12+mid:])))
	w.bytes(p[16+mid:])
	return w.b
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

// writeMoovInPlace lands the rewritten moov with the one write order that is
// crash-safe on every layout: append the new moov at the end of the file,
// sync it to disk, THEN retire the old moov to a free box with a single
// 4-byte type flip (and sync again). Until the flip the original moov is the
// file's only authoritative one; after it the appended moov is - so a crash
// at any instant leaves a readable file with exactly one live moov. No
// in-place overwrite of the only moov ever happens (a torn write there would
// destroy the file), which is why the same-size and moov-at-tail layouts
// take this path too, at the cost of one retired free block.
func writeMoovInPlace(f mkv.ReadWriteSeekCloser, lay *topLayout, newMoov []byte) error {
	return appendAndRetire(f, lay, "retime", newMoov)
}

// appendAndRetire appends blobs (the new moov last) at the end of the file, syncs, then retires the old moov(s).
func appendAndRetire(f mkv.ReadWriteSeekCloser, lay *topLayout, op string, blobs ...[]byte) error {
	syncer, ok := f.(interface{ Sync() error })
	if !ok {
		return errf("%s: the file handle does not support Sync (required for the crash-safe moov swap)", op)
	}
	// Another writer may have grown the file since its layout was read: appending at the old end would overwrite it.
	if end, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	} else if end != lay.fileSize {
		return errf("%s: the file changed while it was being edited (%d bytes, %d before): edit it again once nothing else writes to it", op, end, lay.fileSize)
	}
	// Until the flip the original moov is authoritative: a failed append is cut off again, so the file stays editable.
	undo := func(err error) error {
		if t, ok := f.(interface{ Truncate(int64) error }); ok {
			_ = t.Truncate(lay.fileSize)
		}
		return err
	}
	for _, b := range blobs {
		if _, err := f.Write(b); err != nil {
			return undo(errf("%s: append moov: %w", op, err))
		}
	}
	if err := syncer.Sync(); err != nil {
		return undo(errf("%s: sync appended moov: %w", op, err))
	}
	// Retire any stray moov (left behind by an interrupted run) BEFORE the live
	// one. A forward walk keeps reading the live head moov - the original
	// semantics - throughout; flipping the head first would instead expose a
	// stale stray moov for the length of the window.
	for _, off := range lay.strayMoovs {
		if err := retireMoov(f, off, op); err != nil {
			return err
		}
	}
	if len(lay.strayMoovs) > 0 {
		if err := syncer.Sync(); err != nil {
			return errf("%s: sync retired stray moov: %w", op, err)
		}
	}
	// The live moov goes last: this single flip is the instant the appended
	// moov becomes the file's authoritative one.
	if err := retireMoov(f, lay.moovOff, op); err != nil {
		return err
	}
	if err := syncer.Sync(); err != nil {
		return errf("%s: sync: %w", op, err)
	}
	return nil
}

// retireMoov turns a moov box into a free box with a single 4-byte type flip.
// The type sits at offset+4 in both header forms (the 64-bit form keeps size=1
// at 0-3, the type at 4-7, the largesize after).
func retireMoov(f mkv.ReadWriteSeekCloser, off int64, op string) error {
	if _, err := f.Seek(off+4, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write([]byte("free")); err != nil {
		return errf("%s: retire moov at %d: %w", op, off, err)
	}
	return nil
}
