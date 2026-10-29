//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package tmplock

import (
	"errors"
	"os"
	"syscall"
)

const haveLocks = true

func openForLock(path string) (*os.File, error) { return os.Open(path) }

// lockFile takes flock's exclusive lock without waiting: it belongs to h's open file, so it outlives the writer's
// own descriptor and the rename.
func lockFile(h *os.File) error {
	c, err := h.SyscallConn()
	if err != nil {
		return err
	}
	var lerr error
	if err := c.Control(func(fd uintptr) { lerr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB) }); err != nil {
		return err
	}
	if errors.Is(lerr, syscall.EWOULDBLOCK) || errors.Is(lerr, syscall.EAGAIN) {
		return ErrBusy
	}
	return lerr
}

func inUse(error) bool { return false }
