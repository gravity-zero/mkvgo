// Package tmplock locks an output's temporary file while a run writes it, so a second run aiming at the same output
// tells a live temporary file from one a killed run left behind: the first is refused, the second replaced.
package tmplock

import (
	"errors"
	"io/fs"
	"os"
)

// ErrBusy reports a temporary file another run holds.
var ErrBusy = errors.New("held by another run")

// Lock is a held lock; the zero Lock holds nothing (a file system without locks).
type Lock struct{ h *os.File }

// Take locks f, the file this run just created at path. ErrBusy when another run holds it, or when path no longer
// names f (another run replaced it first). Where it cannot be locked (no locks on the file system, the file not
// readable) it is written as before, unlocked.
func Take(path string, f *os.File) (*Lock, error) {
	h, err := openForLock(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrBusy
	}
	if err != nil {
		return &Lock{}, nil
	}
	if err := lockFile(h); err != nil {
		_ = h.Close()
		if errors.Is(err, ErrBusy) {
			return nil, ErrBusy
		}
		return &Lock{}, nil
	}
	a, aerr := f.Stat()
	b, berr := h.Stat()
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		_ = h.Close()
		return nil, ErrBusy
	}
	return &Lock{h: h}, nil
}

// Release releases the lock; nil and the zero Lock are no-ops.
func (l *Lock) Release() {
	if l != nil && l.h != nil {
		_ = l.h.Close()
		l.h = nil
	}
}

// Clear removes the regular file a killed run left at path, ErrBusy when a live run holds it. A missing file is
// no error.
func Clear(path string) error {
	h, err := openForLock(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := lockFile(h); errors.Is(err, ErrBusy) {
		_ = h.Close()
		return ErrBusy
	}
	rerr := os.Remove(path)
	_ = h.Close()
	switch {
	case rerr == nil, errors.Is(rerr, fs.ErrNotExist):
		return nil
	case inUse(rerr):
		return ErrBusy
	}
	return rerr
}
