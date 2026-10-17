package mp4

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/gravity-zero/mkvgo/mkv"
)

// openDirectSource opens the source a full pass reads its segments' samples
// from, at the offsets its walk recorded - or returns nil when the samples go
// through a temp file instead: a source across a network, where each sample
// read is a round trip (Options.SourceAccess, else a local file's mount).
func openDirectSource(o *Options, fs *mkv.FS, path string) (mkv.ReadSeekCloser, error) {
	if o.SourceAccess == SourceNetwork {
		return nil, nil
	}
	src, err := fs.DoOpen(path)
	if err != nil {
		return nil, err
	}
	if !directReads(o, src) {
		src.Close()
		return nil, nil
	}
	return src, nil
}

// directReads says whether a full pass may read src at recorded offsets, a
// block header at a time: Options.SourceAccess when set, else a local file.
func directReads(o *Options, src io.Reader) bool {
	switch o.SourceAccess {
	case SourceLocal:
		return true
	case SourceNetwork:
		return false
	}
	f, ok := src.(*os.File)
	return ok && !networkSource(f)
}

// directTrack reports whether a track's samples can be read from the source
// as they are stored: no conversion, no compression but a stripped header.
func directTrack(t *outTrack) bool {
	return t.conv == nil && t.mkv.Compression.Verbatim()
}

// learnFirstFrame reads a block walked by its header alone when the track has
// still to learn from its first frame: its sample entry, its frame size.
func learnFirstFrame(t *outTrack, b mkv.Block, src io.ReadSeeker) error {
	if t.sampleEntry != nil && t.frameLearned {
		return nil
	}
	raw, err := readSample(src, b.DataOffset, uint32(b.Size))
	if err != nil {
		return errf("read sample: %w", err)
	}
	data, err := t.mkv.DecodePayload(raw)
	if err != nil {
		return err
	}
	t.learnFrame(data)
	if t.sampleEntry == nil {
		if t.sampleEntry, err = t.spec.sampleEntry(&t.mkv, data); err != nil {
			return err
		}
	}
	return nil
}

// trackMedia hands out a track's sample bytes in order, from the source at the
// recorded offsets, or from the temp file the walk filled.
type trackMedia struct {
	tmp    mkv.ReadSeekCloser
	src    io.ReadSeeker
	file   *os.File
	prefix []byte
	offs   []int64
	buf    []byte
	wipe   bool // the bytes are an encrypted segment's plaintext: pooled buffers go back zeroed
}

// openTrackMedia opens each track's sample bytes; src is the direct source, nil when every track has a temp file.
func openTrackMedia(fs *mkv.FS, fts []*fragTrack, src mkv.ReadSeekCloser, buf []byte, wipe bool) ([]*trackMedia, error) {
	ms := make([]*trackMedia, len(fts))
	for i, ft := range fts {
		if ft.offs != nil {
			m := &trackMedia{src: src, prefix: ft.outTrack.mkv.HeaderStripping, offs: ft.offs, buf: buf, wipe: wipe}
			m.file, _ = src.(*os.File)
			ms[i] = m
			continue
		}
		r, err := fs.DoOpen(ft.tmpPath)
		if err != nil {
			closeTrackMedia(ms)
			return nil, err
		}
		ms[i] = &trackMedia{tmp: r}
	}
	return ms, nil
}

func closeTrackMedia(ms []*trackMedia) {
	for _, m := range ms {
		if m != nil && m.tmp != nil {
			m.tmp.Close()
		}
	}
}

// write writes head, then the dataLen bytes of the track's samples from index from on.
func (m *trackMedia) write(w io.Writer, head []byte, samples []fragSample, from int, dataLen int64) error {
	if m.tmp != nil {
		return writeSegmentFile(w, head, dataLen, m.tmp)
	}
	c := &directCopier{w: w, src: m.src, file: m.file, buf: m.buf, wipe: m.wipe}
	if err := c.emit(head); err != nil {
		return err
	}
	var run span
	copyRun := func() error {
		if run.n == 0 {
			return nil
		}
		err := c.copy(run.off, run.n)
		run = span{}
		return err
	}
	for x := range samples {
		if len(m.prefix) > 0 {
			if err := copyRun(); err != nil {
				return errf("copy segment media: %w", err)
			}
			if err := c.emit(m.prefix); err != nil {
				return err
			}
		}
		off, n := m.offs[from+x], int64(samples[x].size)-int64(len(m.prefix))
		if run.n > 0 && run.off+run.n == off {
			run.n += n
			continue
		}
		if err := copyRun(); err != nil {
			return errf("copy segment media: %w", err)
		}
		run = span{off: off, n: n}
	}
	if err := copyRun(); err != nil {
		return errf("copy segment media: %w", err)
	}
	return c.flush()
}

// fullPassBuf is a full pass's copy buffer: the runs of samples it gathers
// (one read each, exactly the sample bytes) go out in writes of this size.
const fullPassBuf = 1 << 20

// directCopier copies runs of the source into one writer through a buffer; a
// run the buffer cannot hold is handed to a writer that reads from the file.
type directCopier struct {
	w    io.Writer
	src  io.ReadSeeker
	file *os.File
	buf  []byte
	used int
	wipe bool
}

// emit queues bytes the caller holds (a head, a stripped header).
func (c *directCopier) emit(b []byte) error {
	for len(b) > 0 {
		if c.used == len(c.buf) {
			if err := c.flush(); err != nil {
				return err
			}
		}
		n := copy(c.buf[c.used:], b)
		c.used, b = c.used+n, b[n:]
	}
	return nil
}

// copy queues the source's bytes [off, off+n).
func (c *directCopier) copy(off, n int64) error {
	if n >= int64(len(c.buf)) {
		if err := c.flush(); err != nil {
			return err
		}
		if rf, ok := c.w.(io.ReaderFrom); ok && c.file != nil {
			if _, err := c.file.Seek(off, io.SeekStart); err != nil {
				return err
			}
			m, err := rf.ReadFrom(io.LimitReader(c.file, n))
			if err == nil && m < n {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		for n > 0 {
			k := min(n, int64(len(c.buf)))
			if err := c.read(off, c.buf[:k]); err != nil {
				return err
			}
			c.used = int(k)
			if err := c.flush(); err != nil {
				return err
			}
			off, n = off+k, n-k
		}
		return nil
	}
	if n > int64(len(c.buf)-c.used) {
		if err := c.flush(); err != nil {
			return err
		}
	}
	if err := c.read(off, c.buf[c.used:c.used+int(n)]); err != nil {
		return err
	}
	c.used += int(n)
	return nil
}

// read fills b from the source at off; a source ending before the sample does is io.ErrUnexpectedEOF.
func (c *directCopier) read(off int64, b []byte) error {
	if c.file != nil {
		_, err := c.file.ReadAt(b, off)
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if _, err := c.src.Seek(off, io.SeekStart); err != nil {
		return err
	}
	_, err := io.ReadFull(c.src, b)
	return err
}

// flush writes the buffer in one write; fewer bytes taken without an error is io.ErrShortWrite.
func (c *directCopier) flush() error {
	if c.used == 0 {
		return nil
	}
	m, err := c.w.Write(c.buf[:c.used])
	if err == nil && m < c.used {
		err = io.ErrShortWrite
	}
	if c.wipe {
		clear(c.buf[:c.used])
	}
	c.used = 0
	return err
}

// read returns the dataLen bytes of the track's samples from index from on.
func (m *trackMedia) read(samples []fragSample, from int, dataLen int64) ([]byte, error) {
	if err := segmentFits(dataLen); err != nil {
		return nil, err
	}
	if m.tmp != nil {
		b := make([]byte, dataLen)
		if _, err := io.ReadFull(m.tmp, b); err != nil {
			return nil, errf("read segment media: %w", err)
		}
		return b, nil
	}
	b := bytes.NewBuffer(make([]byte, 0, dataLen))
	if err := m.write(b, nil, samples, from, dataLen); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
