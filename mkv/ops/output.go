package ops

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/gravity-zero/mkvgo/internal/outguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// output is an operation's output file. On the OS it is written under a temporary name next to dst and moved
// onto dst once complete, so a failed operation leaves the file dst already named as it was.
type output struct {
	mkv.WriteSeekCloser
	fs  *mkv.FS
	dst string
	// tmp is the temporary file; "" when the output is written to dst itself (a custom FS, a dst that is not
	// a regular file, a directory nothing can be created in), removed on failure when removeDst.
	tmp       string
	removeDst bool
}

// createOutput creates the output for dst, refusing a dst that is one of the inputs: writing it would destroy
// what the operation is about to read. The operation defers finish right after.
func createOutput(fsys *mkv.FS, dst string, inputs ...string) (*output, error) {
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
	f, tmp, terr := createTemp(filepath.Dir(dst))
	if errors.Is(terr, fs.ErrPermission) {
		return createInPlace(fsys, dst, true)
	}
	if terr != nil {
		return nil, terr
	}
	if err == nil {
		_ = f.Chmod(st.Mode().Perm())
	}
	return &output{WriteSeekCloser: f, fs: fsys, dst: dst, tmp: tmp}, nil
}

// createInPlace creates the output as dst itself.
func createInPlace(fsys *mkv.FS, dst string, removeDst bool) (*output, error) {
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	return &output{WriteSeekCloser: f, fs: fsys, dst: dst, removeDst: removeDst}, nil
}

// createTemp creates a new file in dir under a name no other file has.
func createTemp(dir string) (*os.File, string, error) {
	var err error
	for range 100 {
		tmp := filepath.Join(dir, ".mkvgo-"+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
		var f *os.File
		if f, err = os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666); !errors.Is(err, fs.ErrExist) {
			return f, tmp, err
		}
	}
	return nil, "", err
}

// finish ends the output: it closes it and, when *err is nil, puts the complete file under dst; otherwise it
// removes what was written. A close or move error is surfaced through *err.
func (o *output) finish(err *error) {
	o.close(err)
	o.commit(err)
}

// close flushes a temporary file to disk and closes the output.
func (o *output) close(err *error) {
	if f, ok := o.WriteSeekCloser.(*os.File); ok && o.tmp != "" && *err == nil {
		if serr := f.Sync(); serr != nil && !errors.Is(serr, errors.ErrUnsupported) {
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
	if *err == nil {
		*err = os.Rename(o.tmp, o.dst)
	}
	if *err != nil {
		_ = os.Remove(o.tmp)
		return
	}
	flushDir(filepath.Dir(o.dst))
}

// flushDir flushes dir's entries to disk so a completed move survives a crash; Windows has no such call.
func flushDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
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
