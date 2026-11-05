package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// groupWithAdditions is a Cluster holding one BlockGroup: its BlockAdditions (raw body) before a 1-byte Block.
func groupWithAdditions(additions []byte) *bytes.Reader {
	var g bytes.Buffer
	g.Write(wrapEl(mkv.IDBlockAdditions, additions))
	g.Write(wrapEl(mkv.IDBlock, []byte{0x81, 0x00, 0x00, 0x00, 0xAA}))
	var c bytes.Buffer
	c.Write(uintEl(mkv.IDTimestamp, 0))
	c.Write(wrapEl(mkv.IDBlockGroup, g.Bytes()))
	return buildBlockReaderInput(c.Bytes())
}

// refusedNoPanic reads the first block and wants it refused, a panic reported as a failure.
func refusedNoPanic(t *testing.T, name string, data *bytes.Reader) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: Next panicked: %v", name, r)
		}
	}()
	br, err := NewBlockReader(data, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := br.Next(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("%s: %v, want the block refused", name, err)
	}
}

// TestBlockAdditionalOfUnknownSize: a BlockAdditional of unknown size (-1) passed the 16 MiB check and reached
// make([]byte, -1), a panic on every walk of the file.
func TestBlockAdditionalOfUnknownSize(t *testing.T) {
	more := []byte{0xA6, 0x82, 0xA5, 0xFF} // BlockMore(2) > BlockAdditional of unknown size
	refusedNoPanic(t, "unknown-size BlockAdditional", groupWithAdditions(more))
}

// TestBlockAdditionsAreBounded: one BlockGroup kept every BlockMore its file held - 2 bytes on disk each, 32 in
// memory, and up to 16 MiB each with data - with no cap past the single BlockAdditional's.
func TestBlockAdditionsAreBounded(t *testing.T) {
	refusedNoPanic(t, "empty BlockMore past the count cap",
		groupWithAdditions(bytes.Repeat([]byte{0xA6, 0x80}, maxBlockMores+1)))

	big := wrapEl(mkv.IDBlockMore, wrapEl(mkv.IDBlockAdditional, make([]byte, maxBlockAdditional/2+1)))
	refusedNoPanic(t, "additions past the byte cap", groupWithAdditions(bytes.Repeat(big, 2)))

	within := wrapEl(mkv.IDBlockMore, wrapEl(mkv.IDBlockAdditional, []byte{1, 2, 3}))
	br, err := NewBlockReader(groupWithAdditions(bytes.Repeat(within, 4)), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := br.Next(); err != nil || len(b.Additions) != 4 {
		t.Errorf("4 small additions: %d read, %v", len(b.Additions), err)
	}
}
