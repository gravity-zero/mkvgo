package ops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gravity-zero/mkvgo/internal/outguard"
	"github.com/gravity-zero/mkvgo/internal/tmplock"
	"github.com/gravity-zero/mkvgo/mkv"
)

// output is an operation's output file. On the OS it is written to dst+tempSuffix and moved
// onto dst once complete, so a failed operation leaves the file dst already named as it was.
type output struct {
	mkv.WriteSeekCloser
	fs  *mkv.FS
	dst string
	// tmp is the temporary file; "" when the output is written to dst itself (a custom FS, a dst that is not
	// a regular file, a directory nothing can be created in), removed on failure when removeDst.
	tmp       string
	lock      *tmplock.Lock // held on tmp until it is renamed or removed
	removeDst bool
	sync      bool // Options.SyncOutput
}

// syncFile flushes a file to disk; a test counts the calls.
var syncFile = (*os.File).Sync

// createOutput creates the output for dst, refusing a dst that is one of the inputs: writing it would destroy
// what the operation is about to read. With sync (Options.SyncOutput) the output reaches the disk before it is
// renamed. The operation defers finish right after.
func createOutput(fsys *mkv.FS, sync bool, dst string, inputs ...string) (*output, error) {
	if err := refuseInput(fsys, dst, inputs...); err != nil {
		return nil, err
	}
	if fsys != nil && fsys.Create != nil {
		w, err := fsys.DoCreate(dst)
		if err != nil {
			return nil, err
		}
		return &output{WriteSeekCloser: w, fs: fsys, dst: dst, removeDst: true}, nil
	}
	st, err := os.Lstat(dst)
	if err == nil && !st.Mode().IsRegular() {
		// A device, a pipe or a link is written through, as before, and never removed.
		return createInPlace(fsys, dst, false)
	}
	f, tmp, lock, terr := createTemp(dst)
	if errors.Is(terr, fs.ErrPermission) {
		return createInPlace(fsys, dst, true)
	}
	if terr != nil {
		return nil, terr
	}
	if err == nil {
		_ = f.Chmod(st.Mode().Perm())
	}
	return &output{WriteSeekCloser: f, fs: fsys, dst: dst, tmp: tmp, lock: lock, sync: sync}, nil
}

// createInPlace creates the output as dst itself.
func createInPlace(fsys *mkv.FS, dst string, removeDst bool) (*output, error) {
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	return &output{WriteSeekCloser: f, fs: fsys, dst: dst, removeDst: removeDst}, nil
}

// tempSuffix names an output's temporary file, dst+tempSuffix, so a leftover of a killed run is recognisable.
const tempSuffix = ".mkvgo.tmp"

// createTemp creates dst's temporary file and locks it, replacing a regular file a killed run left there and
// refusing one a live run holds (internal/tmplock). Anything else under that name is refused: the output is never
// written through a link.
func createTemp(dst string) (*os.File, string, *tmplock.Lock, error) {
	tmp := dst + tempSuffix
	if st, err := os.Lstat(tmp); err == nil {
		if !st.Mode().IsRegular() {
			return nil, "", nil, tempInTheWay(tmp, dst)
		}
		if err := tmplock.Clear(tmp); err != nil {
			return nil, "", nil, busy(dst, tmp, err)
		}
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, fs.ErrExist) {
		if st, serr := os.Lstat(tmp); serr == nil && st.Mode().IsRegular() {
			return nil, "", nil, busy(dst, tmp, tmplock.ErrBusy)
		}
		return nil, "", nil, tempInTheWay(tmp, dst)
	}
	if err != nil {
		return nil, "", nil, err
	}
	lock, err := tmplock.Take(tmp, f)
	if err != nil {
		_ = f.Close()
		return nil, "", nil, busy(dst, tmp, err)
	}
	return f, tmp, lock, nil
}

// busy is err with tmplock's refusals said for the user: mkv.ErrOutputBusy naming the file two runs aimed at, and
// a temporary file that cannot be checked. Neither wraps fs.ErrPermission, which would write over dst instead.
func busy(dst, tmp string, err error) error {
	switch {
	case errors.Is(err, tmplock.ErrBusy):
		return fmt.Errorf("%s: %w", dst, mkv.ErrOutputBusy)
	case errors.Is(err, tmplock.ErrUnchecked):
		return fmt.Errorf("%s, where %s is written before it is moved into place, belongs to another user or cannot be read: remove it if no other run is writing %s", tmp, dst, dst)
	}
	return err
}

func tempInTheWay(tmp, dst string) error {
	return fmt.Errorf("%s, where the output is written before it is moved onto %s, is not a regular file: remove it and try again", tmp, dst)
}

// finish ends the output: it closes it and, when *err is nil, puts the complete file under dst; otherwise it
// removes what was written. A close or move error is surfaced through *err.
func (o *output) finish(err *error) {
	o.close(err)
	o.commit(err)
}

// close closes the output, flushing a temporary file to disk first under sync.
func (o *output) close(err *error) {
	if f, ok := o.WriteSeekCloser.(*os.File); ok && o.tmp != "" && o.sync && *err == nil {
		if serr := syncFile(f); serr != nil && !errors.Is(serr, errors.ErrUnsupported) {
			*err = serr
		}
	}
	closeWithErr(o.WriteSeekCloser, err)
}

// commit moves a closed output onto dst when *err is nil, and removes it otherwise.
func (o *output) commit(err *error) {
	if o.tmp == "" {
		if *err != nil && o.removeDst {
			_ = o.fs.DoRemove(o.dst)
		}
		return
	}
	defer o.lock.Release()
	if *err == nil {
		*err = os.Rename(o.tmp, o.dst)
	}
	if *err != nil {
		_ = os.Remove(o.tmp)
		return
	}
	if o.sync {
		flushDir(filepath.Dir(o.dst))
	}
}

// flushDir flushes dir's entries to disk so a completed move survives a crash; Windows has no such call.
func flushDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = syncFile(d)
		_ = d.Close()
	}
}

// refuseInput refuses a dst that is one of the inputs, before anything is written to it (internal/outguard).
func refuseInput(fs *mkv.FS, dst string, inputs ...string) error {
	return outguard.Distinct(fs.DoStat, dst, inputs...)
}

// sameFile reports whether a and b name one file (internal/outguard).
func sameFile(fs *mkv.FS, a, b string) bool { return outguard.SameFile(fs.DoStat, a, b) }

// attachmentPaths are the files lazy attachments are copied from at write time: inputs like any source.
func attachmentPaths(attachments []mkv.Attachment) []string {
	var paths []string
	for _, a := range attachments {
		if a.DataPath != "" {
			paths = append(paths, a.DataPath)
		}
	}
	return paths
}
