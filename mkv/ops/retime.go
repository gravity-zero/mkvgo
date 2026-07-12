package ops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"sort"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// retime.go - RetimeTracks cancels a constant A/V desync (the repack defect
// where audio content starts N ms after the video) by patching timecodes in
// place. Matroska block timecodes are RELATIVE to their cluster's Timestamp,
// signed int16 - a range of +-32.7s at the standard 1ms timecode scale,
// while the defect measures in hundreds of ms. Cancelling the delay is
// therefore a 2-byte patch per block of the shifted tracks: no payload byte
// moves, no temp file, no disk duplication. The patches run under the same
// crash-safety journal as ReindexInPlace (an interrupted run is rolled back
// automatically by the next in-place operation, or by RecoverInPlace), and
// Options.RollbackSink turns that journal into a rollback delta of a few KB.

// retimePatch is one byte range to overwrite: orig for the journal, repl to
// apply. Patches cover block timecodes (2 bytes each), cluster CRC-32 values
// whose content changed, and CueTime values of cues keyed on shifted tracks.
type retimePatch struct {
	off        int64
	orig, repl []byte
}

// retimeDispersionFactor models how much slower dispersed 4 KiB page writes
// are than a sequential rewrite: the auto mode patches in place only while
// patchCount x 4KiB x factor stays under the file size, and switches to the
// sequential rewrite beyond (the 10-real-files benchmark that motivated it:
// multi-track movies lose 2-2.5x in place, short single-track wins 3.5x).
// A var so tests can force either path.
var retimeDispersionFactor = int64(5)

// RetimeTracks shifts the block timecodes of the given tracks (track number
// -> shift in nanoseconds, negative = earlier), choosing the cheaper of its
// two engines automatically: the in-place 2-bytes-per-block patch when the
// patches are few relative to the file (a short file, laced audio), the
// sequential rewrite when they are many (multi-track movies, where dispersed
// page writes cost more than rewriting the file once). Force either with
// RetimeTracksInPlace / RetimeTracksReplace; the refusal rules and the
// verification chain are shared.
func RetimeTracks(ctx context.Context, path string, shift map[uint64]int64, opts ...mkv.Options) error {
	err := retimeInPlace(ctx, path, shift, opts, true)
	if errors.Is(err, errRetimeScattered) || errors.Is(err, errRetimeUnknownSize) || errors.Is(err, errRetimeCueUnpatchable) {
		return RetimeTracksReplace(ctx, path, shift, opts...)
	}
	return err
}

// RetimeTracksInPlace shifts the block timecodes of the given tracks in
// place, under the same crash-safe journal as ReindexInPlace: a patch of 2
// bytes per block of the shifted tracks, no payload byte moved, no temp
// file. Cluster CRC-32 elements covering patched blocks are recomputed, and
// CuePoints keyed on shifted tracks move by the same shift. It refuses when
// a shift does not resolve to a whole number of timecode ticks, when any
// resulting relative timecode would leave int16 range or make an absolute
// timestamp negative, when a track is unknown or has no blocks, or when a
// cue mixes shifted and unshifted tracks. Options.DeepVerify re-walks the
// result and checks every shifted track's first block moved by exactly the
// requested shift.
//
// The operation needs write access to the file only. On any failure after
// the journal has landed, the original bytes are restored; a crash
// mid-operation is repaired by the automatic recovery the next in-place run
// performs, or explicitly by RecoverInPlace.
func RetimeTracksInPlace(ctx context.Context, path string, shift map[uint64]int64, opts ...mkv.Options) error {
	return retimeInPlace(ctx, path, shift, opts, false)
}

// The in-place engine's structural refusals, as sentinels so the auto mode
// can route them to the rewrite (which lifts all three): patches too dense,
// a streamed Segment, cues the point-patcher cannot represent.
var (
	errRetimeScattered      = errors.New("retime: patches too scattered for in-place")
	errRetimeUnknownSize    = errors.New("retime: unknown-size (streamed) Segment is not supported in place")
	errRetimeCueUnpatchable = errors.New("retime: the cue set cannot be point-patched in place")
)

// RetimeTracksReplace shifts the block timecodes of the given tracks through
// a sequential rewrite: the reindex engine copies the file cluster by
// cluster, patching the shifted tracks' timecodes (and any cluster CRC-32)
// on the fly, rebuilds the seek index from the SHIFTED blocks - yielding
// healthy, video-keyed cues even when the source's were not - verifies the
// copy (light always; Options.DeepVerify adds the full-read validation diff,
// the payload byte-comparison and a re-walk proving every shifted track
// moved by exactly the requested shift) and only then atomically replaces
// the original. Options.KeepBackup keeps it as path+".bak"; needs write
// permission on the directory.
//
// This is the engine of choice for multi-track movies: patching one block's
// 2 bytes dirties its whole page, so in-place I/O grows past a sequential
// rewrite once patches are dense - and the rewrite also lifts two in-place
// restrictions (unknown-size Segments; cue sets that cannot be
// point-patched, since the index is rebuilt).
func RetimeTracksReplace(ctx context.Context, path string, shift map[uint64]int64, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	if len(shift) == 0 {
		return fmt.Errorf("retime: no tracks to shift")
	}
	meta, err := reader.OpenMetaWithFS(ctx, path, fs)
	if err != nil {
		return fmt.Errorf("retime: read metadata: %w", err)
	}
	shiftTC, _, err := retimeShiftTC(path, meta, shift)
	if err != nil {
		return err
	}

	tmp := path + ".mkvgo.tmp"
	if _, err := fs.DoStat(tmp); err == nil {
		return fmt.Errorf("retime replace: leftover temporary file %s exists; remove it first", tmp)
	}
	fail := func(err error) error {
		_ = fs.DoRemove(tmp) // best-effort cleanup; original is untouched
		return err
	}

	var rb *rollbackBuilder
	if mkv.RollbackSinkFrom(opts) != nil {
		rb = newSpoolingRollbackBuilder(fs, tmp+".rbspool.tmp")
		defer rb.cleanup()
	}

	firstTC := make(map[uint64]int64)
	mutate := func(body []byte) ([]retimePatch, error) {
		ps, merr := retimeCluster(body, 0, shiftTC, firstTC)
		if merr != nil {
			return nil, merr
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].off < ps[j].off })
		for _, p := range ps {
			copy(body[p.off:p.off+int64(len(p.repl))], p.repl)
		}
		return ps, nil
	}

	cues, scale, err := reindexCopy(ctx, path, tmp, fs, mkv.ProgressFrom(opts), rb, mutate)
	if err != nil {
		return fail(err)
	}
	for track := range shiftTC {
		if _, ok := firstTC[track]; !ok {
			return fail(fmt.Errorf("retime: track %d has no blocks", track))
		}
	}

	if err := verifyReindexedCues(ctx, tmp, fs, cues, scale); err != nil {
		return fail(err)
	}
	if mkv.DeepVerifyFrom(opts) {
		before := preValidate(ctx, path, fs)
		if err := deepVerifyValidate(ctx, tmp, fs, before, mkv.StrictVerifyFrom(opts), mkv.OnPreexistingFrom(opts)); err != nil {
			return fail(err)
		}
		// The payload comparison still holds: only timecodes moved, and the
		// block digests hash frame payloads.
		if err := deepVerifyVerbatim(ctx, path, tmp, fs); err != nil {
			return fail(err)
		}
		if err := retimeVerifyShifts(ctx, tmp, fs, shiftTC, firstTC); err != nil {
			return fail(fmt.Errorf("retime deep verify: %w", err))
		}
	}
	if err := emitRollbackEntry(ctx, rb, path, tmp, fs, opts); err != nil {
		return fail(err)
	}
	return installReplacement(fs, tmp, path, mkv.KeepBackupFrom(opts))
}

// retimeVerifyShifts re-walks path and checks every shifted track's first
// block sits at exactly its pre-shift time plus the shift.
func retimeVerifyShifts(ctx context.Context, path string, fs *mkv.FS, shiftTC, before map[uint64]int64) error {
	f, err := fs.DoOpen(path)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := fs.DoStat(path)
	if err != nil {
		return err
	}
	_, after, err := retimeScan(ctx, f, stat.Size(), nil, nil, 0)
	if err != nil {
		return fmt.Errorf("re-walk: %w", err)
	}
	for track, tc := range shiftTC {
		want := before[track] + tc
		if got, ok := after[track]; !ok || got != want {
			return fmt.Errorf("track %d first block at %d ticks, want %d", track, got, want)
		}
	}
	return nil
}

// retimeShiftTC validates the shift map against the file's tracks and
// timecode scale and converts it to ticks.
func retimeShiftTC(path string, meta *mkv.Container, shift map[uint64]int64) (map[uint64]int64, int64, error) {
	scale := meta.Info.TimecodeScale
	if scale <= 0 {
		scale = 1_000_000
	}
	known := make(map[uint64]bool, len(meta.Tracks))
	for _, t := range meta.Tracks {
		known[t.ID] = true
	}
	shiftTC := make(map[uint64]int64, len(shift))
	for track, deltaNs := range shift {
		if !known[track] {
			return nil, 0, fmt.Errorf("retime: track %d does not exist in %s", track, path)
		}
		tc := roundDiv(deltaNs, scale)
		if tc == 0 {
			return nil, 0, fmt.Errorf("retime: shift %dns for track %d is below the file's timecode resolution (%dns per tick)", deltaNs, track, scale)
		}
		if tc*scale != deltaNs {
			return nil, 0, fmt.Errorf("retime: shift %dns for track %d is not a whole number of timecode ticks (%dns per tick)", deltaNs, track, scale)
		}
		shiftTC[track] = tc
	}
	return shiftTC, scale, nil
}

func retimeInPlace(ctx context.Context, path string, shift map[uint64]int64, opts []mkv.Options, bailWhenScattered bool) error {
	fs := mkv.FSFrom(opts)
	if len(shift) == 0 {
		return fmt.Errorf("retime: no tracks to shift")
	}

	f, trunc, sync, size, err := beginInPlacePatch(path, fs, "retime")
	if err != nil {
		return err
	}
	defer f.Close()

	// Head-only metadata: validate the tracks exist and get the timecode
	// scale the shifts must be expressed in.
	meta, err := reader.OpenMetaWithFS(ctx, path, fs)
	if err != nil {
		return fmt.Errorf("retime: read metadata: %w", err)
	}
	shiftTC, _, err := retimeShiftTC(path, meta, shift)
	if err != nil {
		return err
	}

	// In auto mode, bail to the rewrite once in-place stops being the
	// cheaper engine: dispersed 4 KiB page writes past the break-even, or a
	// journal past its cap.
	maxPatches := int64(0)
	if bailWhenScattered {
		byIO := size / (4096 * retimeDispersionFactor)
		byJournal := int64(maxInplaceJournal-64) / 14 // 12-byte zone header + 2-byte payload
		maxPatches = byIO
		if byJournal < maxPatches {
			maxPatches = byJournal
		}
		if maxPatches < 1 {
			maxPatches = 1
		}
	}

	patches, firstTC, err := retimeScan(ctx, f, size, shiftTC, mkv.ProgressFrom(opts), maxPatches)
	if err != nil {
		return err
	}
	for track := range shiftTC {
		if _, ok := firstTC[track]; !ok {
			return fmt.Errorf("retime: track %d has no blocks", track)
		}
	}

	zones := make([]inplaceZone, len(patches))
	for i, p := range patches {
		zones[i] = inplaceZone{off: p.off, orig: p.orig}
	}

	rollback := func(cause error) error {
		if rbErr := rollbackInPlaceJournal(f, &inplaceJournal{origSize: size, zones: zones}); rbErr != nil {
			return fmt.Errorf("retime: %w; rollback ALSO failed (%v), the file is left mid-operation with its journal still attached, run RecoverInPlace", cause, rbErr)
		}
		return fmt.Errorf("retime: %w (original file restored)", cause)
	}

	// The deep-verify diff needs the pre-patch issue set, captured before any
	// byte moves (a full read, but DeepVerify is expensive by contract).
	var beforeIssues []mkv.Issue
	if mkv.DeepVerifyFrom(opts) {
		beforeIssues = preValidate(ctx, path, fs)
	}

	// 1. Land the crash-safety journal past the end of the file.
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		return fmt.Errorf("retime: seek to end of file: %w", err)
	}
	if err := writeInPlaceJournal(f, size, zones); err != nil {
		return rollback(fmt.Errorf("write journal: %w", err))
	}
	if err := sync(); err != nil {
		return rollback(fmt.Errorf("sync after journal: %w", err))
	}

	// 2. Apply the patches (walk order = ascending offsets).
	for _, p := range patches {
		if _, err := f.Seek(p.off, io.SeekStart); err != nil {
			return rollback(fmt.Errorf("seek patch at %d: %w", p.off, err))
		}
		if _, err := f.Write(p.repl); err != nil {
			return rollback(fmt.Errorf("apply patch at %d: %w", p.off, err))
		}
	}
	if err := sync(); err != nil {
		return rollback(fmt.Errorf("sync after patches: %w", err))
	}

	// 3. Always-on verify: every patched range reads back as written.
	rbuf := make([]byte, 8)
	for _, p := range patches {
		if _, err := f.Seek(p.off, io.SeekStart); err != nil {
			return rollback(fmt.Errorf("verify seek at %d: %w", p.off, err))
		}
		got := rbuf[:len(p.repl)]
		if _, err := io.ReadFull(f, got); err != nil {
			return rollback(fmt.Errorf("verify read at %d: %w", p.off, err))
		}
		if !bytes.Equal(got, p.repl) {
			return rollback(fmt.Errorf("verify: patch at %d did not stick", p.off))
		}
	}

	// 4. Optional deep verify: full-read validation, and every shifted
	// track's first block moved by exactly the requested shift.
	if mkv.DeepVerifyFrom(opts) {
		if err := deepVerifyValidate(ctx, path, fs, beforeIssues, mkv.StrictVerifyFrom(opts), mkv.OnPreexistingFrom(opts)); err != nil {
			return rollback(err)
		}
		_, after, err := retimeScan(ctx, f, size, nil, nil, 0)
		if err != nil {
			return rollback(fmt.Errorf("deep verify re-walk: %w", err))
		}
		for track, tc := range shiftTC {
			want := firstTC[track] + tc
			if got, ok := after[track]; !ok || got != want {
				return rollback(fmt.Errorf("deep verify: track %d first block at %d ticks, want %d", track, got, want))
			}
		}
	}

	// 5. Rollback delta, while the journal still allows undoing everything.
	if err := emitInPlaceRollback(ctx, f, size, size, zones, opts); err != nil {
		return rollback(err)
	}

	// 6. Success: drop the journal.
	if err := trunc.Truncate(size); err != nil {
		return fmt.Errorf("retime: truncate away journal: %w", err)
	}
	if err := sync(); err != nil {
		return fmt.Errorf("retime: final sync: %w", err)
	}
	return nil
}

// retimeScan walks the whole file sequentially and computes the patch list
// for the given shifts (per-track, in timecode ticks): block timecodes,
// cluster CRC-32 values to recompute, and CueTimes of cues keyed on shifted
// tracks. It also reports the first (minimum) absolute block timecode seen
// per track. A nil shiftTC scans without producing patches (the deep-verify
// re-walk). Strict: a file the walk cannot parse is refused (repair it with
// reindex first).
func retimeScan(ctx context.Context, f io.ReadSeeker, size int64, shiftTC map[uint64]int64, progress mkv.ProgressFunc, maxPatches int64) (patches []retimePatch, firstTC map[uint64]int64, err error) {
	firstTC = make(map[uint64]int64)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("retime: seek start: %w", err)
	}
	r := bufio.NewReaderSize(f, reindexBufSize)

	ebmlHdr, ebmlHdrBytes, err := ebml.ReadElementHeader(r)
	if err != nil || ebmlHdr.ID != ebml.IDEBMLHeader || ebmlHdr.Size < 0 {
		return nil, nil, fmt.Errorf("retime: not a Matroska file (EBML header: %v)", err)
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		return nil, nil, fmt.Errorf("retime: skip EBML header: %w", err)
	}
	segHdr, segHdrBytes, err := ebml.ReadElementHeader(r)
	if err != nil || segHdr.ID != mkv.IDSegment {
		return nil, nil, fmt.Errorf("retime: expected Segment (%v)", err)
	}
	if segHdr.Size < 0 {
		// The crash-safety journal lands past the end of the file and relies
		// on readers stopping at the declared Segment end; an unknown-size
		// (streamed) Segment would expose it. Same contract as ReindexInPlace.
		return nil, nil, fmt.Errorf("%w; use retime --replace (or the automatic mode)", errRetimeUnknownSize)
	}
	consumed := int64(ebmlHdrBytes) + ebmlHdr.Size + int64(segHdrBytes)
	segEnd := consumed + segHdr.Size

	var buf []byte
	for consumed < segEnd {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		elemStart := consumed
		h, hdrBytes, err := ebml.ReadElementHeader(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return patches, firstTC, nil
			}
			return nil, nil, fmt.Errorf("retime: top-level element at %d: %w (repair the file with reindex first)", elemStart, err)
		}
		if h.Size < 0 {
			return nil, nil, fmt.Errorf("retime: unknown-size element 0x%X at %d is not supported in place (use reindex first)", h.ID, elemStart)
		}
		if h.Size > maxReindexClusterSize {
			return nil, nil, fmt.Errorf("retime: element 0x%X size %d exceeds limit (%d)", h.ID, h.Size, maxReindexClusterSize)
		}

		switch h.ID {
		case mkv.IDCluster, mkv.IDCues:
			if int(h.Size) > len(buf) {
				buf = make([]byte, h.Size)
			}
			body := buf[:h.Size]
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, nil, fmt.Errorf("retime: read 0x%X body: %w", h.ID, err)
			}
			bodyOff := elemStart + int64(hdrBytes)
			var ps []retimePatch
			if h.ID == mkv.IDCluster {
				ps, err = retimeCluster(body, bodyOff, shiftTC, firstTC)
			} else {
				ps, err = retimeCues(body, bodyOff, shiftTC)
			}
			if err != nil {
				return nil, nil, err
			}
			patches = append(patches, ps...)
			if maxPatches > 0 && int64(len(patches)) > maxPatches {
				return nil, nil, errRetimeScattered
			}
		default:
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				return nil, nil, fmt.Errorf("retime: skip 0x%X: %w", h.ID, err)
			}
		}
		consumed = elemStart + int64(hdrBytes) + h.Size
		if progress != nil && size > 0 {
			progress(consumed, size)
		}
	}
	return patches, firstTC, nil
}

// retimeCluster parses one cluster body and returns the patches for its
// blocks on shifted tracks, plus the recomputed CRC-32 when the cluster
// carries one and its content changed. bodyOff is the absolute file offset
// of the body. firstTC collects the minimum absolute block timecode per
// track (in ticks).
func retimeCluster(body []byte, bodyOff int64, shiftTC map[uint64]int64, firstTC map[uint64]int64) ([]retimePatch, error) {
	var patches []retimePatch
	var clusterTS int64
	tsSeen := false
	crcValOff := int64(-1) // in-body offset of the 4 CRC value bytes
	crcElemEnd := int64(0)

	br := bytes.NewReader(body)
	total := int64(len(body))
	pos := func() int64 { return total - int64(br.Len()) }

	patchBlock := func(payloadStart int64, blockSize int64) error {
		sub := bytes.NewReader(body[payloadStart : payloadStart+blockSize])
		track, n, err := ebml.ReadDataSize(sub)
		if err != nil || blockSize < int64(n)+3 {
			return fmt.Errorf("retime: malformed block at %d", bodyOff+payloadStart)
		}
		relOff := payloadStart + int64(n)
		rel := int64(int16(binary.BigEndian.Uint16(body[relOff : relOff+2])))
		if !tsSeen {
			return fmt.Errorf("retime: block before cluster Timestamp at %d (malformed cluster)", bodyOff+payloadStart)
		}
		abs := clusterTS + rel
		if cur, ok := firstTC[uint64(track)]; !ok || abs < cur {
			firstTC[uint64(track)] = abs
		}
		tc, shifted := shiftTC[uint64(track)]
		if !shifted {
			return nil
		}
		newRel := rel + tc
		if newRel < math.MinInt16 || newRel > math.MaxInt16 {
			return fmt.Errorf("retime: shifting track %d block at %d would leave int16 relative-timecode range (%d)", track, bodyOff+payloadStart, newRel)
		}
		if clusterTS+newRel < 0 {
			return fmt.Errorf("retime: shifting track %d block at %d would make its absolute timestamp negative", track, bodyOff+payloadStart)
		}
		orig := append([]byte(nil), body[relOff:relOff+2]...)
		repl := make([]byte, 2)
		binary.BigEndian.PutUint16(repl, uint16(int16(newRel)))
		// Patch the scratch body too, so a CRC recompute sees the new bytes.
		copy(body[relOff:relOff+2], repl)
		patches = append(patches, retimePatch{off: bodyOff + relOff, orig: orig, repl: repl})
		return nil
	}

	first := true
	for br.Len() > 0 {
		chStart := pos()
		ch, n, err := ebml.ReadElementHeader(br)
		if err != nil || ch.Size < 0 || ch.Size > int64(br.Len()) {
			return nil, fmt.Errorf("retime: cluster child at %d does not parse (repair the file with reindex first)", bodyOff+chStart)
		}
		payloadStart := chStart + int64(n)
		switch ch.ID {
		case 0xBF: // CRC-32: only trustworthy as the first child, per spec
			if first && ch.Size == 4 {
				crcValOff = payloadStart
				crcElemEnd = payloadStart + 4
			}
		case mkv.IDTimestamp:
			v, err := ebml.ReadUint(bytes.NewReader(body[payloadStart:payloadStart+ch.Size]), ch.Size)
			if err != nil {
				return nil, fmt.Errorf("retime: cluster Timestamp at %d does not parse", bodyOff+chStart)
			}
			clusterTS = int64(v)
			tsSeen = true
		case mkv.IDSimpleBlock:
			if err := patchBlock(payloadStart, ch.Size); err != nil {
				return nil, err
			}
		case mkv.IDBlockGroup:
			gb := bytes.NewReader(body[payloadStart : payloadStart+ch.Size])
			gTotal := ch.Size
			gPos := func() int64 { return gTotal - int64(gb.Len()) }
			gCRCValOff, gCRCElemEnd := int64(-1), int64(-1)
			gFirst, before := true, len(patches)
			for gb.Len() > 0 {
				gStart := gPos()
				gh, gn, err := ebml.ReadElementHeader(gb)
				if err != nil || gh.Size < 0 || gh.Size > int64(gb.Len()) {
					return nil, fmt.Errorf("retime: BlockGroup child at %d does not parse", bodyOff+payloadStart+gStart)
				}
				switch {
				case gh.ID == 0xBF && gFirst && gh.Size == 4: // the group's own CRC-32
					gCRCValOff = payloadStart + gStart + int64(gn)
					gCRCElemEnd = gCRCValOff + 4
				case gh.ID == mkv.IDBlock:
					if err := patchBlock(payloadStart+gStart+int64(gn), gh.Size); err != nil {
						return nil, err
					}
				}
				if _, err := gb.Seek(gh.Size, io.SeekCurrent); err != nil {
					return nil, err
				}
				gFirst = false
			}
			// A Block under this group moved and the group guards itself with a
			// CRC-32: recompute it before the cluster's own CRC is taken over
			// these same bytes.
			if len(patches) > before && gCRCValOff >= 0 {
				oldCRC := append([]byte(nil), body[gCRCValOff:gCRCValOff+4]...)
				newCRC := make([]byte, 4)
				binary.LittleEndian.PutUint32(newCRC, crc32.ChecksumIEEE(body[gCRCElemEnd:payloadStart+ch.Size]))
				if !bytes.Equal(oldCRC, newCRC) {
					copy(body[gCRCValOff:gCRCValOff+4], newCRC)
					patches = append(patches, retimePatch{off: bodyOff + gCRCValOff, orig: oldCRC, repl: newCRC})
				}
			}
		}
		if _, err := br.Seek(ch.Size, io.SeekCurrent); err != nil {
			return nil, err
		}
		first = false
	}

	// The cluster carries a CRC-32 and its content changed: recompute it over
	// the (patched) bytes after the CRC element, stored little-endian.
	if len(patches) > 0 && crcValOff >= 0 {
		oldCRC := append([]byte(nil), body[crcValOff:crcValOff+4]...)
		newCRC := make([]byte, 4)
		binary.LittleEndian.PutUint32(newCRC, crc32.ChecksumIEEE(body[crcElemEnd:]))
		if !bytes.Equal(oldCRC, newCRC) {
			patches = append(patches, retimePatch{off: bodyOff + crcValOff, orig: oldCRC, repl: newCRC})
		}
	}
	return patches, nil
}

// retimeCues parses a Cues body and shifts the CueTime of every CuePoint
// keyed on shifted tracks (audio-only files cue audio blocks; mixed files
// cue video keyframes, which do not move unless the video track is shifted).
// A cue that references both shifted and unshifted tracks - or two tracks
// with different shifts - cannot be represented and is refused. The new
// value is re-encoded into the CueTime element's existing length (leading
// zero bytes are legal EBML); a value that no longer fits is refused.
func retimeCues(body []byte, bodyOff int64, shiftTC map[uint64]int64) ([]retimePatch, error) {
	var patches []retimePatch
	br := bytes.NewReader(body)
	total := int64(len(body))
	pos := func() int64 { return total - int64(br.Len()) }
	crcValOff, crcElemEnd := int64(-1), int64(-1)
	first := true

	for br.Len() > 0 {
		cpStart := pos()
		cp, n, err := ebml.ReadElementHeader(br)
		if err != nil || cp.Size < 0 || cp.Size > int64(br.Len()) {
			return nil, fmt.Errorf("retime: Cues child at %d does not parse", bodyOff+cpStart)
		}
		if cp.ID != mkv.IDCuePoint {
			// CRC-32: only trustworthy as the first child, per spec. The
			// CueTimes below are patched underneath it, so it must be recomputed.
			if cp.ID == 0xBF && first && cp.Size == 4 {
				crcValOff = cpStart + int64(n)
				crcElemEnd = crcValOff + 4
			}
			first = false
			if _, err := br.Seek(cp.Size, io.SeekCurrent); err != nil {
				return nil, err
			}
			continue
		}
		first = false
		cpBody := body[cpStart+int64(n) : cpStart+int64(n)+cp.Size]
		cueTimeOff, cueTimeLen := int64(-1), int64(0)
		var cueTime int64
		var refTracks []uint64

		cb := bytes.NewReader(cpBody)
		cTotal := int64(len(cpBody))
		cPos := func() int64 { return cTotal - int64(cb.Len()) }
		for cb.Len() > 0 {
			eStart := cPos()
			eh, en, err := ebml.ReadElementHeader(cb)
			if err != nil || eh.Size < 0 || eh.Size > int64(cb.Len()) {
				return nil, fmt.Errorf("retime: CuePoint child at %d does not parse", bodyOff+cpStart+eStart)
			}
			switch eh.ID {
			case mkv.IDCueTime:
				v, err := ebml.ReadUint(bytes.NewReader(cpBody[eStart+int64(en):eStart+int64(en)+eh.Size]), eh.Size)
				if err != nil {
					return nil, fmt.Errorf("retime: CueTime at %d does not parse", bodyOff+cpStart+eStart)
				}
				cueTime = int64(v)
				cueTimeOff = cpStart + int64(n) + eStart + int64(en)
				cueTimeLen = eh.Size
			case mkv.IDCueTrackPositions:
				tpBody := cpBody[eStart+int64(en) : eStart+int64(en)+eh.Size]
				tb := bytes.NewReader(tpBody)
				for tb.Len() > 0 {
					th, tn, err := ebml.ReadElementHeader(tb)
					if err != nil || th.Size < 0 || th.Size > int64(tb.Len()) {
						break
					}
					if th.ID == mkv.IDCueTrack && tn > 0 {
						tPos := int64(len(tpBody)) - int64(tb.Len()) // value starts here
						v, err := ebml.ReadUint(bytes.NewReader(tpBody[tPos:tPos+th.Size]), th.Size)
						if err == nil {
							refTracks = append(refTracks, v)
						}
					}
					if _, err := tb.Seek(th.Size, io.SeekCurrent); err != nil {
						break
					}
				}
			}
			if _, err := cb.Seek(eh.Size, io.SeekCurrent); err != nil {
				return nil, err
			}
		}

		shifted, unshifted := 0, 0
		var delta int64
		for _, tr := range refTracks {
			if tc, ok := shiftTC[tr]; ok {
				if shifted > 0 && tc != delta {
					return nil, fmt.Errorf("%w: cue at %d references tracks with different shifts", errRetimeCueUnpatchable, bodyOff+cpStart)
				}
				delta = tc
				shifted++
			} else {
				unshifted++
			}
		}
		if shifted == 0 {
			if _, err := br.Seek(cp.Size, io.SeekCurrent); err != nil {
				return nil, err
			}
			continue
		}
		if unshifted > 0 {
			return nil, fmt.Errorf("%w: cue at %d references both shifted and unshifted tracks (the rewrite regenerates the index)", errRetimeCueUnpatchable, bodyOff+cpStart)
		}
		if cueTimeOff < 0 {
			return nil, fmt.Errorf("retime: cue at %d has no CueTime", bodyOff+cpStart)
		}
		newTime := cueTime + delta
		if newTime < 0 {
			return nil, fmt.Errorf("%w: cue at %d would get a negative time", errRetimeCueUnpatchable, bodyOff+cpStart)
		}
		if cueTimeLen < 8 && newTime >= int64(1)<<(8*cueTimeLen) {
			return nil, fmt.Errorf("%w: cue at %d: shifted time does not fit the existing CueTime encoding (the rewrite regenerates the index)", errRetimeCueUnpatchable, bodyOff+cpStart)
		}
		orig := append([]byte(nil), body[cueTimeOff:cueTimeOff+cueTimeLen]...)
		repl := make([]byte, cueTimeLen)
		v := uint64(newTime)
		for i := cueTimeLen - 1; i >= 0; i-- {
			repl[i] = byte(v)
			v >>= 8
		}
		// Patch the scratch body too, so a CRC recompute sees the new bytes.
		copy(body[cueTimeOff:cueTimeOff+cueTimeLen], repl)
		patches = append(patches, retimePatch{off: bodyOff + cueTimeOff, orig: orig, repl: repl})

		if _, err := br.Seek(cp.Size, io.SeekCurrent); err != nil {
			return nil, err
		}
	}

	// The Cues element carries a CRC-32 and its CueTimes changed: recompute it
	// over the (patched) bytes after the CRC element, stored little-endian -
	// exactly as retimeCluster does for a cluster. Leaving it stale would make
	// every CRC-checking reader reject the index the patch just corrected.
	if len(patches) > 0 && crcValOff >= 0 {
		oldCRC := append([]byte(nil), body[crcValOff:crcValOff+4]...)
		newCRC := make([]byte, 4)
		binary.LittleEndian.PutUint32(newCRC, crc32.ChecksumIEEE(body[crcElemEnd:]))
		if !bytes.Equal(oldCRC, newCRC) {
			patches = append(patches, retimePatch{off: bodyOff + crcValOff, orig: oldCRC, repl: newCRC})
		}
	}
	return patches, nil
}

// roundDiv divides a by b rounding half away from zero.
func roundDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	if a >= 0 {
		return (a + b/2) / b
	}
	return (a - b/2) / b
}
