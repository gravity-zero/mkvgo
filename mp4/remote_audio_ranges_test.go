package mp4

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
)

// bigFrameMP4 is an MP4 of 80 KiB video frames whose audio the muxer stores in
// runs between them: an audio segment is a few KiB in a window of megabytes.
func bigFrameMP4(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		frame := bytes.Repeat([]byte{byte(i)}, 80<<10)
		copy(frame, []byte{0, 0, 0, 1, 0x65})
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: frame})
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: bytes.Repeat([]byte{0xAA, byte(i)}, 150)})
	}
	sortGenBlocks(blocks)
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	out := filepath.Join(t.TempDir(), "big.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, out, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	return out
}

// rangedBytes sums the bytes the Range headers of the GETs since from asked for.
func (r *remoteSource) rangedBytes(from int) (n int64, gets int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rg := range r.ranges[from:] {
		var a, b int64
		if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &a, &b); err == nil {
			n += b - a + 1
		}
	}
	return n, len(r.ranges) - from
}

// A remote MP4's audio segment is asked for by its samples' ranges when they
// take few requests - the video between them is never fetched - and read
// through, video and all, when they would take more than remoteMaxRanges;
// Resource's bytes either way.
func TestRemoteAudioRanges(t *testing.T) {
	src := bigFrameMP4(t)
	ctx := context.Background()
	ref, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	name := "seg_a1_00002.m4s"
	want, _, err := ref.Resource(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, maxRanges := range []int{16, 0} {
		t.Run(fmt.Sprintf("maxRanges=%d", maxRanges), func(t *testing.T) {
			defer func(n int) { remoteMaxRanges = n }(remoteMaxRanges)
			remoteMaxRanges = maxRanges
			rs := newRemoteSource(t, src)
			plan, err := PlanHLS(ctx, rs.srv.URL+"/s.mp4", Options{SegmentMs: 2000, FS: httpfs.New().Port(), StreamFromFS: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(plan.share.dropAll)
			rs.mu.Lock()
			from := len(rs.ranges)
			rs.mu.Unlock()
			h, err := plan.Open(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if _, err := h.WriteTo(ctx, &got); err != nil {
				t.Fatal(err)
			}
			h.Close()
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatal("bytes differ from Resource's")
			}
			fetched, gets := rs.rangedBytes(from)
			switch {
			case maxRanges == 16 && (fetched > int64(len(want))*2 || gets > 16):
				t.Errorf("by ranges: %d bytes in %d requests for a %d-byte segment", fetched, gets, len(want))
			case maxRanges == 0 && (fetched < 512<<10 || gets > 2):
				t.Errorf("read through: %d bytes in %d requests, want a window or more in one or two", fetched, gets)
			}
		})
	}
}

// Runs whose holes pass the gap take one read each; closer ones share a read.
func TestRangeCount(t *testing.T) {
	var pieces []span
	for i := int64(0); i < 10; i++ { // samples 100 KiB apart: video frames between them
		pieces = append(pieces, span{emit: []byte("h")}, span{off: i * 100 << 10, n: 1500})
	}
	if n := rangeCount(pieces, 64<<10); n != 10 {
		t.Errorf("100 KiB holes, 64 KiB gap: %d reads, want 10", n)
	}
	if n := rangeCount(pieces, 128<<10); n != 1 {
		t.Errorf("100 KiB holes, 128 KiB gap: %d reads, want 1", n)
	}
}

// A CENC video segment read sample by sample is read ahead over its whole
// range, the window topped up as the samples go, not over its first 8 MiB only.
func TestCENCSamplesReadAhead(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 60; i++ { // 12 MiB of video in one segment, one H.264 picture a frame
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i == 0, data: testAVCFrame(200 << 10)})
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	type rng struct{ off, n int64 }
	var asked []rng
	defer func(f func(*os.File, int64, int64)) { adviseWillNeed = f }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { asked = append(asked, rng{off, n}) }
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return false }
	ctx := context.Background()
	key := []byte("0123456789abcdef")
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, SourceAccess: SourceLocal, CENC: &CENCOptions{Scheme: "cenc", Key: key, KeyID: key, IV: key[:8], KeyURI: "k"}})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := plan.Open(ctx, plan.SegmentName(0))
	if err != nil {
		t.Fatal(err)
	}
	asked = nil
	if _, err := h2.WriteTo(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	h2.Close()
	var covered int64
	for _, r := range asked {
		covered += r.n
	}
	if covered < 11<<20 {
		t.Fatalf("read ahead %d bytes of a 12 MiB segment in %d requests", covered, len(asked))
	}
}
