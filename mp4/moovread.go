package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/gravity-zero/mkvgo/internal/mpegts"
	"github.com/gravity-zero/mkvgo/mkv"
)

// findMoov returns the file offset and length of the moov box payload. It walks
// the top-level boxes by header; if that walk desyncs - a box whose declared
// size is wrong sends it into the mdat (some real files have a slightly-off mdat
// size) - it falls back to a bounded backward scan for the moov, which sits near
// the end in those files. That tolerance matches mainstream probers, which still read those files.
//
// A file that does not open on a box an ISO base media file can open with is
// refused on its first bytes (ErrNotMP4): the backward scan is a tolerance for
// a damaged MP4, not a way to find out that an MPEG-TS has no moov.
func findMoov(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
	if err := checkISOBMFFHead(r, size); err != nil {
		return 0, 0, err
	}
	if dataOff, plen, ferr := findMoovForward(r, size); ferr == nil {
		return dataOff, plen, nil
	}
	return findMoovBackward(r, size)
}

// ErrNotMP4 is returned when a source handed to the MP4 reader does not open
// on an ISO base media box: it is some other container, whatever its name
// says. errors.Is-able, so a caller can route the file elsewhere.
var ErrNotMP4 = errors.New("not an MP4/MOV file (it does not start with an ISO base media box)")

// ErrMPEGTS is returned, along with ErrNotMP4, for an MPEG transport stream (or a Blu-ray M2TS) handed to the MP4
// reader, whatever its name says: mkvgo does not read MPEG-TS. It is matroska.ErrMPEGTS too.
var ErrMPEGTS = mkv.ErrMPEGTS

// isoBMFFOpeners are the box types an ISO base media or QuickTime file - or a
// fragment of one - opens with.
var isoBMFFOpeners = map[string]bool{
	"ftyp": true, "styp": true, "moov": true, "moof": true, "mdat": true,
	"free": true, "skip": true, "wide": true, "pnot": true, "uuid": true,
	"sidx": true, "emsg": true, "prft": true, "meta": true,
}

// OpensLikeISOBMFF reports whether a file's first 8 bytes are the header of a box an MP4/MOV opens with.
func OpensLikeISOBMFF(head []byte) bool { return len(head) >= 8 && isoBMFFOpeners[string(head[4:8])] }

// checkISOBMFFHead refuses with ErrNotMP4 a source whose first box header
// does not name a box such a file opens with. A source too short to hold a
// box header is left to the walk, which says what it lacks.
func checkISOBMFFHead(r io.ReadSeeker, size int64) error {
	if size < 8 {
		return nil
	}
	var hdr [8]byte
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return errf("read box header: %w", err)
	}
	if !isoBMFFOpeners[string(hdr[4:8])] {
		if mpegts.At(r, size) {
			return mpegtsError{}
		}
		return errf("%w", ErrNotMP4)
	}
	return nil
}

// mpegtsError says ErrMPEGTS and is ErrNotMP4 too, for a caller that routes any other container elsewhere.
type mpegtsError struct{}

func (mpegtsError) Error() string   { return "mp4: " + ErrMPEGTS.Error() }
func (mpegtsError) Unwrap() []error { return []error{ErrMPEGTS, ErrNotMP4} }

func findMoovForward(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return 0, 0, err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return 0, 0, errf("read box header: %w", err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return 0, 0, errf("read largesize: %w", err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			return 0, 0, errf("box %q has invalid size %d at offset %d", typ, boxSize, off)
		}
		if typ == "moov" {
			return off + headerLen, boxSize - headerLen, nil
		}
		off += boxSize
	}
	return 0, 0, errf("no moov box found")
}

// maxMoovScanWindow caps the backward moov scan. A long movie's moov runs to
// tens of MB and only an extreme one to hundreds, so past this there is no moov.
const maxMoovScanWindow = 256 << 20

// moovScanChunk is how much of the file the backward scan holds at a time.
const moovScanChunk = 1 << 20

// findMoovBackward scans back from EOF for the last moov box, validated by its
// first child being a real moov child (mvhd/trak/…), so a "moov" byte sequence
// inside the mdat is not mistaken for it. The tail is read one chunk at a
// time, highest first, so the scan holds a chunk whatever the file: each chunk
// is read with the 7 bytes that follow it, and judges the candidates whose
// type starts at least 4 bytes into it - the size field before a type that
// starts sooner lies in the chunk below, which reads those bytes too.
func findMoovBackward(r io.ReadSeeker, size int64) (dataOffset, payloadLen int64, err error) {
	lowest := max(size-maxMoovScanWindow, 0)
	buf := make([]byte, moovScanChunk+7)
	for end := size; end > lowest; {
		start := max(end-moovScanChunk, lowest)
		chunk := buf[:min(end+7, size)-start]
		if _, err := r.Seek(start, io.SeekStart); err != nil {
			return 0, 0, err
		}
		if _, err := io.ReadFull(r, chunk); err != nil {
			return 0, 0, err
		}
		for hi := len(chunk); ; {
			i := bytes.LastIndex(chunk[:hi], []byte("moov"))
			if i < 4 { // need the 4-byte size field before the type
				break
			}
			boxStart := start + int64(i-4)
			boxSize := int64(binary.BigEndian.Uint32(chunk[i-4 : i]))
			if boxSize >= 16 && boxStart+boxSize <= size && validMoovAt(r, boxStart, boxSize, size) {
				return boxStart + 8, boxSize - 8, nil
			}
			hi = i
		}
		end = start
	}
	return 0, 0, errf("no moov box found")
}

// validMoovAt confirms a moov candidate by reading its first child header: a real
// moov opens with a known child box (mvhd in practice) whose size fits inside it.
func validMoovAt(r io.ReadSeeker, boxStart, boxSize, size int64) bool {
	if boxStart+16 > size {
		return false
	}
	var hdr [8]byte
	if _, err := r.Seek(boxStart+8, io.SeekStart); err != nil {
		return false
	}
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return false
	}
	childSize := int64(binary.BigEndian.Uint32(hdr[:4]))
	if childSize < 8 || childSize > boxSize-8 {
		return false
	}
	switch string(hdr[4:8]) {
	case "mvhd", "iods", "trak", "mvex", "udta", "meta":
		return true
	}
	return false
}

// readMoov returns the full moov box payload - every sample-table byte included.
// Used by the remux/extract path, which needs the complete tables.
func readMoov(r io.ReadSeeker, size int64) ([]byte, error) {
	return readMoovLimit(r, size, DefaultMaxMoovBytes)
}

// readMoovLimit is readMoov refusing a moov past limit bytes before reading it.
func readMoovLimit(r io.ReadSeeker, size, limit int64) ([]byte, error) {
	dataOff, payloadLen, err := findMoov(r, size)
	if err != nil {
		return nil, err
	}
	if err := checkMoovSize(payloadLen, limit); err != nil {
		return nil, err
	}
	return readMoovAt(r, dataOff, payloadLen)
}

// checkMoovSize refuses a moov payload past limit, saying how to raise it.
func checkMoovSize(payloadLen, limit int64) error {
	if payloadLen > limit {
		return errf("moov box declares %d bytes, past the %d-byte limit: raise Options.MaxMoovBytes to read a source this large", payloadLen, limit)
	}
	return nil
}

// readMoovAt reads the whole moov payload findMoov located.
func readMoovAt(r io.ReadSeeker, dataOff, payloadLen int64) ([]byte, error) {
	if _, err := r.Seek(dataOff, io.SeekStart); err != nil {
		return nil, err
	}
	payload, err := readExact(r, payloadLen)
	if err != nil {
		return nil, errf("read moov: %w", err)
	}
	return payload, nil
}

// lazyContainers are the boxes readMoovLazy descends into to reach the sample
// tables; lazySkipBody are the large sample-table boxes whose bodies it skips
// (reading only the header + first entry). Everything else is read whole.
var lazyContainers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true, "edts": true,
}

var lazySkipBody = map[string]bool{
	"stsz": true, "stco": true, "co64": true, "stsc": true, "stz2": true,
}

// lazyKeepHead is how much of a skipped box's body is still read: its
// version/flags + count + first entry - enough for headerFrameCount and
// firstSampleLoc, which are all the metadata/keyframe path reads from them.
const lazyKeepHead = 16

// lazyChunk is how far ahead the lazy reader fills in one disk read, so the
// scattered small boxes (headers + stsd/stts/stss/ctts bodies) come in a few
// large contiguous reads rather than one read per box.
const lazyChunk = 64 << 10

// readMoovLazy returns a moov payload buffer of the real moov size, reading from
// disk only the small boxes plus the header and first entry of the large
// sample-table boxes (stsz/stco/co64/stsc/stz2); their bodies are left zeroed and
// never fetched. The buffer is structurally identical to a full moov read (same
// box headers at the same offsets), so iterBoxes/parseTrak treat it the same  -
// the metadata/keyframe path never reads the skipped bodies. Reads are coalesced
// into lazyChunk-sized runs and only the large bodies are seeked over, so it is
// both I/O-light and round-trip-light on a network mount, where those sample
// tables are most of the moov's bytes. Any read error or out-of-range box returns
// an error, so the caller falls back to the full read.
func readMoovLazy(r io.ReadSeeker, size int64) ([]byte, error) {
	dataOff, payloadLen, err := findMoov(r, size)
	if err != nil {
		return nil, err
	}
	return readMoovLazyAt(r, dataOff, payloadLen)
}

// maxLazyMoovBytes is the largest moov readMoovLazyAt allocates up front; a
// larger declared moov goes to readMoovAt, whose buffer grows with the bytes
// that arrive, so a size a remote server or a sparse file declares is never
// allocated before it is read.
const maxLazyMoovBytes = 256 << 20

// readMoovLazyAt is readMoovLazy on the moov payload findMoov located.
func readMoovLazyAt(r io.ReadSeeker, dataOff, payloadLen int64) ([]byte, error) {
	// The buffer is filled sparsely (large sample tables are seeked over), so
	// it is allocated at the declared size and capped for that reason.
	if payloadLen > maxLazyMoovBytes {
		return nil, errf("moov of %d bytes is past the %d-byte lazy read", payloadLen, int64(maxLazyMoovBytes))
	}
	m := &lazyMoov{r: r, base: dataOff, buf: make([]byte, payloadLen)}
	if err := m.parse(0, len(m.buf), 0); err != nil {
		return nil, err
	}
	return m.buf, nil
}

// lazyMoov fills a moov buffer forward, coalescing reads and skipping the large
// sample-table bodies. filled is how far buf is populated (read, or zeroed by a
// skip); the next disk read starts at base+filled.
type lazyMoov struct {
	r      io.ReadSeeker
	base   int64
	buf    []byte
	filled int
}

// ensure loads buf up to at least n bytes, reading from disk in lazyChunk runs.
func (m *lazyMoov) ensure(n int) error {
	if n <= m.filled {
		return nil
	}
	end := m.filled + lazyChunk
	if end < n {
		end = n
	}
	if end > len(m.buf) {
		end = len(m.buf)
	}
	if _, err := m.r.Seek(m.base+int64(m.filled), io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(m.r, m.buf[m.filled:end]); err != nil {
		return err
	}
	m.filled = end
	return nil
}

// skip advances filled past a box body without reading it (the bytes stay zero);
// the next ensure seeks over the gap.
func (m *lazyMoov) skip(boxEnd int) {
	if boxEnd > m.filled {
		m.filled = boxEnd
	}
}

// maxLazyDepth bounds how deep parse descends: moov, trak, mdia, minf, stbl hold the sample tables five levels
// down; a file nesting containers deeper is forged, and each level costs a stack frame.
const maxLazyDepth = 8

func (m *lazyMoov) parse(start, end, depth int) error {
	if depth > maxLazyDepth {
		return errf("the moov nests containers deeper than %d levels: the file is damaged or forged", maxLazyDepth)
	}
	for off := start; off+8 <= end; {
		if err := m.ensure(off + 8); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(m.buf[off : off+4]))
		typ := string(m.buf[off+4 : off+8])
		hdr := 8
		switch boxSize {
		case 1:
			if off+16 > end {
				return errf("truncated 64-bit box %q", typ)
			}
			if err := m.ensure(off + 16); err != nil {
				return err
			}
			boxSize = int64(binary.BigEndian.Uint64(m.buf[off+8 : off+16]))
			hdr = 16
		case 0:
			boxSize = int64(end - off)
		}
		if boxSize < int64(hdr) || boxSize > int64(end-off) {
			return errf("box %q has invalid size %d", typ, boxSize)
		}
		boxEnd := off + int(boxSize)
		switch {
		case lazyContainers[typ]:
			if err := m.parse(off+hdr, boxEnd, depth+1); err != nil {
				return err
			}
		case lazySkipBody[typ]:
			keep := boxEnd
			if off+hdr+lazyKeepHead < keep {
				keep = off + hdr + lazyKeepHead
			}
			if err := m.ensure(keep); err != nil {
				return err
			}
			m.skip(boxEnd)
		default:
			if err := m.ensure(boxEnd); err != nil {
				return err
			}
		}
		off = boxEnd
	}
	return nil
}
