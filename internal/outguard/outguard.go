// Package outguard holds the rule every writer applies before it creates its output: the output is never one of
// the files the operation reads. Each engine passes the stat of its own filesystem.
package outguard

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/gravity-zero/mkvgo/internal/shown"
)

// Stat is the filesystem port: os.Stat, or an FS's DoStat.
type Stat func(string) (os.FileInfo, error)

// Distinct refuses a dst that names one of the inputs, by path or by file (a hard link, a path written two ways);
// empty inputs are skipped, and a dst that does not exist yet is no input.
func Distinct(stat Stat, dst string, inputs ...string) error {
	for _, in := range inputs {
		if in != "" && SameFile(stat, dst, in) {
			return errors.New(shown.Path(dst) + " is also an input: write the output to another file")
		}
	}
	return nil
}

// SameFile reports whether a and b name one file: the same path, or two paths the filesystem resolves to one file.
func SameFile(stat Stat, a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, errA := stat(a)
	fb, errB := stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}
