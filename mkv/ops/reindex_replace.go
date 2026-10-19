package ops

import (
	"context"
	"errors"
	"fmt"
	"os"

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
	fs := mkv.FSFrom(opts)
	tmp := path + ".mkvgo.tmp"

	if _, err := fs.DoStat(tmp); err == nil {
		return fmt.Errorf("reindex replace: leftover temporary file %s exists; remove it first", tmp)
	}
	if err := backupFree(fs, path, mkv.KeepBackupFrom(opts)); err != nil {
		return err
	}

	if err := Reindex(ctx, path, tmp, opts...); err != nil {
		_ = fs.DoRemove(tmp) // best-effort cleanup; original is untouched either way
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
	if !keepBackup {
		if err := fs.DoRename(tmp, path); err != nil {
			_ = fs.DoRemove(tmp) // do not leave a blocker for the next run
			return fmt.Errorf("replace: install verified copy: %w", err)
		}
		return nil
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
