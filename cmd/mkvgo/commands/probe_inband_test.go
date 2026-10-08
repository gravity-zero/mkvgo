package commands_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// A real HEVC Main 10 hvcC with its SPS in-record (PQ / bt2020nc).
const hevcHDRPrivateHex = "0102200000009000000000001ef000fcfdfafa00000f03200001001840010c01ffff02200000030090000003000003001e959809210001002d42010102200000030090000003000003001ea0208104d96566924caf016a12201208000003000800000300c04022000100074401c172b42240"

// hdr10SEINAL is a prefix SEI NAL carrying mastering_display_colour_volume (137)
// and content_light_level_info (144: MaxCLL 957, MaxFALL 143), emulation-prevented.
func hdr10SEINAL() []byte {
	var p []byte
	u16 := func(v uint16) { p = append(p, byte(v>>8), byte(v)) }
	u32 := func(v uint32) { p = append(p, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	p = append(p, 137, 24)
	for _, v := range []uint16{13250, 34500, 7500, 3000, 34000, 16000, 15635, 16450} {
		u16(v)
	}
	u32(10_000_000)
	u32(1)
	p = append(p, 144, 4)
	u16(957)
	u16(143)
	p = append(p, 0x80)
	esc := []byte{0x4e, 0x01}
	zeros := 0
	for _, b := range p {
		if zeros >= 2 && b <= 3 {
			esc = append(esc, 3)
			zeros = 0
		}
		esc = append(esc, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return esc
}

// hdr10InBandMKV writes an MKV whose PQ HEVC track carries its static metadata
// only in the first sample's SEI, as most HDR10 muxes do.
func hdr10InBandMKV(t *testing.T) string {
	t.Helper()
	cp, err := hex.DecodeString(hevcHDRPrivateHex)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hdr.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}}
	tracks := []mkv.Track{{
		ID: 1, Type: mkv.VideoTrack, Codec: "V_MPEGH/ISO/HEVC", CodecPrivate: cp,
		Width: ptrU32(3840), Height: ptrU32(2160),
		ColorSpace: ptrU16(9), ColorTransfer: ptrU16(16), ColorPrimaries: ptrU16(9),
	}}
	if err := mw.WriteMetadata(c, tracks, 1000); err != nil {
		t.Fatal(err)
	}
	sei := hdr10SEINAL()
	frame := append([]byte{0, 0, 0, byte(len(sei))}, sei...)
	if err := mw.WriteClusterWithCues(0, 1000000, []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: frame}}); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The -in-band flag is the CLI's WithInBandColourFallback: without it probe
// reports the head only; with it the hdr10 line carries the SEI's values.
func TestCmdProbe_InBandFlag(t *testing.T) {
	src := hdr10InBandMKV(t)

	out := capture(t, func() { commands.CmdProbe(src) })
	if strings.Contains(out, "hdr10:") {
		t.Fatalf("head-only probe must report no static metadata, got:\n%s", out)
	}

	commands.InBand = true
	defer func() { commands.InBand = false }()
	out = capture(t, func() { commands.CmdProbe(src) })
	if !strings.Contains(out, "hdr10: MaxCLL=957 MaxFALL=143") || !strings.Contains(out, "lum=0.0001-1000") {
		t.Fatalf("-in-band probe must report the SEI's static metadata, got:\n%s", out)
	}

	commands.JsonOutput = true
	defer func() { commands.JsonOutput = false }()
	out = capture(t, func() { commands.CmdProbe(src) })
	if !strings.Contains(out, `"max_cll": 957`) {
		t.Fatalf("-json -in-band must carry hdr, got:\n%s", out)
	}
}
