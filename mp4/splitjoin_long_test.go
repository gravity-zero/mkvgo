package mp4

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// longFilm is a sparse three-hour MP4 (a picture every 2 s, a keyframe a minute, a 44.1 kHz sound sample every 2 s) with chapters past 2^32 ticks of 441 kHz, the common timescale a split or join writes it in.
func longFilm(t *testing.T) string {
	t.Helper()
	const minutes = 180
	var blocks []genBlock
	for ms := int64(0); ms < minutes*60_000; ms += 2000 {
		blocks = append(blocks,
			genBlock{track: 1, pts: ms, key: ms%60_000 == 0, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(ms / 2000)}},
			genBlock{track: 2, pts: ms, key: true, data: []byte{0xAA, byte(ms / 2000)}})
	}
	sortGenBlocks(blocks)
	chapters := []mkv.Chapter{
		{ID: 1, Title: "Opening", StartMs: 0},
		{ID: 2, Title: "Middle", StartMs: 60 * 60_000},
		{ID: 3, Title: "Past the 32-bit range", StartMs: 175 * 60_000},
	}
	src := filepath.Join(t.TempDir(), "long.mp4")
	if err := RemuxToMP4(context.Background(), buildMKVWithChapters(t, synthFixtureTracks(), blocks, chapters), src); err != nil {
		t.Fatal(err)
	}
	return src
}

// TestSplitJoinPastThe32BitRange: a film whose length overflows 32-bit headers at the timescale a split or join writes comes back whole, its long parts and joins in version 1 headers.
func TestSplitJoinPastThe32BitRange(t *testing.T) {
	src := longFilm(t)
	if d := mustMeta(t, src).DurationMs; d < 179*60_000 {
		t.Fatalf("the source lasts %d ms, want three hours", d)
	}
	for _, cut := range []int64{60 * 60_000, 170 * 60_000} {
		t.Run(fmt.Sprintf("cut=%dmin", cut/60_000), func(t *testing.T) {
			ranges := []mkv.TimeRange{{StartMs: 0, EndMs: cut}, {StartMs: cut}}
			parts := checkSplitJoin(t, src, mkv.SplitOptions{Ranges: ranges}, 2, false)
			for _, p := range parts {
				long := mustMeta(t, p).DurationMs > 162*60_000 // 2^32 ticks of 441 kHz
				if v := headerVersions(t, p); long != (v["mvhd"] == 1) {
					t.Errorf("%s: header versions %v", filepath.Base(p), v)
				}
			}
		})
	}
	dst := filepath.Join(t.TempDir(), "twice.mp4")
	if err := Join(context.Background(), []string{src, src}, dst); err != nil {
		t.Fatal(err)
	}
	checkJoinedEndToEnd(t, []string{src, src}, dst)
	if w, g := 2*mustMeta(t, src).DurationMs, mustMeta(t, dst).DurationMs; absMs(g-w) > 1 {
		t.Errorf("the six-hour join lasts %d ms, want %d", g, w)
	}
	if got := mustMeta(t, dst).Chapters; len(got) != 6 || got[5].StartMs != mustMeta(t, src).DurationMs+175*60_000 {
		t.Errorf("chapters of the six-hour join: %+v", got)
	}
	if v := headerVersions(t, dst); v["mvhd"] != 1 || v["tkhd"] != 1 || v["elst"] != 1 {
		t.Errorf("six-hour join header versions %v, want mvhd, tkhd and elst in version 1", v)
	}
}

// headerVersions is the highest version a file's mvhd, tkhd, mdhd and elst boxes carry.
func headerVersions(t *testing.T, path string) map[string]int {
	t.Helper()
	v := map[string]int{}
	note := func(name string, p []byte, err error) {
		if err == nil && len(p) > 0 {
			v[name] = max(v[name], int(p[0]))
		}
	}
	for _, b := range moovBoxesOf(t, path) {
		switch b.typ {
		case "mvhd":
			note("mvhd", b.payload, nil)
		case "trak":
			p, err := boxAt(b.payload, "tkhd")
			note("tkhd", p, err)
			p, err = boxAt(b.payload, "mdia", "mdhd")
			note("mdhd", p, err)
			p, err = boxAt(b.payload, "edts", "elst")
			note("elst", p, err)
		}
	}
	return v
}
