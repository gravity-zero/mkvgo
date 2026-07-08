package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ReindexReplace rebuilds path's seek index through a temporary copy in the
// same directory, verifies the result (always the light check, plus the deep
// pass when Options.DeepVerify is set), and only then atomically renames the
// copy over the original. The original is never touched until every check has
// passed. Options.KeepBackup preserves it as path+".bak". Needs write
// permission on the directory (temp file + rename).
func ReindexReplace(ctx context.Context, path string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	tmp := path + ".mkvgo.tmp"

	if _, err := fs.DoStat(tmp); err == nil {
		return fmt.Errorf("reindex replace: leftover temporary file %s exists; remove it first", tmp)
	}

	if err := Reindex(ctx, path, tmp, opts...); err != nil {
		_ = fs.DoRemove(tmp) // best-effort cleanup; original is untouched either way
		return err
	}

	if !mkv.KeepBackupFrom(opts) {
		if err := fs.DoRename(tmp, path); err != nil {
			_ = fs.DoRemove(tmp) // do not leave a blocker for the next run
			return fmt.Errorf("reindex replace: install verified copy: %w", err)
		}
		return nil
	}

	backup := path + ".bak"
	if err := fs.DoRename(path, backup); err != nil {
		_ = fs.DoRemove(tmp)
		return fmt.Errorf("reindex replace: back up original: %w", err)
	}
	if err := fs.DoRename(tmp, path); err != nil {
		installErr := fmt.Errorf("reindex replace: install verified copy: %w", err)
		if rerr := fs.DoRename(backup, path); rerr != nil {
			return errors.Join(installErr, fmt.Errorf("reindex replace: restore original from backup: %w", rerr))
		}
		_ = fs.DoRemove(tmp) // original restored; drop the copy so the next run is not blocked
		return installErr
	}
	return nil
}
