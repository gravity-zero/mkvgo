package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
	"github.com/gravity-zero/mkvgo/mkv"
)

// mdatOf returns the payloads of a file's top-level mdat boxes, joined.
func mdatOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var out []byte
	for len(b) >= 8 {
		size, head := int(binary.BigEndian.Uint32(b)), 8
		if size == 1 {
			size, head = int(binary.BigEndian.Uint64(b[8:])), 16
		}
		if size < head || size > len(b) {
			t.Fatalf("box of %d bytes in %d", size, len(b))
		}
		if string(b[4:8]) == "mdat" {
			out = append(out, b[head:size]...)
		}
		b = b[size:]
	}
	return out
}

var dvConversions = []struct {
	name string
	conv mkv.DolbyVisionConversion
	want func(*dovitest.Fixture) [][]byte
}{
	{"profile 8.1", mkv.DolbyVisionToProfile81, func(fx *dovitest.Fixture) [][]byte { return fx.Profile81 }},
	{"base layer", mkv.DolbyVisionToBaseLayer, func(fx *dovitest.Fixture) [][]byte { return fx.Base }},
}

func checkDV(t *testing.T, stage string, conv mkv.DolbyVisionConversion, dv *mkv.DolbyVision) {
	t.Helper()
	if conv == mkv.DolbyVisionToBaseLayer {
		if dv != nil {
			t.Errorf("%s: still signals Dolby Vision %+v", stage, dv)
		}
		return
	}
	if dv == nil || dv.Profile != 8 || dv.BLSignalCompatID != 1 || dv.ELPresent {
		t.Errorf("%s: signals %+v, want profile 8 with an HDR10 base and no enhancement layer", stage, dv)
	}
}

// TestDolbyVisionRemux: a profile 7 Matroska file remuxed to MP4 and back holds the converted samples, byte for
// byte, under the configuration of the conversion; the fast-start layout sizes them as written.
func TestDolbyVisionRemux(t *testing.T) {
	for _, tc := range dvConversions {
		for _, fast := range []bool{false, true} {
			dir := t.TempDir()
			fx := dovitest.Profile7(t, dir, true)
			mp4Path, back := filepath.Join(dir, "out.mp4"), filepath.Join(dir, "back.mkv")
			if err := RemuxToMP4(context.Background(), fx.Path, mp4Path, Options{DolbyVision: tc.conv, FastStart: fast}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			c, _, err := OpenMeta(context.Background(), mp4Path)
			if err != nil {
				t.Fatal(err)
			}
			checkDV(t, tc.name+" MP4", tc.conv, videoDV(c.Tracks))
			if err := RemuxFromMP4(context.Background(), mp4Path, back); err != nil {
				t.Fatal(err)
			}
			_, blocks := readMKV(t, back)
			want := tc.want(fx)
			if len(blocks) != len(want) {
				t.Fatalf("%s: %d samples, want %d", tc.name, len(blocks), len(want))
			}
			for i := range blocks {
				if !bytes.Equal(blocks[i].Data, want[i]) {
					t.Fatalf("%s fast=%v: sample %d differs from the expected layers", tc.name, fast, i)
				}
			}
		}
	}
}

// TestDolbyVisionFromMP4: an MP4 holding both layers converts on its way to Matroska.
func TestDolbyVisionFromMP4(t *testing.T) {
	for _, tc := range dvConversions {
		dir := t.TempDir()
		fx := dovitest.Profile7(t, dir, false)
		mp4Path, back := filepath.Join(dir, "p7.mp4"), filepath.Join(dir, "back.mkv")
		if err := RemuxToMP4(context.Background(), fx.Path, mp4Path); err != nil {
			t.Fatal(err)
		}
		if err := RemuxFromMP4(context.Background(), mp4Path, back, Options{DolbyVision: tc.conv}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c, blocks := readMKV(t, back)
		checkDV(t, tc.name+" Matroska", tc.conv, videoDV(c.Tracks))
		want := tc.want(fx)
		for i := range blocks {
			if !bytes.Equal(blocks[i].Data, want[i]) {
				t.Fatalf("%s: sample %d differs from the expected layers", tc.name, i)
			}
		}
		if _, err := PlanHLS(context.Background(), mp4Path, Options{DolbyVision: tc.conv}); err == nil || !strings.Contains(err.Error(), "Options.DolbyVision") {
			t.Errorf("%s: the on-demand plan of an MP4 source answered %v", tc.name, err)
		}
	}
}

// TestDolbyVisionHLS: the full pass and the on-demand plan serve the converted samples, sized as written, and the
// master playlist names the stream the conversion leaves.
func TestDolbyVisionHLS(t *testing.T) {
	t.Run("SDR base", func(t *testing.T) { dolbyVisionHLS(t, false) })
	t.Run("HDR10 base", func(t *testing.T) { dolbyVisionHLS(t, true) })
}

func dolbyVisionHLS(t *testing.T, hdr bool) {
	for _, tc := range dvConversions {
		dir := t.TempDir()
		fx := dovitest.Build(t, dir, dovitest.Spec{Full: true, HDR: hdr})
		want := bytes.Join(tc.want(fx), nil)
		o := Options{DolbyVision: tc.conv, SegmentMs: 1000}

		out := filepath.Join(dir, "hls")
		if err := RemuxToHLS(context.Background(), fx.Path, out, o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var names []string
		_ = filepath.WalkDir(out, func(p string, _ os.DirEntry, _ error) error {
			if strings.HasPrefix(filepath.Base(p), "seg") {
				names = append(names, p)
			}
			return nil
		})
		sort.Strings(names)
		var got []byte
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, mdatOf(t, b)...)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the full pass wrote %d media bytes in %d segments, want %d", tc.name, len(got), len(names), len(want))
		}
		master, _ := os.ReadFile(filepath.Join(out, "master.m3u8"))
		checkMaster(t, tc.name+" full pass", tc.conv, hdr, string(master))

		p, err := PlanHLS(context.Background(), fx.Path, o)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got = got[:0]
		for n := range p.NumSegments() {
			seg, err := p.Segment(context.Background(), n)
			if err != nil {
				t.Fatalf("%s segment %d: %v", tc.name, n, err)
			}
			if size, _, _, ok, err := p.Stat(context.Background(), p.SegmentName(n)); err != nil || (ok && size != int64(len(seg))) {
				t.Errorf("%s segment %d: Stat says %d bytes (err %v), served %d", tc.name, n, size, err, len(seg))
			}
			if res, _, err := p.Resource(context.Background(), p.SegmentName(n)); err != nil || !bytes.Equal(res, seg) {
				t.Errorf("%s segment %d: Resource differs from Segment (err %v)", tc.name, n, err)
			}
			got = append(got, mdatOf(t, seg)...)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the plan served %d media bytes, want %d", tc.name, len(got), len(want))
		}
		checkMaster(t, tc.name+" plan", tc.conv, hdr, string(p.MasterPlaylist()))
		if init := p.InitSegment(); bytes.Contains(init, []byte("dvvC")) != (tc.conv == mkv.DolbyVisionToProfile81) || bytes.Contains(init, []byte("dvcC")) {
			t.Errorf("%s: the plan's init segment carries the wrong Dolby Vision box", tc.name)
		}
	}
}

// checkMaster holds a master playlist to the stream a conversion leaves: profile 8.1 is named beside a PQ base, and
// the base layer alone keeps the range of its own description - SDR for an SDR base, PQ for an HDR10 one.
func checkMaster(t *testing.T, stage string, conv mkv.DolbyVisionConversion, hdr bool, master string) {
	t.Helper()
	has, pq := strings.Contains(master, `SUPPLEMENTAL-CODECS="dvh1.08.06/db1p"`), strings.Contains(master, "VIDEO-RANGE=PQ")
	if !hdr && (has != (conv == mkv.DolbyVisionToProfile81) || has != pq || strings.Contains(master, "dvh1.07")) {
		t.Errorf("%s, SDR base: master playlist does not name the converted stream:\n%s", stage, master)
	}
	if hdr && (has != (conv == mkv.DolbyVisionToProfile81) || !pq || strings.Contains(master, "dvh1.07") || (!has && strings.Contains(master, "dvh1"))) {
		t.Errorf("%s, HDR10 base: master playlist does not name the converted stream:\n%s", stage, master)
	}
}

// TestDolbyVisionDamagedRPU: a sample whose RPU cannot be converted fails every path by type.
func TestDolbyVisionDamagedRPU(t *testing.T) {
	dir := t.TempDir()
	fx := dovitest.Profile7(t, dir, true)
	b, err := os.ReadFile(fx.Path)
	if err != nil {
		t.Fatal(err)
	}
	b[bytes.LastIndex(b, []byte{0x7C, 0x01, 0x19})+12] ^= 0xFF
	if err := os.WriteFile(fx.Path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	o := Options{DolbyVision: mkv.DolbyVisionToProfile81}
	var dvErr *mkv.DolbyVisionError
	if err := RemuxToMP4(context.Background(), fx.Path, filepath.Join(dir, "o.mp4"), o); !errors.As(err, &dvErr) {
		t.Errorf("RemuxToMP4: %v", err)
	}
	if err := RemuxToHLS(context.Background(), fx.Path, filepath.Join(dir, "hls"), o); !errors.As(err, &dvErr) {
		t.Errorf("RemuxToHLS: %v", err)
	}
	p, err := PlanHLS(context.Background(), fx.Path, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Segment(context.Background(), p.NumSegments()-1); !errors.As(err, &dvErr) {
		t.Errorf("plan segment: %v", err)
	}
}

// TestDolbyVisionFFprobe: an outside reader parses the converted MP4 and the converted HLS rendition to the values
// the conversion owes. Opt-in: MKVGO_FFPROBE names the ffprobe to use.
func TestDolbyVisionFFprobe(t *testing.T) {
	ctx := context.Background()
	for _, full := range []bool{false, true} {
		dir := t.TempDir()
		fx := dovitest.Build(t, dir, dovitest.Spec{Full: full, HDR: true})
		src := filepath.Join(dir, "p7.mp4")
		if err := RemuxToMP4(ctx, fx.Path, src, Options{FastStart: true}); err != nil {
			t.Fatal(err)
		}
		for _, from := range []string{fx.Path, src} {
			p81, base := filepath.Join(dir, "p81.mp4"), filepath.Join(dir, "base.mp4")
			if from == src {
				p81, base = filepath.Join(dir, "p81.mkv"), filepath.Join(dir, "base.mkv")
			}
			remux := RemuxToMP4
			if from == src {
				remux = RemuxFromMP4
			}
			if err := errors.Join(remux(ctx, from, p81, Options{DolbyVision: mkv.DolbyVisionToProfile81}),
				remux(ctx, from, base, Options{DolbyVision: mkv.DolbyVisionToBaseLayer})); err != nil {
				t.Fatal(err)
			}
			dovitest.CheckProfile81(t, from, p81, full)
			dovitest.CheckBase(t, from, base)
		}
		hls := filepath.Join(dir, "hls")
		if err := RemuxToHLS(ctx, fx.Path, hls, Options{DolbyVision: mkv.DolbyVisionToProfile81, SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		dovitest.CheckProfile81(t, fx.Path, filepath.Join(hls, "playlist.m3u8"), full)
	}
}
