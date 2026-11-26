// Package dovitest builds a dual-layer Dolby Vision profile 7 Matroska file for tests: the base layer of a real HEVC
// fixture, each picture followed by a wrapped enhancement layer NAL unit and a profile 7 RPU.
package dovitest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// Fixture is a profile 7 file and what each conversion must leave of its video samples.
type Fixture struct {
	Path      string
	Track     mkv.Track
	Base      [][]byte // the base layer alone
	Profile81 [][]byte // the base layer and the RPU converted
}

// Profile7 writes the fixture under dir; full says whether its enhancement layer is a full one.
func Profile7(tb testing.TB, dir string, full bool) *Fixture {
	tb.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	rpu := dovi.NAL(RPU(full))[2:] // the RPU, escaped
	src := filepath.Join(root, "testdata", "hevc_slices.mkv")
	c, err := reader.Open(context.Background(), src)
	if err != nil {
		tb.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		tb.Fatal(err)
	}
	fx := &Fixture{Path: filepath.Join(dir, "profile7.mkv")}
	for _, t := range c.Tracks {
		if t.Type == mkv.VideoTrack {
			fx.Track = t
		}
	}
	fx.Track.DolbyVision = &mkv.DolbyVision{VersionMajor: 1, Profile: 7, Level: 6, RPUPresent: true, ELPresent: true, BLPresent: true, BLSignalCompatID: 6}
	lenSize := int(fx.Track.CodecPrivate[21]&3) + 1
	unit := func(nal []byte) []byte {
		out := make([]byte, lenSize, lenSize+len(nal))
		for i := range lenSize {
			out[i] = byte(len(nal) >> (8 * (lenSize - 1 - i)))
		}
		return append(out, nal...)
	}
	converted := RPU81(full)
	var blocks []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tb.Fatal(err)
		}
		if b.TrackNumber != fx.Track.ID {
			continue
		}
		base := append([]byte(nil), b.Data...)
		fx.Base = append(fx.Base, base)
		fx.Profile81 = append(fx.Profile81, append(append([]byte(nil), base...), unit(dovi.NAL(converted))...))
		el := unit([]byte{63 << 1, 1, 0x40, 0x01, byte(len(blocks)), 0xAA})
		b.Data = append(append(append([]byte(nil), base...), el...), unit(append([]byte{62 << 1, 1}, rpu...))...)
		blocks = append(blocks, b)
	}
	out, err := os.Create(fx.Path)
	if err != nil {
		tb.Fatal(err)
	}
	defer out.Close()
	mw := writer.NewMKVWriter(out)
	meta := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: c.Info.TimecodeScale, MuxingApp: "test", WritingApp: "test"}}
	if err := errors.Join(mw.WriteStart(), mw.WriteMetadata(meta, []mkv.Track{fx.Track}, c.DurationMs)); err != nil {
		tb.Fatal(err)
	}
	for len(blocks) > 0 {
		n := 1
		for n < len(blocks) && !blocks[n].Keyframe {
			n++
		}
		if err := mw.WriteClusterWithCues(blocks[0].Timecode, c.Info.TimecodeScale, blocks[:n]); err != nil {
			tb.Fatal(err)
		}
		blocks = blocks[n:]
	}
	if err := mw.Finalize(); err != nil {
		tb.Fatal(err)
	}
	return fx
}
