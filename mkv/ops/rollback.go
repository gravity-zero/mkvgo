package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"sort"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// rollback.go - inverse repair delta. A reindex copies cluster payloads
// verbatim, so the recipe to rebuild the ORIGINAL from the REPAIRED file is
// tiny: "copy this range of the repaired output" for ~99.99% of the bytes,
// plus literals for what the repair dropped or rewrote (old index elements,
// element headers re-encoded by the writer, damaged ranges). The delta is
// emitted DURING the rewrite - the writer already knows the src->dst mapping
// of every run it copies - so it costs no extra diff pass; only the repaired
// file's hash needs one sequential re-read after Finalize.
//
// Entry format (framed, append-friendly, all integers big-endian):
//
//	magic "MKVGRB1\0" | u32 flags | sha256 original | sha256 repaired |
//	u64 original size | u32 nOps | ops... | crc32c(everything before)
//	op := 0x01 COPY  { u64 dstOff, u64 len }  // range of the REPAIRED file
//	    | 0x02 LIT   { u64 len, bytes... }    // bytes only the original had
//	    | 0x03 TRUNC { u64 len }              // in-place: appended tail to cut
//
// Ops follow the original sequentially (COPY/LIT lengths sum to the original
// size), so reconstruction is a single streaming pass. The original's sha256
// is computed BY CONSTRUCTION while emitting: each op's content is hashed in
// op order, which is original order.

var rollbackMagic = [8]byte{'M', 'K', 'V', 'G', 'R', 'B', '1', 0}

const (
	rollbackOpCopy  = 0x01
	rollbackOpLit   = 0x02
	rollbackOpTrunc = 0x03
)

// rollbackMaxBuffer caps the buffered ops region: a source so damaged that
// its literals exceed this is out of the delta's design envelope (the point
// is a tiny recipe; callers keep the full-copy fallback). A var so tests can
// shrink it.
var rollbackMaxBuffer int64 = 256 << 20 // 256 MiB

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// errRollbackTooBig marks a delta abandoned for exceeding rollbackMaxBuffer.
var errRollbackTooBig = fmt.Errorf("rollback delta exceeds the %d-byte buffer cap", rollbackMaxBuffer)

// rollbackBuilder accumulates the ops of one delta entry during a repair
// walk. Errors are sticky: after the first failure every method is a no-op
// and finalize reports it (the caller decides best-effort vs required).
//
// The ops region lives in RAM until rollbackSpillThreshold, then - when the
// builder was given a spool location - overflows to a temporary file, so a
// long multi-track movie's delta (hundreds of MB of 2-byte patches, each
// paying its op framing) never busts memory. A builder without a spool (the
// in-place operations, whose contract is file-only permission and whose
// deltas are bounded by the crash journal's cap anyway) keeps the RAM cap.
type rollbackBuilder struct {
	ops     bytes.Buffer
	nOps    uint32
	srcHash hash.Hash // sha256 of the original, fed op by op in original order
	tiled   int64     // original bytes covered so far
	err     error

	fs        *mkv.FS
	spoolPath string                  // "" = RAM only (capped)
	spool     mkv.ReadWriteSeekCloser // non-nil once spilled
	opsLen    int64                   // total ops bytes, RAM + spool
}

// rollbackSpillThreshold is where a spooling builder moves its ops region
// from RAM to the spool file. A var so tests can force the spill path.
var rollbackSpillThreshold int64 = 8 << 20 // 8 MiB

func newRollbackBuilder() *rollbackBuilder {
	return &rollbackBuilder{srcHash: sha256.New()}
}

// newSpoolingRollbackBuilder is newRollbackBuilder plus a spool location the
// ops overflow to instead of hitting the RAM cap. The caller must arrange
// cleanup() (idempotent; finalize runs it too) so an abandoned build does
// not leave the spool file behind.
func newSpoolingRollbackBuilder(fs *mkv.FS, spoolPath string) *rollbackBuilder {
	return &rollbackBuilder{srcHash: sha256.New(), fs: fs, spoolPath: spoolPath}
}

func (b *rollbackBuilder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// cleanup releases the spool file, if one was created. Safe to call twice.
func (b *rollbackBuilder) cleanup() {
	if b == nil || b.spool == nil {
		return
	}
	b.spool.Close() //nolint:errcheck
	_ = b.fs.DoRemove(b.spoolPath)
	b.spool = nil
}

// writeOps appends raw op bytes, spilling from RAM to the spool past the
// threshold. Failures are sticky and swallowed (the callers' own streams
// must not be disturbed); finalize reports them.
func (b *rollbackBuilder) writeOps(p []byte) (int, error) {
	if b.err != nil {
		return len(p), nil
	}
	if b.spool == nil && b.spoolPath != "" && int64(b.ops.Len())+int64(len(p)) > rollbackSpillThreshold {
		f, err := b.fs.DoOpenFile(b.spoolPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			b.fail(fmt.Errorf("rollback: create spool: %w", err))
			return len(p), nil
		}
		b.spool = f
		if _, err := b.spool.Write(b.ops.Bytes()); err != nil {
			b.fail(fmt.Errorf("rollback: spill to spool: %w", err))
			return len(p), nil
		}
		b.ops.Reset()
	}
	if b.spool != nil {
		if _, err := b.spool.Write(p); err != nil {
			b.fail(fmt.Errorf("rollback: write spool: %w", err))
			return len(p), nil
		}
	} else {
		b.ops.Write(p)
	}
	b.opsLen += int64(len(p))
	return len(p), nil
}

func (b *rollbackBuilder) overCap(add int64) bool {
	if b.spoolPath != "" {
		return false // spooling builders are disk-bound, not RAM-capped
	}
	if b.opsLen+add > rollbackMaxBuffer {
		b.fail(errRollbackTooBig)
		return true
	}
	return false
}

// copyRun records that the original bytes `data` live verbatim in the
// repaired output at dstOff.
func (b *rollbackBuilder) copyRun(dstOff int64, data []byte) {
	if b.err != nil || b.overCap(17) {
		return
	}
	var hdr [17]byte
	hdr[0] = rollbackOpCopy
	binary.BigEndian.PutUint64(hdr[1:], uint64(dstOff))
	binary.BigEndian.PutUint64(hdr[9:], uint64(len(data)))
	b.writeOps(hdr[:]) //nolint:errcheck // sticky-error semantics
	b.srcHash.Write(data)
	b.tiled += int64(len(data))
	b.nOps++
}

// copyRunStreamed is copyRun for bytes not held in memory: the caller streams
// the run's content through the returned writer (a tee alongside its own
// copy) so the original hash still sees every byte.
func (b *rollbackBuilder) copyRunStreamed(dstOff, n int64) io.Writer {
	if b.err != nil || b.overCap(17) {
		return io.Discard
	}
	var hdr [17]byte
	hdr[0] = rollbackOpCopy
	binary.BigEndian.PutUint64(hdr[1:], uint64(dstOff))
	binary.BigEndian.PutUint64(hdr[9:], uint64(n))
	b.writeOps(hdr[:]) //nolint:errcheck // sticky-error semantics
	b.tiled += n
	b.nOps++
	return b.srcHash
}

// literal records original bytes that the repaired output does not carry.
func (b *rollbackBuilder) literal(data []byte) {
	if b.err != nil || b.overCap(9+int64(len(data))) {
		return
	}
	var hdr [9]byte
	hdr[0] = rollbackOpLit
	binary.BigEndian.PutUint64(hdr[1:], uint64(len(data)))
	b.writeOps(hdr[:]) //nolint:errcheck // sticky-error semantics
	b.writeOps(data)   //nolint:errcheck
	b.srcHash.Write(data)
	b.tiled += int64(len(data))
	b.nOps++
}

// literalFrom reads n original bytes from r at offset off (restoring nothing:
// the caller owns the stream position) and records them as a literal. Used
// for ranges the walk skipped or dropped.
func (b *rollbackBuilder) literalFrom(r io.ReadSeeker, off, n int64) {
	if b.err != nil || b.overCap(9+n) {
		return
	}
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		b.fail(fmt.Errorf("rollback: seek literal source: %w", err))
		return
	}
	var hdr [9]byte
	hdr[0] = rollbackOpLit
	binary.BigEndian.PutUint64(hdr[1:], uint64(n))
	b.writeOps(hdr[:]) //nolint:errcheck // sticky-error semantics
	if _, err := io.CopyN(io.MultiWriter(opsWriter{b}, b.srcHash), r, n); err != nil {
		b.fail(fmt.Errorf("rollback: read literal source: %w", err))
		return
	}
	b.tiled += n
	b.nOps++
}

// literalWriter reserves a LIT op of exactly n bytes and returns the writer
// the caller must stream them into, alongside its own consumption of the
// source. On a sticky error it returns io.Discard so the caller's stream
// handling is unaffected.
func (b *rollbackBuilder) literalWriter(n int64) io.Writer {
	if b.err != nil || b.overCap(9+n) {
		return io.Discard
	}
	var hdr [9]byte
	hdr[0] = rollbackOpLit
	binary.BigEndian.PutUint64(hdr[1:], uint64(n))
	b.writeOps(hdr[:]) //nolint:errcheck // sticky-error semantics
	b.tiled += n
	b.nOps++
	return io.MultiWriter(opsWriter{b}, b.srcHash)
}

// opsWriter adapts writeOps to io.Writer for the streaming literal paths.
type opsWriter struct{ b *rollbackBuilder }

func (w opsWriter) Write(p []byte) (int, error) { return w.b.writeOps(p) }

// literalHeader reconstructs the exact source bytes of an element header from
// its parsed form - the ID's canonical encoding plus the size VINT at the
// width the source used (an EBML header of a given width is a unique
// encoding) - and records them as a literal. This is how re-encoded headers
// (the writer may pick a different VINT width) and lying size fields round-trip.
func (b *rollbackBuilder) literalHeader(h ebml.ElementHeader, hdrBytes int) {
	if b.err != nil {
		return
	}
	idBytes := encodeUintBE(uint64(h.ID))
	sizeWidth := hdrBytes - len(idBytes)
	raw, err := vintEncode(h.Size, sizeWidth)
	if err != nil {
		b.fail(fmt.Errorf("rollback: synthesize header 0x%X: %w", h.ID, err))
		return
	}
	b.literal(append(idBytes, raw...))
}

// trunc records that the repaired file carries n appended tail bytes that are
// not part of the original (the in-place path's new Cues + SeekHead tail).
func (b *rollbackBuilder) trunc(n int64) {
	if b.err != nil || b.overCap(9) {
		return
	}
	var hdr [9]byte
	hdr[0] = rollbackOpTrunc
	binary.BigEndian.PutUint64(hdr[1:], uint64(n))
	b.writeOps(hdr[:]) //nolint:errcheck // sticky-error semantics
	b.nOps++
}

// finalize checks the ops tile the original exactly, frames the entry and
// writes it to sink in one sequence. Returns the entry size and the two
// hashes for the caller's reporting.
func (b *rollbackBuilder) finalize(sink io.Writer, originalSize int64, repairedSHA [32]byte) (mkv.RollbackInfo, error) {
	if b.err != nil {
		return mkv.RollbackInfo{}, b.err
	}
	if b.tiled != originalSize {
		return mkv.RollbackInfo{}, fmt.Errorf("rollback: ops cover %d bytes, original is %d - refusing to emit an incomplete delta", b.tiled, originalSize)
	}
	var srcSHA [32]byte
	b.srcHash.Sum(srcSHA[:0])

	header := make([]byte, 0, 88)
	header = append(header, rollbackMagic[:]...)
	header = binary.BigEndian.AppendUint32(header, 0) // flags, reserved
	header = append(header, srcSHA[:]...)
	header = append(header, repairedSHA[:]...)
	header = binary.BigEndian.AppendUint64(header, uint64(originalSize))
	header = binary.BigEndian.AppendUint32(header, b.nOps)

	crc := crc32.New(crc32cTable)
	crc.Write(header)
	if _, err := sink.Write(header); err != nil {
		return mkv.RollbackInfo{}, fmt.Errorf("rollback: write entry header: %w", err)
	}
	// Stream the ops region - from RAM, or back from the spool file a large
	// delta overflowed to - through the crc and out to the sink in one pass.
	var opsSrc io.Reader = bytes.NewReader(b.ops.Bytes())
	if b.spool != nil {
		if _, err := b.spool.Seek(0, io.SeekStart); err != nil {
			return mkv.RollbackInfo{}, fmt.Errorf("rollback: rewind spool: %w", err)
		}
		opsSrc = io.LimitReader(b.spool, b.opsLen)
	}
	if _, err := io.Copy(io.MultiWriter(sink, crc), opsSrc); err != nil {
		return mkv.RollbackInfo{}, fmt.Errorf("rollback: write entry ops: %w", err)
	}
	b.cleanup()
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc.Sum32())
	if _, err := sink.Write(crcBuf[:]); err != nil {
		return mkv.RollbackInfo{}, fmt.Errorf("rollback: write entry crc: %w", err)
	}
	return mkv.RollbackInfo{
		Bytes:     int64(len(header)) + b.opsLen + 4,
		SrcSHA256: srcSHA,
		DstSHA256: repairedSHA,
	}, nil
}

// emitRollbackEntry closes out a repair's delta: it hashes the repaired file
// (the one extra sequential read a delta costs), finalises the builder
// against the source size and writes the framed entry to the sink. A nil
// builder is a no-op. Failures honor Options.RollbackRequired: best-effort
// by default (the repair stands, no entry, no OnRollback), fatal when the
// caller demanded the delta.
func emitRollbackEntry(ctx context.Context, rb *rollbackBuilder, srcPath, dstPath string, fs *mkv.FS, opts []mkv.Options) error {
	if rb == nil {
		return nil
	}
	required := mkv.RollbackRequiredFrom(opts)
	fail := func(err error) error {
		if required {
			return fmt.Errorf("rollback delta (RollbackRequired): %w", err)
		}
		return nil
	}
	stat, err := fs.DoStat(srcPath)
	if err != nil {
		return fail(err)
	}
	dstSHA, err := hashFileSHA256(ctx, dstPath, fs)
	if err != nil {
		return fail(err)
	}
	info, err := rb.finalize(mkv.RollbackSinkFrom(opts), stat.Size(), dstSHA)
	if err != nil {
		return fail(err)
	}
	if onRollback := mkv.OnRollbackFrom(opts); onRollback != nil {
		onRollback(info)
	}
	return nil
}

// emitInPlaceRollback builds and writes the delta entry for an in-place
// reindex whose patches have been applied and verified but whose journal has
// not been truncated yet: COPY ops for the unpatched spans (offsets are
// identity - the file IS the original outside the patch zones), literals for
// the zones' journaled original bytes, and a TRUNC for the appended Cues
// tail. One sequential read of [0, finalSize) computes both hashes (the
// final file is that prefix; the truncate only cuts the journal). Failures
// honor Options.RollbackRequired, and since the journal is still alive the
// caller can roll the whole repair back on a required failure.
func emitInPlaceRollback(ctx context.Context, f io.ReadSeeker, origSize, finalSize int64, zones []inplaceZone, opts []mkv.Options) error {
	sink := mkv.RollbackSinkFrom(opts)
	if sink == nil {
		return nil
	}
	required := mkv.RollbackRequiredFrom(opts)
	fail := func(err error) error {
		if required {
			return fmt.Errorf("rollback delta (RollbackRequired): %w", err)
		}
		return nil
	}

	sorted := append([]inplaceZone(nil), zones...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].off < sorted[j].off })

	rb := newRollbackBuilder()
	dstHash := sha256.New()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}

	// Stream [0, finalSize) once: every byte feeds the repaired hash; bytes
	// outside the zones also feed the COPY runs' original hash, zone bytes
	// are replaced by the journaled originals in the delta.
	stream := func(n int64, alsoTo io.Writer) error {
		dst := io.Writer(dstHash)
		if alsoTo != nil {
			dst = io.MultiWriter(dstHash, alsoTo)
		}
		_, err := io.CopyN(dst, f, n)
		return err
	}
	cursor := int64(0)
	for _, z := range sorted {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		zLen := int64(len(z.orig))
		if z.off < cursor || z.off+zLen > origSize {
			return fail(fmt.Errorf("rollback: journal zone [%d,%d) out of order or range", z.off, z.off+zLen))
		}
		if z.off > cursor {
			if err := stream(z.off-cursor, rb.copyRunStreamed(cursor, z.off-cursor)); err != nil {
				return fail(err)
			}
		}
		rb.literal(z.orig)
		if err := stream(zLen, nil); err != nil { // patched bytes: repaired hash only
			return fail(err)
		}
		cursor = z.off + zLen
	}
	if cursor < origSize {
		if err := stream(origSize-cursor, rb.copyRunStreamed(cursor, origSize-cursor)); err != nil {
			return fail(err)
		}
	}
	if finalSize > origSize {
		rb.trunc(finalSize - origSize)
		if err := stream(finalSize-origSize, nil); err != nil { // appended tail
			return fail(err)
		}
	}

	var dstSHA [32]byte
	dstHash.Sum(dstSHA[:0])
	info, err := rb.finalize(sink, origSize, dstSHA)
	if err != nil {
		return fail(err)
	}
	if onRollback := mkv.OnRollbackFrom(opts); onRollback != nil {
		onRollback(info)
	}
	return nil
}

// vintEncode encodes v as an EBML data-size VINT of exactly `width` bytes
// (the encoding a given width produces is unique, so this reproduces the
// source bytes even when the value would fit a narrower VINT).
func vintEncode(v int64, width int) ([]byte, error) {
	if width < 1 || width > 8 {
		return nil, fmt.Errorf("VINT width %d out of range", width)
	}
	out := make([]byte, width)
	marker := byte(0x80 >> (width - 1))
	if v < 0 {
		// Unknown size: all value bits set (a streamed Segment header).
		out[0] = marker | (marker - 1)
		for i := 1; i < width; i++ {
			out[i] = 0xFF
		}
		return out, nil
	}
	max := int64(1)<<(7*width) - 2 // all-ones is the reserved "unknown"
	if v > max {
		return nil, fmt.Errorf("value %d does not fit a %d-byte VINT", v, width)
	}
	for i := width - 1; i > 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	out[0] = byte(v) | marker
	return out, nil
}

// hashFileSHA256 reads path sequentially through the FS port and returns its
// sha256 - the one extra pass a delta-emitting repair costs.
func hashFileSHA256(ctx context.Context, path string, fs *mkv.FS) ([32]byte, error) {
	var sum [32]byte
	f, err := fs.DoOpen(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return sum, rerr
		}
	}
	h.Sum(sum[:0])
	return sum, nil
}

// ApplyRollback reconstructs the pre-repair original into dstPath from the
// repaired file and one delta entry. It refuses to write anything real when
// sha256(repaired) does not match the entry (the repaired file changed since
// the repair was taken), and deletes dstPath rather than return success when
// the entry's crc or the reconstructed sha256 does not check out.
func ApplyRollback(ctx context.Context, repairedPath string, delta io.Reader, dstPath string, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)

	// ── Entry header ────────────────────────────────────────────────────
	header := make([]byte, 88)
	if _, err := io.ReadFull(delta, header); err != nil {
		return fmt.Errorf("rollback: read entry header: %w", err)
	}
	if !bytes.Equal(header[:8], rollbackMagic[:]) {
		return fmt.Errorf("rollback: not a rollback delta entry (bad magic)")
	}
	var wantSrc, wantDst [32]byte
	copy(wantSrc[:], header[12:44])
	copy(wantDst[:], header[44:76])
	originalSize := int64(binary.BigEndian.Uint64(header[76:84]))
	nOps := binary.BigEndian.Uint32(header[84:88])
	if originalSize < 0 {
		return fmt.Errorf("rollback: negative original size")
	}

	// ── Gate 1: the repaired file must be exactly the one the delta was
	// taken against. ─────────────────────────────────────────────────────
	repairedSHA, err := hashFileSHA256(ctx, repairedPath, fs)
	if err != nil {
		return fmt.Errorf("rollback: hash repaired file: %w", err)
	}
	if repairedSHA != wantDst {
		return fmt.Errorf("rollback: %s does not match the delta entry (the repaired file changed since the repair); refusing to reconstruct", repairedPath)
	}

	repaired, err := fs.DoOpen(repairedPath)
	if err != nil {
		return fmt.Errorf("rollback: open repaired: %w", err)
	}
	defer repaired.Close()
	repairedSize := int64(-1)
	if st, serr := fs.DoStat(repairedPath); serr == nil {
		repairedSize = st.Size()
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return fmt.Errorf("rollback: create output: %w", err)
	}
	// On any failure below, do not leave a half-reconstructed file around.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = fs.DoRemove(dstPath)
		}
	}()

	// ── Stream the ops: write the reconstruction while running the crc and
	// the output hash. ───────────────────────────────────────────────────
	crc := crc32.New(crc32cTable)
	crc.Write(header)
	outHash := sha256.New()
	sink := io.MultiWriter(out, outHash)
	written := int64(0)
	copyBuf := make([]byte, 1<<20)

	for i := uint32(0); i < nOps; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var tag [1]byte
		if _, err := io.ReadFull(delta, tag[:]); err != nil {
			return fmt.Errorf("rollback: op %d: truncated delta: %w", i, err)
		}
		crc.Write(tag[:])
		switch tag[0] {
		case rollbackOpCopy:
			var args [16]byte
			if _, err := io.ReadFull(delta, args[:]); err != nil {
				return fmt.Errorf("rollback: op %d: truncated COPY: %w", i, err)
			}
			crc.Write(args[:])
			dstOff := int64(binary.BigEndian.Uint64(args[:8]))
			n := int64(binary.BigEndian.Uint64(args[8:]))
			if dstOff < 0 || n < 0 || (repairedSize >= 0 && dstOff+n > repairedSize) {
				return fmt.Errorf("rollback: op %d: COPY [%d,%d) outside the repaired file", i, dstOff, dstOff+n)
			}
			if written+n > originalSize {
				return fmt.Errorf("rollback: op %d: ops overrun the original size", i)
			}
			if _, err := repaired.Seek(dstOff, io.SeekStart); err != nil {
				return fmt.Errorf("rollback: op %d: seek repaired: %w", i, err)
			}
			if _, err := io.CopyBuffer(sink, io.LimitReader(repaired, n), copyBuf); err != nil {
				return fmt.Errorf("rollback: op %d: copy from repaired: %w", i, err)
			}
			written += n
		case rollbackOpLit:
			var arg [8]byte
			if _, err := io.ReadFull(delta, arg[:]); err != nil {
				return fmt.Errorf("rollback: op %d: truncated LIT: %w", i, err)
			}
			crc.Write(arg[:])
			n := int64(binary.BigEndian.Uint64(arg[:]))
			if n < 0 || written+n > originalSize {
				return fmt.Errorf("rollback: op %d: literal overruns the original size", i)
			}
			if _, err := io.CopyBuffer(io.MultiWriter(sink, crc), io.LimitReader(delta, n), copyBuf); err != nil {
				return fmt.Errorf("rollback: op %d: read literal: %w", i, err)
			}
			written += n
		case rollbackOpTrunc:
			var arg [8]byte
			if _, err := io.ReadFull(delta, arg[:]); err != nil {
				return fmt.Errorf("rollback: op %d: truncated TRUNC: %w", i, err)
			}
			crc.Write(arg[:])
			// Informational: the repaired file carries that much appended
			// tail; the streaming reconstruction produces nothing for it.
		default:
			return fmt.Errorf("rollback: op %d: unknown op 0x%02X", i, tag[0])
		}
	}

	// ── Entry crc, then the two final gates. ─────────────────────────────
	var wantCRC [4]byte
	if _, err := io.ReadFull(delta, wantCRC[:]); err != nil {
		return fmt.Errorf("rollback: read entry crc: %w", err)
	}
	if crc.Sum32() != binary.BigEndian.Uint32(wantCRC[:]) {
		return fmt.Errorf("rollback: entry crc mismatch (torn or corrupted delta)")
	}
	if written != originalSize {
		return fmt.Errorf("rollback: reconstruction is %d bytes, original was %d", written, originalSize)
	}
	var gotSrc [32]byte
	outHash.Sum(gotSrc[:0])
	if gotSrc != wantSrc {
		return fmt.Errorf("rollback: reconstructed file does not hash to the original sha256; refusing to deliver it")
	}
	return nil
}
