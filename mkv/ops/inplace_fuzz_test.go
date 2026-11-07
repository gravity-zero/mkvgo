package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// FuzzEditInPlace pins the in-place contract on any file the reader opens: the edit is refused with the file as it
// was, or it lands and the file opens again with the new title and every block it had.
func FuzzEditInPlace(f *testing.F) {
	for _, tags := range [][]mkv.Tag{nil, movieTags("seed")} {
		data, err := tightHeadBytes(tags)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, uint16(4), true)
		f.Add(data, uint16(300), false)
		f.Add(append(bytes.Clone(data), 0xEC, 0x81, 0x00), uint16(300), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, titleLen uint16, addTag bool) {
		path := filepath.Join(t.TempDir(), "f.mkv")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		blocks, ok := countBlocks(path)
		if !ok {
			return
		}
		title := strings.Repeat("t", int(titleLen%4096)+1)
		err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
			c.Info.Title = title
			if addTag {
				c.Tags = append(c.Tags, movieTags("added")...)
			}
		})
		after, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if err != nil {
			if !bytes.Equal(data, after) {
				t.Fatalf("refused (%v) but the file changed", err)
			}
			return
		}
		c, err := reader.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("edited file no longer opens: %v", err)
		}
		if c.Info.Title != title {
			t.Fatalf("title %q after the edit, want %d characters", c.Info.Title, len(title))
		}
		if n, ok := countBlocks(path); !ok || n != blocks {
			t.Fatalf("%d blocks after the edit (readable %v), had %d", n, ok, blocks)
		}
	})
}

// countBlocks counts a file's blocks, false when the reader refuses the file or a block.
func countBlocks(path string) (int, bool) {
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return 0, false
	}
	n := 0
	for {
		_, err := br.Next()
		if errors.Is(err, io.EOF) {
			return n, true
		}
		if err != nil {
			return n, false
		}
		n++
	}
}
