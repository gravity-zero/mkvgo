package ops

import (
	"github.com/gravity-zero/mkvgo/internal/outguard"
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
