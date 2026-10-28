package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
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
	tmp := path + ".mkvgo.tmp"

	if _, err := fs.DoStat(tmp); err == nil {
		return fmt.Errorf("reindex replace: leftover temporary file %s exists; remove it first", tmp)
	}
	if err := backupFree(fs, path, mkv.KeepBackupFrom(opts)); err != nil {
		return err
	}

	xopts := append([]mkv.Options(nil), opts...)
	if len(xopts) == 0 {
		xopts = []mkv.Options{{}}
	}
	xopts[0].FS = exclusiveTemp(fs, path, tmp)
	if err := Reindex(ctx, path, tmp, xopts...); err != nil {
		if !errors.Is(err, os.ErrExist) { // what stood at tmp is not this run's to remove
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
	if err := keepMode(fs, path, tmp); err != nil {
		_ = fs.DoRemove(tmp)
		return fmt.Errorf("replace: give the copy the original's mode: %w", err)
	}
	if err := syncCopy(fs, tmp); err != nil {
		_ = fs.DoRemove(tmp)
		return fmt.Errorf("replace: flush verified copy: %w", err)
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

// exclusiveTemp is fs creating tmp only where nothing stands, a dangling link included, with path's mode: a plain
// create follows a link planted at tmp and writes where it points.
func exclusiveTemp(fs *mkv.FS, path, tmp string) *mkv.FS {
	if !osFS(fs) {
		return fs
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return &mkv.FS{Create: func(p string) (mkv.WriteSeekCloser, error) {
		if p != tmp {
			return os.Create(p)
		}
		return os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
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
