package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// output_replace_test.go - an output is written aside and moved onto its name
// once complete: a failed operation leaves the file that was there as it was,
// and nothing else behind.

// dirNames lists the names in dir, sorted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// TestFailedOutputKeepsExistingFile: an operation that fails mid-write leaves
// a file already under its output name byte for byte, and no temporary file.
func TestFailedOutputKeepsExistingFile(t *testing.T) {
	ctx := context.Background()
	before := []byte("an earlier result")
	damaged := func(t *testing.T, dir string, o livefixture.Options) string {
		src := filepath.Join(dir, "damaged.mkv")
		writeAll(t, src, livefixture.Build(o))
		return src
	}
	overrun := livefixture.Options{Overrun: true, PayloadBytes: 4096}

	cases := []struct {
		name string
		run  func(t *testing.T, dir, dst string) error
	}{
		{"Mux", func(t *testing.T, dir, dst string) error {
			src := damaged(t, dir, overrun)
			return Mux(ctx, mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: livefixture.VideoTrack}}})
		}},
		{"RemoveTrack", func(t *testing.T, dir, dst string) error {
			return RemoveTrack(ctx, damaged(t, dir, overrun), dst, []uint64{2})
		}},
		{"Reindex", func(t *testing.T, dir, dst string) error {
			o := overrun
			o.SizedClusters = true
			return Reindex(ctx, damaged(t, dir, o), dst)
		}},
		{"RemuxToWebM", func(t *testing.T, dir, dst string) error {
			tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Language: "eng", Width: u32(320), Height: u32(240)}}
			sets := make([][]mkv.Block, 0, 4)
			for i := 0; i < 4; i++ {
				sets = append(sets, []mkv.Block{{TrackNumber: 1, Timecode: int64(i * 1000), Keyframe: true, Data: bytes.Repeat([]byte{0x55}, 4096)}})
			}
			wholePath := buildMultiClusterMKV(t, dir, "whole.mkv", tracks, sets, 4000)
			whole, err := os.ReadFile(wholePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(wholePath); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(dir, "cut.mkv")
			writeAll(t, src, whole[:len(whole)/2])
			return RemuxToWebM(ctx, src, dst)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "existing.mkv")
			writeAll(t, dst, before)
			if err := tc.run(t, dir, dst); err == nil {
				t.Fatal("want the damaged source refused, got success")
			}
			after, err := os.ReadFile(dst)
			if err != nil || !bytes.Equal(after, before) {
				t.Errorf("the existing output was not kept (read err %v, %q)", err, after)
			}
			for _, name := range dirNames(t, dir) {
				if name != "existing.mkv" && name != "damaged.mkv" && name != "cut.mkv" {
					t.Errorf("the failed operation left %s behind", name)
				}
			}
		})
	}

	t.Run("Demux", func(t *testing.T) {
		good := t.TempDir()
		src := filepath.Join(good, "good.mkv")
		writeAll(t, src, livefixture.Build(livefixture.Options{PayloadBytes: 4096}))
		if err := Demux(ctx, mkv.DemuxOptions{SourcePath: src, OutputDir: filepath.Join(good, "out")}); err != nil {
			t.Fatalf("demux of the whole source: %v", err)
		}
		names := dirNames(t, filepath.Join(good, "out"))

		dir := t.TempDir()
		out := filepath.Join(dir, "out")
		if err := os.Mkdir(out, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			writeAll(t, filepath.Join(out, n), before)
		}
		if err := Demux(ctx, mkv.DemuxOptions{SourcePath: damaged(t, dir, overrun), OutputDir: out}); err == nil {
			t.Fatal("want the damaged source refused, got success")
		}
		if got := dirNames(t, out); !slices.Equal(got, names) {
			t.Errorf("track files after the failed demux = %v, want %v", got, names)
		}
		for _, n := range names {
			if after, err := os.ReadFile(filepath.Join(out, n)); err != nil || !bytes.Equal(after, before) {
				t.Errorf("%s was not kept (read err %v, %q)", n, err, after)
			}
		}
	})
}

// TestOutputReplacesExistingFile: a complete output replaces the file under
// its name, keeps that file's permissions, and leaves no temporary file.
func TestOutputReplacesExistingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := buildTestMKV(t, dir)
	fresh := filepath.Join(dir, "fresh.mkv")
	if err := Reindex(ctx, src, fresh); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "existing.mkv")
	writeAll(t, dst, []byte("an earlier result"))
	if err := os.Chmod(dst, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(fresh)
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("the output over an existing file differs from a fresh one (read err %v, %d bytes, want %d)", err, len(got), len(want))
	}
	if st, err := os.Stat(dst); err == nil && runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %v, want the existing file's 0600", st.Mode().Perm())
	}
	if names, want := dirNames(t, dir), []string{"existing.mkv", "fresh.mkv", filepath.Base(src)}; !slices.Equal(names, sortedCopy(want)) {
		t.Errorf("files after the operation = %v, want %v", names, sortedCopy(want))
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// TestOutputTempName: the output is written to dst+".mkvgo.tmp", a leftover of
// an earlier run under that name is replaced, and a link there is refused, not
// written through.
func TestOutputTempName(t *testing.T) {
	ctx := context.Background()

	t.Run("leftover replaced", func(t *testing.T) {
		dir := t.TempDir()
		src := buildTestMKV(t, dir)
		dst := filepath.Join(dir, "out.mkv")
		writeAll(t, dst+".mkvgo.tmp", []byte("left by a killed run"))
		if err := Reindex(ctx, src, dst); err != nil {
			t.Fatal(err)
		}
		if names, want := dirNames(t, dir), sortedCopy([]string{"out.mkv", filepath.Base(src)}); !slices.Equal(names, want) {
			t.Errorf("files after the operation = %v, want %v", names, want)
		}
	})

	t.Run("link refused", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs a privilege on Windows")
		}
		dir := t.TempDir()
		src := buildTestMKV(t, dir)
		dst := filepath.Join(dir, "out.mkv")
		before := []byte("an earlier result")
		writeAll(t, dst, before)
		victim := filepath.Join(dir, "victim")
		writeAll(t, victim, []byte("not the output's"))
		if err := os.Symlink(victim, dst+".mkvgo.tmp"); err != nil {
			t.Fatal(err)
		}
		err := Reindex(ctx, src, dst)
		if err == nil || !strings.Contains(err.Error(), "remove it") {
			t.Fatalf("want the link refused with what to do, got %v", err)
		}
		if got, _ := os.ReadFile(victim); string(got) != "not the output's" {
			t.Errorf("the output was written through the link: %q", got)
		}
		if got, _ := os.ReadFile(dst); !bytes.Equal(got, before) {
			t.Errorf("the existing output was not kept: %q", got)
		}
		if _, err := os.Lstat(dst + ".mkvgo.tmp"); err != nil {
			t.Errorf("the link was removed: %v", err)
		}
	})
}
