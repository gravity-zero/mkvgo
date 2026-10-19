package ops

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestReplaceKeepsAnEarlierBackup: a --keep-backup run never overwrites the .bak an earlier run left, the original it holds.
func TestReplaceKeepsAnEarlierBackup(t *testing.T) {
	dir := t.TempDir()
	path := buildMinimalMKV(t, dir, "a.mkv", []mkv.Track{videoTrack(1)}, testBlocks(1), 200)
	if err := os.WriteFile(path+".bak", []byte("the real original"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	err := ReindexReplace(context.Background(), path, mkv.Options{KeepBackup: true})
	if err == nil || !strings.Contains(err.Error(), ".bak") {
		t.Errorf("err = %v, want a refusal naming the backup", err)
	}
	if got, _ := os.ReadFile(path + ".bak"); !bytes.Equal(got, []byte("the real original")) {
		t.Error("the earlier backup was overwritten")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
		t.Error("the file changed under a refused replace")
	}
}

// syncSpy is a file that records being synced.
type syncSpy struct {
	*os.File
	synced map[string]bool
}

func (s syncSpy) Sync() error {
	s.synced[s.Name()] = true
	return s.File.Sync()
}

// TestReplaceFlushesTheCopyBeforeRenamingIt: the verified copy reaches the disk before it takes the original's name.
func TestReplaceFlushesTheCopyBeforeRenamingIt(t *testing.T) {
	dir := t.TempDir()
	path := buildMinimalMKV(t, dir, "a.mkv", []mkv.Track{videoTrack(1)}, testBlocks(1), 200)
	synced := map[string]bool{}
	var unsynced []string
	fs := &mkv.FS{
		OpenFile: func(p string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
			f, err := os.OpenFile(p, flag, perm)
			if err != nil {
				return nil, err
			}
			return syncSpy{f, synced}, nil
		},
		Rename: func(oldpath, newpath string) error {
			if newpath == path && !synced[oldpath] {
				unsynced = append(unsynced, oldpath)
			}
			return os.Rename(oldpath, newpath)
		},
	}
	if err := ReindexReplace(context.Background(), path, mkv.Options{FS: fs}); err != nil {
		t.Fatal(err)
	}
	if len(unsynced) > 0 {
		t.Errorf("renamed over the original without a sync: %v", unsynced)
	}
}
