package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/internal/tmplock"
	"github.com/gravity-zero/mkvgo/mkv"
)

// ReindexReplace rebuilds path's seek index through a temporary copy in the
// same directory, verifies the result (always the light check, plus the deep
// pass when Options.DeepVerify is set), and only then atomically renames the
// copy over the original. The original is never touched until every check has
// passed. Options.KeepBackup preserves it as path+".bak". Needs write
// permission on the directory (temp file + rename). Options.Resync (tolerate
// and skip corrupted regions) applies here too - see Reindex.
func ReindexReplace(ctx context.Context, path string, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	fs := mkv.FSFrom(opts)
	path, err := replaceTarget(fs, path)
	if err != nil {
		return fmt.Errorf("reindex replace: %w", err)
	}
	tmp := path + tempSuffix
	if err := clearLeftover(fs, "reindex replace", path, tmp); err != nil {
		return err
	}
	if err := backupFree(fs, path, mkv.KeepBackupFrom(opts)); err != nil {
		return err
	}

	xopts := append([]mkv.Options(nil), opts...)
	if len(xopts) == 0 {
		xopts = []mkv.Options{{}}
	}
	var hold tempHold
	defer hold.release()
	xopts[0].FS = exclusiveTemp(fs, path, tmp, &hold)
	if err := Reindex(ctx, path, tmp, xopts...); err != nil {
		if !notOurs(err) { // what stood at tmp is not this run's to remove
			_ = fs.DoRemove(tmp) // best-effort cleanup; original is untouched either way
		}
		return err
	}

	return installReplacement(fs, tmp, path, mkv.KeepBackupFrom(opts))
}

// installReplacement atomically swaps a verified temporary copy over the
// original, optionally keeping the original as path+".bak". Shared by
// ReindexReplace and RetimeTracksReplace.
func installReplacement(fs *mkv.FS, tmp, path string, keepBackup bool) error {
	if err := backupFree(fs, path, keepBackup); err != nil {
		_ = fs.DoRemove(tmp)
		return err
	}
	if err := syncCopy(fs, tmp); err != nil {
		_ = fs.DoRemove(tmp)
		return fmt.Errorf("replace: flush verified copy: %w", err)
	}
	// The mode comes last: a read-only original's mode would keep the copy from being reopened to flush it.
	if err := keepMode(fs, path, tmp); err != nil {
		_ = fs.DoRemove(tmp)
		return fmt.Errorf("replace: give the copy the original's mode: %w", err)
	}
	if !keepBackup {
		if err := fs.DoRename(tmp, path); err != nil {
			_ = fs.DoRemove(tmp) // do not leave a blocker for the next run
			return fmt.Errorf("replace: install verified copy: %w", err)
		}
		return syncDir(fs, path)
	}

	backup := path + ".bak"
	if err := fs.DoRename(path, backup); err != nil {
		_ = fs.DoRemove(tmp)
		return fmt.Errorf("replace: back up original: %w", err)
	}
	if err := fs.DoRename(tmp, path); err != nil {
		installErr := fmt.Errorf("replace: install verified copy: %w", err)
		if rerr := fs.DoRename(backup, path); rerr != nil {
			return errors.Join(installErr, fmt.Errorf("replace: restore original from backup: %w", rerr))
		}
		_ = fs.DoRemove(tmp) // original restored; drop the copy so the next run is not blocked
		return installErr
	}
	return syncDir(fs, path)
}

// osFS reports whether fs is the operating system's: the mode, link and directory steps below apply to it only.
func osFS(fs *mkv.FS) bool {
	return fs == nil || fs.Open == nil && fs.Create == nil && fs.OpenFile == nil && fs.Rename == nil
}

// replaceTarget is the file a replace rewrites: the file a symbolic link names, so the link stays a link to the
// repaired file instead of becoming a copy beside the original.
func replaceTarget(fs *mkv.FS, path string) (string, error) {
	if !osFS(fs) {
		return path, nil
	}
	return filepath.EvalSymlinks(path)
}

// clearLeftover readies a replace's temporary name: on the OS a file a killed run left there is removed and one a
// live run holds refused (internal/tmplock); on another FS any file there is refused.
func clearLeftover(fs *mkv.FS, op, path, tmp string) error {
	if !osFS(fs) {
		if _, err := fs.DoStat(tmp); err == nil {
			return fmt.Errorf("%s: leftover temporary file %s exists; remove it first", op, tmp)
		}
		return nil
	}
	if st, err := os.Lstat(tmp); err != nil || !st.Mode().IsRegular() {
		return nil // nothing, or what the exclusive create refuses
	}
	if err := tmplock.Clear(tmp); err != nil {
		return fmt.Errorf("%s: %w", op, busy(path, err))
	}
	return nil
}

// notOurs reports a failure to create the temporary file because something else stands there: not this run's to
// remove.
func notOurs(err error) bool { return errors.Is(err, os.ErrExist) || errors.Is(err, mkv.ErrOutputBusy) }

// tempHold is the lock on a replace's temporary copy, held until the replace is over.
type tempHold struct{ lock *tmplock.Lock }

func (h *tempHold) release() { h.lock.Release() }

// exclusiveTemp is fs creating tmp only where nothing stands, a dangling link included: a plain create follows a
// link planted at tmp and writes where it points. The copy is its owner's alone while it is written (0600), the
// operation reopening it to finish it; installReplacement gives it the original's mode once complete. The copy is
// locked into hold, so a second run aiming at path is refused.
func exclusiveTemp(fs *mkv.FS, path, tmp string, hold *tempHold) *mkv.FS {
	if !osFS(fs) {
		return fs
	}
	return &mkv.FS{Create: func(p string) (mkv.WriteSeekCloser, error) {
		if p != tmp {
			return os.Create(p)
		}
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			if st, serr := os.Lstat(p); serr == nil && st.Mode().IsRegular() {
				return nil, busy(path, tmplock.ErrBusy) // another run created it since the leftover check
			}
		}
		if err != nil {
			return nil, err
		}
		if hold.lock, err = tmplock.Take(p, f); err != nil {
			_ = f.Close()
			return nil, busy(path, err)
		}
		return f, nil
	}}
}

// keepMode gives the copy the original's permission bits, which the process umask cut at its creation.
func keepMode(fs *mkv.FS, path, tmp string) error {
	if !osFS(fs) {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.Chmod(tmp, st.Mode().Perm())
}

// syncDir flushes the directory holding path, so the rename that installed the copy survives a crash (not on
// Windows, whose directories take no flush).
func syncDir(fs *mkv.FS, path string) error {
	if !osFS(fs) || runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("replace: flush the directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("replace: flush the directory: %w", err)
	}
	return nil
}

// backupFree refuses a replace that keeps a backup when path+".bak" already holds one: an earlier run's original.
func backupFree(fs *mkv.FS, path string, keepBackup bool) error {
	if !keepBackup {
		return nil
	}
	if _, err := fs.DoStat(path + ".bak"); err == nil {
		return fmt.Errorf("replace: %s.bak already holds the original an earlier run kept: move it away, then run again", path)
	}
	return nil
}

// syncCopy flushes the copy to the disk before it takes the original's name, where the FS can reopen it (the OS, or an OpenFile).
func syncCopy(fs *mkv.FS, tmp string) error {
	if fs != nil && fs.OpenFile == nil && (fs.Create != nil || fs.Open != nil) {
		return nil // a custom store finalises its own writes
	}
	f, err := fs.DoOpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if s, ok := f.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}
