package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// dyingFile lets budget write calls reach the file whole, then acts as a process that died: nothing else is written, truncated or synced.
type dyingFile struct {
	*os.File
	budget *int64
	died   *bool
}

var errDied = errors.New("process died")

func (d dyingFile) Write(p []byte) (int, error) {
	if *d.budget <= 0 {
		*d.died = true
	}
	if *d.died {
		return 0, errDied
	}
	*d.budget--
	return d.File.Write(p)
}

func (d dyingFile) Truncate(size int64) error {
	if *d.died {
		return errDied
	}
	return d.File.Truncate(size)
}

func (d dyingFile) Sync() error {
	if *d.died {
		return errDied
	}
	return d.File.Sync()
}

func dyingFS(budget *int64) *mkv.FS {
	died := new(bool)
	return &mkv.FS{OpenFile: func(path string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
		f, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return dyingFile{File: f, budget: budget, died: died}, nil
	}}
}

// TestNEW04_ReindexDiesAtEveryWrite kills ReindexInPlace before each of its write calls and recovers: the file must come back to the original or to the finished result.
func TestNEW04_ReindexDiesAtEveryWrite(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
		{{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 3000)
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work := filepath.Join(dir, "work.mkv")
	dieAtEveryWrite(t, work, orig, func(fs *mkv.FS) error { return ReindexInPlace(ctx, work, mkv.Options{FS: fs}) })
}

// dieAtEveryWrite runs op once whole, then once per write call with the process dying before it, recovering after each.
func dieAtEveryWrite(t *testing.T, work string, orig []byte, op func(*mkv.FS) error) {
	t.Helper()
	ctx := context.Background()
	if err := os.WriteFile(work, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	unlimited := int64(1 << 40)
	if err := op(dyingFS(&unlimited)); err != nil {
		t.Fatal(err)
	}
	done, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	total := int64(1<<40) - unlimited
	counts := map[string]int{}
	for b := int64(0); b <= total; b++ {
		if err := os.WriteFile(work, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		budget := b
		_ = op(dyingFS(&budget))
		for k := int64(0); k < 3; k++ { // a recovery that dies too, before its write k+1
			rb := k
			_, _ = RecoverInPlace(ctx, work, mkv.Options{FS: dyingFS(&rb)})
		}
		if _, err := RecoverInPlace(ctx, work); err != nil {
			t.Fatalf("death before write %d of %d: recover: %v", b+1, total, err)
		}
		got, err := os.ReadFile(work)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case bytes.Equal(got, orig):
			counts["original"]++
		case bytes.Equal(got, done):
			counts["finished"]++
		default:
			t.Errorf("death before write %d of %d: %d bytes after recovery, neither the original (%d) nor the finished file (%d)", b+1, total, len(got), len(orig), len(done))
		}
	}
	if counts["original"] == 0 || counts["finished"] == 0 {
		t.Errorf("outcomes %v: the deaths did not cover both sides", counts)
	}
}

// TestNEW04_RetimeDiesAtEveryWrite is the same for RetimeTracksInPlace.
func TestNEW04_RetimeDiesAtEveryWrite(t *testing.T) {
	dir := t.TempDir()
	src := delayedAudioFixture(t, dir, "src.mkv", 4, 900)
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work.mkv")
	dieAtEveryWrite(t, work, orig, func(fs *mkv.FS) error {
		return RetimeTracksInPlace(context.Background(), work, map[uint64]int64{2: -900_000_000}, mkv.Options{FS: fs})
	})
}

// tornFile lets budget bytes reach the file, the write that crosses it torn there, then acts as a machine that lost power: nothing else lands.
type tornFile struct {
	*os.File
	budget *int64
	died   *bool
}

func (d tornFile) Write(p []byte) (int, error) {
	if *d.died {
		return 0, errDied
	}
	if int64(len(p)) > *d.budget {
		n, _ := d.File.Write(p[:*d.budget])
		*d.budget, *d.died = 0, true
		return n, errDied
	}
	*d.budget -= int64(len(p))
	return d.File.Write(p)
}

func (d tornFile) Truncate(size int64) error {
	if *d.died {
		return errDied
	}
	return d.File.Truncate(size)
}

func (d tornFile) Sync() error {
	if *d.died {
		return errDied
	}
	return d.File.Sync()
}

func tornFS(budget *int64) *mkv.FS {
	died := new(bool)
	return &mkv.FS{OpenFile: func(path string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
		f, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
		return tornFile{File: f, budget: budget, died: died}, nil
	}}
}

// TestNEW04_ReindexTornAtEveryByte cuts ReindexInPlace's writes at every byte, a write torn where the power went, and recovers: the file must come back to the original or to the finished result.
func TestNEW04_ReindexTornAtEveryByte(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
		{{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 3000)
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work := filepath.Join(dir, "work.mkv")
	if err := os.WriteFile(work, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	unlimited := int64(1 << 40)
	if err := ReindexInPlace(ctx, work, mkv.Options{FS: tornFS(&unlimited)}); err != nil {
		t.Fatal(err)
	}
	done, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	total := int64(1<<40) - unlimited
	counts := map[string]int{}
	for b := int64(0); b <= total; b++ {
		if err := os.WriteFile(work, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		budget := b
		_ = ReindexInPlace(ctx, work, mkv.Options{FS: tornFS(&budget)})
		if _, err := RecoverInPlace(ctx, work); err != nil {
			t.Fatalf("torn at byte %d of %d: recover: %v", b, total, err)
		}
		got, err := os.ReadFile(work)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case bytes.Equal(got, orig):
			counts["original"]++
		case bytes.Equal(got, done):
			counts["finished"]++
		default:
			t.Errorf("torn at byte %d of %d: %d bytes after recovery, neither the original (%d) nor the finished file (%d)", b, total, len(got), len(orig), len(done))
		}
	}
	if counts["original"] == 0 || counts["finished"] == 0 {
		t.Errorf("outcomes %v: the cuts did not cover both sides", counts)
	}
}
