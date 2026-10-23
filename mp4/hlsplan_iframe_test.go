package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// mp4box locates a top-level box by type in b, returning its header length
// (8 or 16 for the 64-bit largesize form) and declared size. Only the boxes
// this file's tests need are looked up (styp/moof/mdat), so a box whose
// declared size runs past the end of a truncated slice (the mdat of an
// I-frame byte range, whose largesize covers the WHOLE segment while the
// slice only holds its first sample) is still located by its header.
func mp4box(b []byte, typ string) (off, headerLen, size int, ok bool) {
	pos := 0
	for pos+8 <= len(b) {
		sz := int(binary.BigEndian.Uint32(b[pos:]))
		t := string(b[pos+4 : pos+8])
		hdr := 8
		if sz == 1 {
			if pos+16 > len(b) {
				return 0, 0, 0, false
			}
			sz = int(binary.BigEndian.Uint64(b[pos+8:]))
			hdr = 16
		}
		if t == typ {
			return pos, hdr, sz, true
		}
		if sz <= 0 || pos+sz > len(b) {
			return 0, 0, 0, false
		}
		pos += sz
	}
	return 0, 0, 0, false
}

// parseIframePlaylist returns the URIs of an I-frame playlist, in order: each a resource of its own, never a byte range.
func parseIframePlaylist(t *testing.T, pl []byte) []string {
	t.Helper()
	if bytes.Contains(pl, []byte("BYTERANGE")) {
		t.Errorf("I-frame playlist addresses byte ranges:\n%s", pl)
	}
	var uris []string
	lines := strings.Split(string(pl), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#EXTINF:") && i+1 < len(lines) {
			uris = append(uris, lines[i+1])
		}
	}
	return uris
}

// assertIframeFragment checks an I-frame resource is a fragment of its own holding the segment's opening keyframe
// alone: one sync sample, the segment's decode time, duration and composition offset, the same bytes, and nothing
// after them. A negative
// wantKeyframeSize takes the segment's.
func assertIframeFragment(t *testing.T, name string, frag, seg []byte, wantKeyframeSize int) {
	t.Helper()
	if _, _, _, ok := mp4box(frag, "styp"); !ok {
		t.Errorf("%s: no styp box", name)
	}
	ftfdt, fs := readFragment(t, frag)
	stfdt, ss := readFragment(t, seg)
	if len(fs) != 1 || !fs[0].sync {
		t.Fatalf("%s: %d samples (sync %v), want the keyframe alone", name, len(fs), len(fs) > 0 && fs[0].sync)
	}
	if wantKeyframeSize < 0 {
		wantKeyframeSize = len(ss[0].data)
	}
	if len(fs[0].data) != wantKeyframeSize || !bytes.Equal(fs[0].data, ss[0].data) || ftfdt != stfdt {
		t.Errorf("%s: keyframe of %d bytes at %d, want the segment's %d bytes at %d", name, len(fs[0].data), ftfdt, len(ss[0].data), stfdt)
	}
	_, fdurs, fcts := segmentTiming(t, frag)
	_, sdurs, scts := segmentTiming(t, seg)
	if fdurs[0] != sdurs[0] || fcts[0] != scts[0] {
		t.Errorf("%s: keyframe lasts %d at composition offset %d, want the segment's %d at %d", name, fdurs[0], fcts[0], sdurs[0], scts[0])
	}
	mdatOff, mdatHdr, mdatSize, ok := mp4box(frag, "mdat")
	if !ok || mdatOff+mdatSize != len(frag) || mdatSize-mdatHdr != wantKeyframeSize {
		t.Errorf("%s: mdat at %d of %d bytes in a %d-byte fragment, want the keyframe's %d bytes ending it", name, mdatOff, mdatSize, len(frag), wantKeyframeSize)
	}
}

// assertIframeFragments checks the full pass in dir wrote each I-frame resource its playlist names as a fragment of
// its segment's keyframe, and the plan lists and serves the same bytes.
func assertIframeFragments(t *testing.T, dir string, plan *HLSPlan, keySize int) {
	t.Helper()
	pl, err := os.ReadFile(filepath.Join(dir, "iframe.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	uris := parseIframePlaylist(t, pl)
	if len(uris) < 5 {
		t.Fatalf("fixture produced only %d I-frames, want 5+", len(uris))
	}
	names := plan.Resources()
	for _, uri := range uris {
		var n int
		if _, err := fmt.Sscanf(uri, "iframe%d.m4s", &n); err != nil || uri != fmt.Sprintf("iframe%05d.m4s", n) {
			t.Fatalf("I-frame URI %q, want iframeNNNNN.m4s", uri)
		}
		frag, err := os.ReadFile(filepath.Join(dir, uri))
		if err != nil {
			t.Fatal(err)
		}
		seg, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("seg%05d.m4s", n)))
		if err != nil {
			t.Fatal(err)
		}
		assertIframeFragment(t, uri, frag, seg, keySize)
		got, mime, err := plan.Resource(context.Background(), uri)
		if err != nil {
			t.Fatalf("Resource(%s): %v", uri, err)
		}
		if !bytes.Equal(got, frag) || mime != "video/iso.segment" {
			t.Errorf("plan %s (%d bytes, %s) differs from the full pass's (%d bytes)", uri, len(got), mime, len(frag))
		}
		if !containsName(names, uri) {
			t.Errorf("Resources() does not list %s", uri)
		}
		if !isPackagedName(uri) || !isPackagedName("v2/"+uri) {
			t.Errorf("%s is not a packaged name", uri)
		}
	}
}

// keyframeSize/otherSize distinguish a GOP's leading I-frame from the rest in
// the fixtures below, so the byte-range assertions are unambiguous.
const (
	iframeTestKeySize   = 200
	iframeTestOtherSize = 20
)

func iframeTestFrame(i int, key bool) []byte {
	n := iframeTestOtherSize
	if key {
		n = iframeTestKeySize
	}
	d := make([]byte, n)
	d[0], d[1], d[2], d[3] = 0x00, 0x00, 0x00, 0x01
	d[4] = 0x65
	d[5] = byte(i)
	return d
}

// A Matroska source's on-demand trick-play playlist is lazily built and
// byte-identical to the full pass's, across several keyframe-cut segments;
// each advertised byte range, sliced out of the actual segment file, decodes
// to exactly the segment-opening keyframe (never more).
func TestPlanHLSIframeMatroska(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 150; i++ { // 6s @ 40ms/frame, keyframe every 25 frames (1s) → 6 segments @ 1s
		key := i%25 == 0
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: iframeTestFrame(i, key)})
	}
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks)

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	fullIframe, err := os.ReadFile(filepath.Join(dir, "iframe.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !containsName(plan.Resources(), "iframe.m3u8") {
		t.Fatal("Resources() must list iframe.m3u8 for an unencrypted Matroska plan with a video track")
	}
	planIframe, mime, err := plan.Resource(context.Background(), "iframe.m3u8")
	if err != nil {
		t.Fatalf("Resource(iframe.m3u8): %v", err)
	}
	if mime != "application/vnd.apple.mpegurl" {
		t.Errorf("iframe.m3u8 mime = %q", mime)
	}
	if !bytes.Equal(planIframe, fullIframe) {
		t.Errorf("plan iframe.m3u8 differs from the full pass:\nplan: %s\nfull: %s", planIframe, fullIframe)
	}

	// Second call is served from cache and stays identical.
	again, err := p4iframe(t, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, planIframe) {
		t.Error("iframe.m3u8 changed across repeated requests")
	}

	// Every I-frame is a fragment of exactly the leading keyframe.
	assertIframeFragments(t, dir, plan, iframeTestKeySize)

	// The master advertises the trick-play variant, mirroring the MP4 path.
	master, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(master, []byte("#EXT-X-I-FRAME-STREAM-INF:")) {
		t.Errorf("full-pass master missing EXT-X-I-FRAME-STREAM-INF:\n%s", master)
	}
}

func p4iframe(t *testing.T, plan *HLSPlan) ([]byte, error) {
	t.Helper()
	data, _, err := plan.Resource(context.Background(), "iframe.m3u8")
	return data, err
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// Open-GOP style reordering (a GOP's frames written in decode order - I first
// - but with non-monotonic presentation timecodes) must not confuse which
// sample is the segment-opening keyframe: it is always the first block
// encountered in file/decode order, never picked by sorting on PTS.
func TestPlanHLSIframeMatroska_ReorderedPTS(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	// 6 GOPs of 5 frames (200ms/GOP): decode order I, P, B, B, B with the P's
	// presentation time placed at the GOP's end and the B's in between - a
	// display order of I, B, B, B, P out of an I, P, B, B, B decode order.
	// The GOP spans 1200ms so each one lands in its own ~1s test cluster (the
	// fixture writer's cluster-grouping threshold) and so gets its own Cue -
	// the plan's segment boundaries come from Cues, one per keyframe.
	const gopMs = 1200
	const gops = 6
	for g := 0; g < gops; g++ {
		base := int64(g) * gopMs
		gblocks = append(gblocks, genBlock{track: 1, pts: base, key: true, data: iframeTestFrame(g*5, true)})
		gblocks = append(gblocks, genBlock{track: 1, pts: base + 960, key: false, data: iframeTestFrame(g*5+1, false)})
		gblocks = append(gblocks, genBlock{track: 1, pts: base + 240, key: false, data: iframeTestFrame(g*5+2, false)})
		gblocks = append(gblocks, genBlock{track: 1, pts: base + 480, key: false, data: iframeTestFrame(g*5+3, false)})
		gblocks = append(gblocks, genBlock{track: 1, pts: base + 720, key: false, data: iframeTestFrame(g*5+4, false)})
	}
	// buildMKV writes blocks in the order given (decode order); do NOT sort by pts.
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks)

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: gopMs}); err != nil {
		t.Fatal(err)
	}
	fullIframe, err := os.ReadFile(filepath.Join(dir, "iframe.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: gopMs})
	if err != nil {
		t.Fatal(err)
	}
	planIframe, _, err := plan.Resource(context.Background(), "iframe.m3u8")
	if err != nil {
		t.Fatalf("Resource(iframe.m3u8): %v", err)
	}
	if !bytes.Equal(planIframe, fullIframe) {
		t.Errorf("plan iframe.m3u8 differs from the full pass with reordered PTS:\nplan: %s\nfull: %s", planIframe, fullIframe)
	}
	assertIframeFragments(t, dir, plan, iframeTestKeySize)
}

// An encrypted presentation exposes no trick-play surface (an AES-CBC
// ciphertext sub-range is not independently decryptable) - the same
// exclusion the MP4 path and the full pass already apply.
func TestPlanHLSIframeMatroska_Encrypted(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 100; i++ {
		key := i%25 == 0
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: iframeTestFrame(i, key)})
	}
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks)

	enc := &HLSEncryption{Key: bytes.Repeat([]byte{0x42}, 16), KeyURI: "https://example.test/key"}
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, Encrypt: enc}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "iframe.m3u8")); err == nil {
		t.Error("full pass must not write iframe.m3u8 for an encrypted presentation")
	}

	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000, Encrypt: enc})
	if err != nil {
		t.Fatal(err)
	}
	if containsName(plan.Resources(), "iframe.m3u8") {
		t.Error("Resources() must not list iframe.m3u8 for an encrypted plan")
	}
	if _, _, err := plan.Resource(context.Background(), "iframe.m3u8"); err == nil {
		t.Error("Resource(iframe.m3u8) must error for an encrypted plan")
	}
}

// A CENC Matroska plan exposes no trick-play surface either, as the MP4 plan and the full pass: the BYTERANGEs would
// point into fragments whose senc/saiz/saio the I-frame slice leaves out.
func TestPlanHLSIframeMatroska_CENC(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 100; i++ {
		key := i%25 == 0
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: iframeTestFrame(i, key)})
	}
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks)
	cenc := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), InlineKey: true}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000, CENC: cenc})
	if err != nil {
		t.Fatal(err)
	}
	if containsName(plan.Resources(), "iframe.m3u8") {
		t.Error("Resources() must not list iframe.m3u8 for a CENC plan")
	}
	if _, _, err := plan.Resource(context.Background(), "iframe.m3u8"); err == nil {
		t.Error("Resource(iframe.m3u8) must error for a CENC plan")
	}
}

// The lazy I-frame pass costs block HEADERS, never payload volume: on a
// fixture whose video frames are all large enough (128 KiB) to be worth
// seeking over, building iframe.m3u8 must read next to nothing of the
// source, and PlanHLS's own construction (before any resource is requested)
// must stay just as cheap as it always was - unaffected by the new lazy path
// existing at all. (A block below the seek-skip threshold falls back to a
// plain buffered read - the reader's pre-existing, documented trade-off for
// small interleaved payloads - so this fixture keeps every frame comfortably
// above it to isolate the "no payload volume" claim.)
func TestPlanHLSIframeMatroska_SkipsPayload(t *testing.T) {
	w, h := uint32(320), uint32(240)
	const frameSize = 512 << 10
	const framesPerGOP = 10
	const gops = 8
	bigFrame := func(i int) []byte {
		d := make([]byte, frameSize)
		d[0], d[1], d[2], d[3] = 0x00, 0x00, 0x00, 0x01
		d[4] = 0x65
		d[5] = byte(i)
		return d
	}
	var gblocks []genBlock
	for i := 0; i < framesPerGOP*gops; i++ { // 1s GOPs (40ms/frame × 25)
		key := i%framesPerGOP == 0
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: bigFrame(i)})
	}
	src := buildPlanFixture(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks, nil)

	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	var readBytes int64
	fs := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return countingRSC{f: f, n: &readBytes}, nil
	}}

	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	// Construction touches at most the first block plus the last cued
	// cluster to EOF (pre-existing peekHead/peekTail behaviour, untouched by
	// the lazy I-frame path) - nowhere near a full-file scan.
	if limit := st.Size() / 2; readBytes > limit {
		t.Errorf("PlanHLS construction read %d of %d bytes (%.0f%%) - must stay well short of a full scan, unaffected by the lazy I-frame path",
			readBytes, st.Size(), 100*float64(readBytes)/float64(st.Size()))
	}

	readBytes = 0 // count the lazy I-frame pass alone
	if _, _, err := plan.Resource(context.Background(), "iframe.m3u8"); err != nil {
		t.Fatalf("Resource(iframe.m3u8): %v", err)
	}
	if limit := st.Size() / 20; readBytes > limit {
		t.Errorf("iframe.m3u8 build read %d of %d bytes (%.0f%%) - media payloads must be skipped, not read",
			readBytes, st.Size(), 100*float64(readBytes)/float64(st.Size()))
	}
}

// An MP4 source's I-frames are fragments of their own too, the plan's built from the sample table equal to the full
// pass's files.
func TestPlanHLSIframeMP4Source(t *testing.T) {
	src := hlsplanMP4Source(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	pl, _, err := plan.Resource(context.Background(), "iframe.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if full, _ := os.ReadFile(filepath.Join(dir, "iframe.m3u8")); !bytes.Equal(pl, full) {
		t.Errorf("plan iframe.m3u8 differs from the full pass:\nplan: %s\nfull: %s", pl, full)
	}
	assertIframeFragments(t, dir, plan, 6)
}

// A keyframe stored compressed or header-stripped is served as the segment carries it: inflated, its stripped bytes
// put back, the plan's fragment equal to the full pass's.
func TestPlanHLSIframeStoredFrames(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var zblocks []genBlock
	for i := 0; i < 150; i++ {
		key := i%25 == 0
		zblocks = append(zblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: zlibBytes(iframeTestFrame(i, key))})
	}
	sources := map[string]string{
		"zlib": buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h,
			Compression: mkv.CompressionZlib}}, zblocks),
		"header stripping": lacedStripMKV(t, 1, true),
	}
	keySize := map[string]int{"zlib": iframeTestKeySize, "header stripping": len(cryptVecFrame(0, true))}
	for kind, src := range sources {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000})
			if err != nil {
				t.Fatal(err)
			}
			pl, _, err := plan.Resource(context.Background(), "iframe.m3u8")
			if err != nil {
				t.Fatal(err)
			}
			if full, _ := os.ReadFile(filepath.Join(dir, "iframe.m3u8")); !bytes.Equal(pl, full) {
				t.Errorf("plan iframe.m3u8 differs from the full pass:\nplan: %s\nfull: %s", pl, full)
			}
			assertIframeFragments(t, dir, plan, keySize[kind])
		})
	}
}

// A name that is no I-frame of the plan is refused, and a concatenation leaves its parts' fragments out with the
// playlist naming them.
func TestPlanHLSIframeNames(t *testing.T) {
	plan, err := PlanHLS(context.Background(), hlsplanMP4Source(t), Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"iframe1.m4s", "iframe00000.m4s", "iframe99999.m4s", "iframe00001.mp4"} {
		if _, _, err := plan.Resource(context.Background(), name); !errors.Is(err, ErrNoResource) {
			t.Errorf("Resource(%s) = %v, want a missing resource", name, err)
		}
	}
	if !isConcatSuperseded("iframe00001.m4s") || isConcatSuperseded("iframe1.m4s") {
		t.Error("a concatenated part's I-frame fragments are not left out with its iframe.m3u8")
	}
}
