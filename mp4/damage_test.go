package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestRemuxRefusesDamageInsideTheFile: a remux tolerates a truncated tail by
// stopping at it. An element that overruns the file in the MIDDLE of it read
// the same - and the remux of a 37 s file wrote its first second and reported
// success. It is refused, with the repair named, and leaves no output; the
// same file cut at its tail still remuxes what it holds.
func TestRemuxRefusesDamageInsideTheFile(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
	}
	var blocks []genBlock
	for i := 0; i < 75; i++ { // 3 s at 25 fps: three clusters
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: bytes.Repeat([]byte{0xD0, byte(i)}, 16)})
	}
	src := buildMKV(t, tracks, blocks)
	sound, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()

	// Plant a 2 GiB element over the first block of the second cluster.
	cluster := []byte{0x1F, 0x43, 0xB6, 0x75}
	first := bytes.Index(sound, cluster)
	second := first + 4 + bytes.Index(sound[first+4:], cluster)
	block := second + bytes.IndexByte(sound[second:], 0xA3)
	damaged := append([]byte(nil), sound...)
	copy(damaged[block:], []byte{0xEC, 0x01, 0x00, 0x00, 0x00, 0x7F, 0xFF, 0xFF, 0xFF})
	bad := filepath.Join(dir, "damaged.mkv")
	if err := os.WriteFile(bad, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "damaged.mp4")
	err = RemuxToMP4(ctx, bad, out)
	if !errors.Is(err, reader.ErrDamagedRegion) {
		t.Fatalf("RemuxToMP4 on a file damaged in its second cluster = %v, want the damage refused", err)
	}
	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Errorf("the refused remux left an output behind (stat: %v)", serr)
	}

	// The tolerance this must not break: the same file cut inside its last
	// block is a truncated tail, and still remuxes.
	cut := filepath.Join(dir, "cut.mkv")
	lastCluster := bytes.LastIndex(sound, cluster)
	if err := os.WriteFile(cut, sound[:lastCluster+60], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToMP4(ctx, cut, filepath.Join(dir, "cut.mp4")); err != nil {
		t.Fatalf("RemuxToMP4 on a truncated tail: %v", err)
	}
}

// zeroedSource copies src with [from, to) zeroed: the pieces a download never
// received, the shape of the damaged files a library holds.
func zeroedSource(t *testing.T, src string, from, to int) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	for i := from; i < to && i < len(data); i++ {
		data[i] = 0
	}
	out := filepath.Join(t.TempDir(), "zeroed.mkv")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// clusterOffsets lists where the source's Clusters start.
func clusterOffsets(t *testing.T, src string) []int {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var offs []int
	for at := 0; ; {
		i := bytes.Index(data[at:], []byte{0x1F, 0x43, 0xB6, 0x75})
		if i < 0 {
			return offs
		}
		offs = append(offs, at+i)
		at += i + 4
	}
}

// TestOnDemandServesPastASmallDamagedRegion: a zeroed stretch shorter than the
// plan's cap (Options.DamageBytes) that swallows the end of one
// cluster and the start of the next, cue included. The window holding its start
// serves what it read up to the damage, the window whose cue points into the
// zeros serves no sample instead of failing, every other window is the full
// pass's, and the full pass itself refuses the file with the repair named.
func TestOnDemandServesPastASmallDamagedRegion(t *testing.T) {
	clean := buildInterleavedSource(t, 40) // 20 clusters of 2 s (1 MiB each), each cued
	offs := clusterOffsets(t, clean)
	if len(offs) < 16 {
		t.Fatalf("%d clusters, want 60 (one per 2 s)", len(offs))
	}
	from, to := offs[11]-100<<10, offs[11]+300<<10 // 400 KiB across the boundary of the 22 s cluster: 1.1 MB to the next Cluster
	bad := zeroedSource(t, clean, from, to)
	t.Logf("%d clusters, first at %d, 20 s at %d, 22 s at %d; zeroed [%d, %d)", len(offs), offs[0], offs[10], offs[11], from, to)
	ctx := context.Background()
	full := t.TempDir()
	if err := RemuxToHLS(ctx, clean, full, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(ctx, bad, t.TempDir(), Options{SegmentMs: 2000}); !errors.Is(err, reader.ErrDamagedRegion) {
		t.Errorf("the full pass on the zeroed file = %v, want the damage refused", err)
	}
	if _, err := PlanHLS(ctx, clean, Options{SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceLocal}); err != nil {
		t.Fatalf("the clean source does not plan: %v", err)
	}
	plan, err := PlanHLS(ctx, bad, Options{SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	for n := 8; n < 15; n++ {
		video, audio := serveWindow(t, plan, n)
		wantV, _ := os.ReadFile(filepath.Join(full, plan.SegmentName(n)))
		wantA, _ := os.ReadFile(filepath.Join(full, fmt.Sprintf("seg_a1_%05d.m4s", n+1)))
		t.Logf("window %d: video %d (full pass %d), audio %d (%d)", n, len(video), len(wantV), len(audio), len(wantA))
		switch n {
		case 10: // 20-22 s: its last 100 KiB are zeros
			if len(video) >= len(wantV) || len(video) == 0 {
				t.Errorf("window %d: video %d bytes, want fewer than the full pass's %d and some", n, len(video), len(wantV))
			}
		case 11: // 22-24 s: its cue points into the zeros
			if len(video) >= len(wantV) {
				t.Errorf("window %d: video %d bytes, want fewer than the full pass's %d (no sample)", n, len(video), len(wantV))
			}
		default:
			if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
				t.Errorf("window %d: segments differ from the full pass's", n)
			}
		}
	}
	st := plan.Stats()
	if st.DamageSkips == 0 || st.DamageRefusals != 0 {
		t.Errorf("stats: %d skips, %d refusals; want skips and no refusal", st.DamageSkips, st.DamageRefusals)
	}
	before := plan.Stats().SourceReadBytes
	video, _ := serveWindow(t, plan, 11) // again: seated past the region it knows, no scan of the zeros
	if read := plan.Stats().SourceReadBytes - before; read > int64(len(video))+64<<10 {
		t.Errorf("window 11 served again read %d bytes for %d served; want nothing past what it serves", read, len(video))
	}
}

// TestOnDemandRefusesADamagedRegionBeyondTheCap: a zeroed stretch longer than
// the cap is refused (reader.ErrDamagedRegion, no Resume), once in the stats
// however many windows ask, and the windows past it serve as before.
func TestOnDemandRefusesADamagedRegionBeyondTheCap(t *testing.T) {
	clean := buildInterleavedSource(t, 40) // 20 clusters of 2 s, each cued
	offs := clusterOffsets(t, clean)
	from, to := offs[10]+100<<10, offs[10]+100<<10+2560<<10 // 2.5 MiB from inside the 20 s cluster, over the cap set below
	bad := zeroedSource(t, clean, from, to)
	ctx := context.Background()
	full := t.TempDir()
	if err := RemuxToHLS(ctx, clean, full, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, bad, Options{SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceLocal, DamageBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{10, 10, 11} {
		_, err := plan.Open(ctx, plan.SegmentName(n))
		var de *reader.DamageError
		if !errors.Is(err, reader.ErrDamagedRegion) || !errors.As(err, &de) || de.Resume >= 0 {
			t.Errorf("window %d: err %v, want the damage refused without a Resume", n, err)
		}
	}
	if st := plan.Stats(); st.DamageRefusals != 1 || st.DamageSkips != 0 {
		t.Errorf("stats: %d refusals, %d skips; want 1 and 0", st.DamageRefusals, st.DamageSkips)
	}
	for _, n := range []int{9, 16} {
		video, audio := serveWindow(t, plan, n)
		wantV, _ := os.ReadFile(filepath.Join(full, plan.SegmentName(n)))
		wantA, _ := os.ReadFile(filepath.Join(full, fmt.Sprintf("seg_a1_%05d.m4s", n+1)))
		if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
			t.Errorf("window %d: segments differ from the full pass's", n)
		}
	}
}

// TestOnDemandNegativeDamageBytesRefusesAnyDamage: a negative Options.DamageBytes passes over no damage, as in
// mkv.Options: even a short region is refused with reader.ErrDamagedRegion.
func TestOnDemandNegativeDamageBytesRefusesAnyDamage(t *testing.T) {
	clean := buildInterleavedSource(t, 40)
	offs := clusterOffsets(t, clean)
	bad := zeroedSource(t, clean, offs[10]+100<<10, offs[10]+100<<10+64<<10)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, bad, Options{SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceLocal, DamageBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Resource(ctx, plan.SegmentName(10)); !errors.Is(err, reader.ErrDamagedRegion) {
		t.Errorf("window 10 under DamageBytes -1: %v, want ErrDamagedRegion", err)
	}
}
