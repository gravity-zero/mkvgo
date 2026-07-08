package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
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

// mp4child is mp4box scoped to a container box's payload (skip its own header).
func mp4child(b []byte, headerLen int, typ string) (off, hdr, size int, ok bool) {
	o, h, s, found := mp4box(b[headerLen:], typ)
	if !found {
		return 0, 0, 0, false
	}
	return o + headerLen, h, s, true
}

// trunFirstSampleSize reads sample[0]'s size field from a trun box (as
// buildTraf writes it: version 1, flags always carrying data_offset +
// duration + size + flags, optionally + composition offset): 12 bytes of
// fullbox header, 4 bytes sample_count, 4 bytes data_offset, 4 bytes
// duration, then the size field.
func trunFirstSampleSize(trun []byte) uint32 {
	return binary.BigEndian.Uint32(trun[24:28])
}

func trunSampleCount(trun []byte) uint32 {
	return binary.BigEndian.Uint32(trun[12:16])
}

// parseIframePlaylist extracts the (length, segment-name) pairs of an
// EXT-X-BYTERANGE playlist, in order.
func parseIframePlaylist(t *testing.T, pl []byte) (lengths []int, segs []string) {
	t.Helper()
	lines := strings.Split(string(pl), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "#EXT-X-BYTERANGE:") {
			continue
		}
		lenStr, _, ok := strings.Cut(strings.TrimPrefix(l, "#EXT-X-BYTERANGE:"), "@")
		if !ok {
			t.Fatalf("malformed byterange line %q", l)
		}
		n, err := strconv.Atoi(lenStr)
		if err != nil {
			t.Fatalf("malformed byterange length %q: %v", lenStr, err)
		}
		if i+1 >= len(lines) {
			t.Fatalf("byterange line with no following URI")
		}
		lengths = append(lengths, n)
		segs = append(segs, lines[i+1])
	}
	return lengths, segs
}

// assertIframeSlice slices dir/segName at [0, length) and checks it box-parses
// structurally: styp, moof (with a trun) and mdat all present, and the bytes
// physically available for the mdat payload equal wantKeyframeSize exactly -
// the trick-play contract (a player fetching this range gets exactly the
// segment-opening keyframe's bytes, nothing more).
func assertIframeSlice(t *testing.T, dir, segName string, length int, wantKeyframeSize int) {
	t.Helper()
	full, err := os.ReadFile(filepath.Join(dir, segName))
	if err != nil {
		t.Fatalf("read %s: %v", segName, err)
	}
	if length > len(full) {
		t.Fatalf("%s: byterange length %d exceeds segment size %d", segName, length, len(full))
	}
	slice := full[:length]

	if _, _, _, ok := mp4box(slice, "styp"); !ok {
		t.Errorf("%s[:%d]: no styp box", segName, length)
	}
	moofOff, moofHdr, moofSize, ok := mp4box(slice, "moof")
	if !ok {
		t.Fatalf("%s[:%d]: no moof box", segName, length)
	}
	moof := slice[moofOff : moofOff+moofSize]
	trafOff, trafHdr, trafSize, ok := mp4child(moof, moofHdr, "traf")
	if !ok {
		t.Fatalf("%s[:%d]: moof has no traf", segName, length)
	}
	traf := moof[trafOff : trafOff+trafSize]
	trunOff, _, trunSize, ok := mp4child(traf, trafHdr, "trun")
	if !ok {
		t.Fatalf("%s[:%d]: traf has no trun", segName, length)
	}
	trun := traf[trunOff : trunOff+trunSize]
	if n := trunSampleCount(trun); n == 0 {
		t.Errorf("%s[:%d]: trun declares 0 samples", segName, length)
	}
	if got := int(trunFirstSampleSize(trun)); got != wantKeyframeSize {
		t.Errorf("%s[:%d]: trun sample[0].size = %d, want %d", segName, length, got, wantKeyframeSize)
	}

	mdatOff, mdatHdr, _, ok := mp4box(slice, "mdat")
	if !ok {
		t.Fatalf("%s[:%d]: no mdat box", segName, length)
	}
	if payload := len(slice) - (mdatOff + mdatHdr); payload != wantKeyframeSize {
		t.Errorf("%s[:%d]: mdat payload available in the slice = %d bytes, want exactly the keyframe size %d",
			segName, length, payload, wantKeyframeSize)
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
	fullLens, fullSegs := parseIframePlaylist(t, fullIframe)
	if len(fullSegs) < 5 {
		t.Fatalf("fixture produced only %d I-frames, want 5+", len(fullSegs))
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

	// Every advertised byte range decodes to exactly the leading keyframe.
	for i, segName := range fullSegs {
		assertIframeSlice(t, dir, segName, fullLens[i], iframeTestKeySize)
	}

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
		if g == gops-1 {
			// The last GOP is left in decode == presentation order: the
			// on-demand plan's final-segment duration is derived from the
			// two highest PTS values it sees (peekTail) while the full pass
			// derives it from the decode-order-last sample's own duration -
			// they only agree when that sample is also the highest-PTS one,
			// a pre-existing, general trait of open-GOP reordering unrelated
			// to trick-play. Keeping this one GOP unreordered isolates what
			// this test is actually about: picking the right leading sample.
			gblocks = append(gblocks, genBlock{track: 1, pts: base + 240, key: false, data: iframeTestFrame(g*5+1, false)})
			gblocks = append(gblocks, genBlock{track: 1, pts: base + 480, key: false, data: iframeTestFrame(g*5+2, false)})
			gblocks = append(gblocks, genBlock{track: 1, pts: base + 720, key: false, data: iframeTestFrame(g*5+3, false)})
			gblocks = append(gblocks, genBlock{track: 1, pts: base + 960, key: false, data: iframeTestFrame(g*5+4, false)})
			continue
		}
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
	fullLens, fullSegs := parseIframePlaylist(t, fullIframe)
	if len(fullSegs) < 5 {
		t.Fatalf("fixture produced only %d I-frames, want 5+", len(fullSegs))
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
	for i, segName := range fullSegs {
		assertIframeSlice(t, dir, segName, fullLens[i], iframeTestKeySize)
	}
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
