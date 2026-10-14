//go:build !(linux && (amd64 || arm64))

package mp4

import "os"

// willNeed is a no-op where posix_fadvise is not called (see prefetch_linux.go).
func willNeed(*os.File, int64, int64) {}
