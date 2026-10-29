//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package tmplock

import (
	"errors"
	"os"
)

const haveLocks = false

func openForLock(path string) (*os.File, error) { return os.Open(path) }

func lockFile(*os.File) error { return errors.ErrUnsupported }

func inUse(error) bool { return false }
