//go:build !linux && !darwin

package mp4

import "os"

// changeTime: no inode change time here (Windows, WASM): the stamp is the size and the modification time alone.
func changeTime(os.FileInfo) (int64, bool) { return 0, false }
