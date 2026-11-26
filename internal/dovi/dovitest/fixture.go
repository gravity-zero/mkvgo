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

// Spec says which profile 7 file Build writes; the zero value is an SDR base under a minimal enhancement layer.
type Spec struct {
	Full   bool   // the enhancement layer is a full one
	HDR    bool   // the track carries an HDR10 description: PQ, BT.2020, mastering display, light levels
	Frames int    // stop after this many frames; 0 keeps them all
	Pad    int    // bytes of filler NAL unit opening the first frame, ahead of the base layer
	Trail  int    // bytes of filler NAL unit closing the first frame, behind the RPU
	RPU    []byte // the RPU of every frame in place of the generated one, unescaped
}

// Profile7 writes the SDR fixture under dir; full says whether its enhancement layer is a full one.
func Profile7(tb testing.TB, dir string, full bool) *Fixture {
	tb.Helper()
	return Build(tb, dir, Spec{Full: full})
}

// Build writes the fixture s describes under dir.
func Build(tb testing.TB, dir string, s Spec) *Fixture {
	tb.Helper()
	full := s.Full
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	if s.RPU == nil {
		s.RPU = RPU(full)
	}
	rpu := dovi.NAL(s.RPU)[2:] // the RPU, escaped
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
	if s.HDR {
		pq, bt2020, limited := uint16(16), uint16(9), uint16(1)
		fx.Track.ColorTransfer, fx.Track.ColorPrimaries, fx.Track.ColorSpace, fx.Track.ColorRange = &pq, &bt2020, &bt2020, &limited
		fx.Track.HDR = &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400, MasteringDisplay: &mkv.MasteringDisplay{
			RedX: 0.68, RedY: 0.32, GreenX: 0.265, GreenY: 0.69, BlueX: 0.15, BlueY: 0.06, WhiteX: 0.3127, WhiteY: 0.329, LuminanceMax: 1000, LuminanceMin: 0.005}}
	}
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
		if s.Frames > 0 && len(blocks) == s.Frames {
			break
		}
		base := append([]byte(nil), b.Data...)
		if len(blocks) == 0 && s.Pad > 0 {
			base = append(unit(filler(s.Pad)), base...)
		}
		fx.Base = append(fx.Base, base)
		fx.Profile81 = append(fx.Profile81, append(append([]byte(nil), base...), unit(dovi.NAL(converted))...))
		el := unit([]byte{63 << 1, 1, 0x40, 0x01, byte(len(blocks)), 0xAA})
		b.Data = append(append(append([]byte(nil), base...), el...), unit(append([]byte{62 << 1, 1}, rpu...))...)
		if len(blocks) == 0 && s.Trail > 0 {
			b.Data = append(b.Data, unit(filler(s.Trail))...)
			fx.Base[0] = append(fx.Base[0], unit(filler(s.Trail))...)
			fx.Profile81[0] = append(fx.Profile81[0], unit(filler(s.Trail))...)
		}
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

// filler returns a filler data NAL unit of n bytes.
func filler(n int) []byte {
	nal := append(make([]byte, 0, n), 38<<1, 1)
	for len(nal) < n-1 {
		nal = append(nal, 0xFF)
	}
	return append(nal, 0x80)
}
