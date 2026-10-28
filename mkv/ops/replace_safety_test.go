package ops

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestReplaceKeepsModeAndLink: a replace installed a copy created at 0666 less the umask, so a file of mode 0640 came
// back 0644, and renamed it over a symbolic link, so the link became a plain file beside the original it named.
func TestReplaceKeepsModeAndLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes and symbolic links are POSIX")
	}
	for _, op := range []struct {
		name string
		run  func(path string) error
	}{
		{"ReindexReplace", func(p string) error { return ReindexReplace(context.Background(), p) }},
		{"RetimeTracksReplace", func(p string) error {
			return RetimeTracksReplace(context.Background(), p, map[uint64]int64{1: 40_000_000})
		}},
	} {
		dir := t.TempDir()
		target := filepath.Join(dir, "film.mkv")
		tailTagsMKV(t, target, 2)
		mustNil(t, os.Chmod(target, 0o640))
		link := filepath.Join(dir, "link.mkv")
		mustNil(t, os.Symlink(target, link))
		if err := op.run(link); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
		if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: the link is no longer a link (%v)", op.name, err)
		}
		if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o640 {
			t.Errorf("%s: the file's mode is %v (%v), want 0640", op.name, st.Mode().Perm(), err)
		}
		if _, err := os.Stat(target + ".mkvgo.tmp"); !os.IsNotExist(err) {
			t.Errorf("%s: the temporary copy was left: %v", op.name, err)
		}
	}
}

// TestReplaceTempIsCreatedExclusively: the leftover check stats the temporary name, which a dangling link passes
// (it names nothing), and the create then followed the link and wrote where it pointed. The replace is refused and
// nothing is written there.
func TestReplaceTempIsCreatedExclusively(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links are POSIX")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "film.mkv")
	tailTagsMKV(t, path, 2)
	victim := filepath.Join(dir, "elsewhere.bin")
	mustNil(t, os.Symlink(victim, path+".mkvgo.tmp"))
	if err := ReindexReplace(context.Background(), path); err == nil {
		t.Error("the replace wrote through a link planted at its temporary name")
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("the link's target was written: %v", err)
	}
	if _, err := os.Lstat(path + ".mkvgo.tmp"); err != nil {
		t.Errorf("the replace removed the link it did not create: %v", err)
		mustNil(t, os.Symlink(victim, path+".mkvgo.tmp"))
	}
	if err := RetimeTracksReplace(context.Background(), path, map[uint64]int64{1: 40}, mkv.Options{}); err == nil {
		t.Error("the retime replace wrote through a link planted at its temporary name")
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("the link's target was written by the retime: %v", err)
	}
}
