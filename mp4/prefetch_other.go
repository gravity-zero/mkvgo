//go:build !(linux && (amd64 || arm64))

package mp4

import "os"

// willNeed and readAhead are no-ops where they are not called (see prefetch_linux.go).
func willNeed(*os.File, int64, int64) {}

func readAhead(*os.File, int64, int64) {}
