package ops

import (
	"fmt"
	"path/filepath"
	"strings"
)

// safePath validates that a resolved path stays within baseDir.
func safePath(baseDir, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute path not allowed: %s", name)
	}
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("path escapes base directory: %s", name)
	}
	full := filepath.Join(baseDir, clean)
	rel, err := filepath.Rel(baseDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes base directory: %s", name)
	}
	return full, nil
}

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
