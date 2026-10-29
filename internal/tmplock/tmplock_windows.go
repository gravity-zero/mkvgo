//go:build windows

package tmplock

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorSharingViolation   = syscall.Errno(32)
	errorLockViolation      = syscall.Errno(33)
)

const haveLocks = true

// openForLock opens path sharing delete, so the lock handle does not keep the writer from renaming the file.
func openForLock(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// lockFile locks one byte past any data the file will hold, so the lock never blocks the writer's own reads and
// writes.
func lockFile(h *os.File) error {
	ol := syscall.Overlapped{Offset: 0xFFFFFFFE, OffsetHigh: 0x7FFFFFFF}
	r, _, err := procLockFileEx.Call(h.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return ErrBusy
	}
	return err
}

// inUse reports a remove refused because a writer has the file open: Windows keeps an open file in place.
func inUse(err error) bool { return errors.Is(err, errorSharingViolation) }
