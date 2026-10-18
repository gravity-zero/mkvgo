package mp4

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gravity-zero/mkvgo/internal/fileid"
	"github.com/gravity-zero/mkvgo/mkv"
)

// statShared stats path through a reader opened under share when that reader
// pins versions and answers Stat (a remote one): it fetches the bytes the
// planning reads first along with the answer. shared is false otherwise.
func statShared(ctx context.Context, fs *mkv.FS, path string, share *mkv.ReadShare) (fi os.FileInfo, shared bool, err error) {
	f, oerr := fs.DoOpenContext(mkv.WithReadShare(ctx, share), path)
	if oerr != nil {
		return nil, false, nil
	}
	defer f.Close()
	st, ok := f.(interface{ Stat() (os.FileInfo, error) })
	if _, pins := f.(mkv.PinnedReader); !ok || !pins {
		return nil, false, nil
	}
	fi, err = st.Stat()
	return fi, true, err
}

// hintsRanges reports whether path opens as a reader that takes range hints (mkv.RangeHinter): a remote one.
func hintsRanges(fs *mkv.FS, path string) bool {
	if fs == nil {
		return false
	}
	f, err := fs.DoOpen(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, ok := f.(mkv.RangeHinter)
	return ok
}

// pinsReaders reports whether path opens as a reader that can be pinned to a version (mkv.PinnedReader).
func pinsReaders(fs *mkv.FS, path string) bool {
	f, err := fs.DoOpen(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, ok := f.(mkv.PinnedReader)
	return ok
}

// pinnedPlanningFS is fs with every reader of path pinned to version id and
// opened under share (mkv.ShareReads), and path's stat answered by fi, the
// stat that version was read from.
func pinnedPlanningFS(share *mkv.ReadShare, fs *mkv.FS, path string, fi os.FileInfo, id string) *mkv.FS {
	pin := func(f mkv.ReadSeekCloser, err error) (mkv.ReadSeekCloser, error) {
		if pr, ok := f.(mkv.PinnedReader); ok && err == nil {
			pr.Pin(id)
		}
		return f, err
	}
	w := *fs
	w.Open = func(p string) (mkv.ReadSeekCloser, error) {
		if p != path {
			return fs.DoOpen(p)
		}
		return pin(fs.DoOpenContext(mkv.WithReadShare(context.Background(), share), p))
	}
	w.OpenContext = func(ctx context.Context, p string) (mkv.ReadSeekCloser, error) {
		if p != path {
			return fs.DoOpenContext(ctx, p)
		}
		return pin(fs.DoOpenContext(mkv.WithReadShare(ctx, share), p))
	}
	w.Stat = func(p string) (os.FileInfo, error) {
		if p != path {
			return fs.DoStat(p)
		}
		return fi, nil
	}
	return &w
}

// sourceSig is what stampSource records of the source, as one string; known is false when it cannot be stat'ed.
func sourceSig(fs *mkv.FS, path string) (sig string, known bool) {
	fi, err := fs.DoStat(path)
	if err != nil {
		return "", false
	}
	id, _ := statIdentity(fi)
	return statStamp(fi) + "|" + id, true
}

// statIdentity is a source's identity beyond its stamp - the version its FS declares, else the OS file id - and whether fi is the OS's own stat.
func statIdentity(fi os.FileInfo) (id string, osStat bool) {
	osStat = os.SameFile(fi, fi) // SameFile knows only the OS's own stat
	if v, ok := fi.(mkv.FileIdentity); ok {
		return v.Identity(), osStat
	}
	if osStat {
		return fileid.Of(fi), true
	}
	return "", false
}

// noteRecovered counts a subtitle block inflated without a declaration (Stats.UndeclaredZlibBlocks).
func (p *HLSPlan) noteRecovered(recovered bool) {
	if !recovered {
		return
	}
	p.winMu.Lock()
	p.stats.UndeclaredZlibBlocks++
	p.winMu.Unlock()
}

// openSource opens the source for reads bound to ctx and binds the open file to the one the plan was
// built on: an OS file is fstat'ed, a PinnedReader is pinned to the identity.
func (p *HLSPlan) openSource(ctx context.Context) (mkv.ReadSeekCloser, error) {
	f, err := p.fs.DoOpenContext(ctx, p.srcPath)
	if err != nil || p.stamp == "" || strings.HasPrefix(p.stamp, "t") {
		return f, err
	}
	if of, ok := f.(*os.File); ok && p.osStat {
		fi, err := of.Stat()
		if err == nil && (statStamp(fi) != p.stamp || !os.SameFile(fi, p.srcInfo)) {
			p.srcChanged.Store(true)
			err = ErrSourceChanged
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	if pr, ok := f.(mkv.PinnedReader); ok && p.srcID != "" {
		pr.Pin(p.srcID)
	}
	return f, nil
}

// stampSource records what the plan holds the source to: its stamp (size and mtime, else the moment the plan was built), its identity, and whether reads verify it themselves.
func (p *HLSPlan) stampSource() {
	fi, err := p.fs.DoStat(p.srcPath)
	if err != nil {
		p.stamp = fmt.Sprintf("t%x", time.Now().UnixNano())
		return
	}
	p.stamp, p.srcInfo = statStamp(fi), fi
	p.srcID, p.osStat = statIdentity(fi)
	p.pinned = p.osStat
	if !p.pinned && p.srcID != "" {
		if f, err := p.fs.DoOpen(p.srcPath); err == nil {
			_, p.pinned = f.(mkv.PinnedReader)
			f.Close()
		}
	}
}

func statStamp(fi os.FileInfo) string {
	return fmt.Sprintf("%x-%x", fi.Size(), fi.ModTime().UnixNano())
}

// readFullAt reads len(buf) bytes at off, io.ReadFull's contract: by ReadAt when the reader has it, else by seeking.
func readFullAt(src mkv.ReadSeekCloser, buf []byte, off int64) (int, error) {
	if ra, ok := src.(io.ReaderAt); ok {
		n, err := ra.ReadAt(buf, off)
		if n == len(buf) {
			return n, nil
		}
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if n == 0 && err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return n, err
	}
	if _, err := src.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(src, buf)
}

// ctxReadMax caps one Read of ctxReadSeeker, so a large moov is read in pieces with a check before each.
const ctxReadMax = 1 << 20

// ctxReadSeeker fails every Read once ctx is done, so a walk that reads each fragment stops within one read.
type ctxReadSeeker struct {
	io.ReadSeeker
	ctx context.Context
}

func (r ctxReadSeeker) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > ctxReadMax {
		p = p[:ctxReadMax]
	}
	return r.ReadSeeker.Read(p)
}
