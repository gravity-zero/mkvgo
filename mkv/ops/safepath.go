package ops

import (
	"strings"

	"github.com/gravity-zero/mkvgo/internal/splitplan"
)

// safePath validates that a resolved path stays within baseDir.
func safePath(baseDir, name string) (string, error) { return splitplan.SafePath(baseDir, name) }

// sanitizeCodec strips path separators from codec names used in filenames.
func sanitizeCodec(codec string) string {
	codec = strings.ReplaceAll(codec, "/", "_")
	codec = strings.ReplaceAll(codec, "\\", "_")
	codec = strings.ReplaceAll(codec, "..", "_")
	if codec == "" {
		codec = "raw"
	}
	return codec
}
