package ops

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

// createOutput creates dst for writing, refusing a dst that is one of the inputs: creating it truncates what the
// operation is about to read, and the cleanup of the failed write then removes it.
func createOutput(fs *mkv.FS, dst string, inputs ...string) (mkv.WriteSeekCloser, error) {
	if err := refuseInput(fs, dst, inputs...); err != nil {
		return nil, err
	}
	return fs.DoCreate(dst)
}

// refuseInput refuses a dst that is one of the inputs, before anything is written to it.
func refuseInput(fs *mkv.FS, dst string, inputs ...string) error {
	for _, in := range inputs {
		if in != "" && sameFile(fs, dst, in) {
			return fmt.Errorf("%s is also an input: write the output to another file", shown.Path(dst))
		}
	}
	return nil
}

// sameFile reports whether a and b name one file: the same path, or two paths the filesystem resolves to one file.
func sameFile(fs *mkv.FS, a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, errA := fs.DoStat(a)
	fb, errB := fs.DoStat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

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
