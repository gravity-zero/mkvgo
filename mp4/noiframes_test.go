package mp4

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// noIframeMKVSource is TestPlanHLSIframeMatroska's fixture: 6 s of H.264,
// a keyframe every second.
func noIframeMKVSource(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		key := i%25 == 0
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: iframeTestFrame(i, key)})
	}
	return buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks)
}

const iframeInf = "#EXT-X-I-FRAME-STREAM-INF:"

// readTree returns every file under dir by slash path.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Options.NoIFrames: no EXT-X-I-FRAME-STREAM-INF in any master, no
// iframe.m3u8 written, listed or served, for a Matroska and an MP4 source,
// single and ABR, full pass and on-demand plan. Every other resource is the
// same as without the option. Each case first checks the default does offer
// the I-frame stream, so the absence is the option's doing.
func TestNoIFrames(t *testing.T) {
	ctx := context.Background()
	sources := map[string]string{"mkv": noIframeMKVSource(t), "mp4": hlsplanMP4Source(t)}
	for kind, src := range sources {
		t.Run(kind+"/full pass", func(t *testing.T) {
			trees := map[bool]map[string][]byte{}
			for _, off := range []bool{false, true} {
				dir := t.TempDir()
				if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 1000, NoIFrames: off}); err != nil {
					t.Fatal(err)
				}
				trees[off] = readTree(t, dir)
			}
			on, off := trees[false], trees[true]
			if on["iframe.m3u8"] == nil || !bytes.Contains(on["master.m3u8"], []byte(iframeInf)) {
				t.Fatal("default full pass offers no I-frame stream: fixture does not exercise the option")
			}
			if _, ok := off["iframe.m3u8"]; ok {
				t.Error("iframe.m3u8 written with NoIFrames")
			}
			if bytes.Contains(off["master.m3u8"], []byte(iframeInf)) {
				t.Errorf("master with NoIFrames still declares the I-frame stream:\n%s", off["master.m3u8"])
			}
			// The master differs only by the I-frame line.
			var kept []string
			for _, l := range strings.SplitAfter(string(on["master.m3u8"]), "\n") {
				if !strings.HasPrefix(l, iframeInf) {
					kept = append(kept, l)
				}
			}
			if strings.Join(kept, "") != string(off["master.m3u8"]) {
				t.Errorf("master differs beyond the I-frame line:\n%s\nvs\n%s", on["master.m3u8"], off["master.m3u8"])
			}
			fragments := len(parseIframePlaylist(t, on["iframe.m3u8"]))
			for name, b := range on {
				if strings.HasPrefix(name, "iframe") || name == "master.m3u8" {
					continue
				}
				if !bytes.Equal(off[name], b) {
					t.Errorf("%s differs with NoIFrames", name)
				}
			}
			if fragments == 0 || len(off) != len(on)-1-fragments {
				t.Errorf("%d files with NoIFrames, %d without, of which %d I-frame fragments", len(off), len(on), fragments)
			}
		})

		t.Run(kind+"/plan", func(t *testing.T) {
			def, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
			if err != nil {
				t.Fatal(err)
			}
			if !containsName(def.Resources(), "iframe.m3u8") || !bytes.Contains(def.MasterPlaylist(), []byte(iframeInf)) {
				t.Fatal("default plan offers no I-frame stream")
			}
			plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000, NoIFrames: true})
			if err != nil {
				t.Fatal(err)
			}
			if containsName(plan.Resources(), "iframe.m3u8") {
				t.Error("Resources() lists iframe.m3u8 with NoIFrames")
			}
			if bytes.Contains(plan.MasterPlaylist(), []byte(iframeInf)) {
				t.Errorf("plan master declares the I-frame stream:\n%s", plan.MasterPlaylist())
			}
			if _, _, err := plan.Resource(ctx, "iframe.m3u8"); err == nil || !strings.Contains(err.Error(), "NoIFrames") {
				t.Errorf("Resource(iframe.m3u8) = %v, want the NoIFrames refusal", err)
			}
			if plan.iframe != nil || plan.iframes != nil {
				t.Error("I-frame playlist built behind a refusal")
			}
			// Every other resource is served and identical to the default plan's.
			for _, name := range def.Resources() {
				if strings.HasPrefix(name, "iframe") {
					if _, _, err := plan.Resource(ctx, name); err == nil || !strings.Contains(err.Error(), "NoIFrames") {
						t.Errorf("Resource(%s) = %v, want the NoIFrames refusal", name, err)
					}
					continue
				}
				if name == "master.m3u8" {
					continue
				}
				want, _, err := def.Resource(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				got, _, err := plan.Resource(ctx, name)
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s differs with NoIFrames (err %v)", name, err)
				}
			}
		})

		t.Run(kind+"/abr", func(t *testing.T) {
			pair := []string{src, src}
			for _, off := range []bool{false, true} {
				o := Options{SegmentMs: 1000, NoIFrames: off}
				dir := t.TempDir()
				if err := RemuxToABR(ctx, pair, dir, o); err != nil {
					t.Fatal(err)
				}
				tree := readTree(t, dir)
				plan, err := PlanABR(ctx, pair, o)
				if err != nil {
					t.Fatal(err)
				}
				var files []string
				for name := range tree {
					if strings.HasSuffix(name, "iframe.m3u8") {
						files = append(files, name)
					}
				}
				var listed []string
				for _, name := range plan.Resources() {
					if strings.HasSuffix(name, "iframe.m3u8") {
						listed = append(listed, name)
					}
				}
				full := bytes.Contains(tree["master.m3u8"], []byte(iframeInf))
				planned := bytes.Contains(plan.MasterPlaylist(), []byte(iframeInf))
				if !off {
					// The default offers trick-play somewhere (files or listing).
					if len(files)+len(listed) == 0 {
						t.Fatal("default ABR offers no I-frame playlist")
					}
					continue
				}
				if full || planned || len(files) > 0 || len(listed) > 0 {
					t.Errorf("ABR with NoIFrames: full master %v, plan master %v, files %v, listed %v", full, planned, files, listed)
				}
				if _, _, err := plan.Resource(ctx, "v1/iframe.m3u8"); err == nil {
					t.Error("PlanABR serves v1/iframe.m3u8 with NoIFrames")
				}
			}
		})
	}
}

// noIframeCounter counts the opens and bytes read through an FS.
type noIframeCounter struct {
	opens, bytes int64
}

func (c *noIframeCounter) fs() *mkv.FS {
	return &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		c.opens++
		return countingRSC{f: f, n: &c.bytes}, nil
	}}
}

// With NoIFrames, serving a 2160p Matroska presentation (plan, master, every
// media playlist, the init segment, the first media segment) reads exactly
// what the default plan reads when nobody asks for iframe.m3u8 - the option
// adds no read and removes none the I-frame stream did not cause - and a
// request for iframe.m3u8 opens nothing and reads 0 bytes, where the default
// plan walks the video track's keyframes for it.
func TestNoIFramesReadsNothingForIframes(t *testing.T) {
	w, h := uint32(3840), uint32(2160)
	const frameSize = 256 << 10
	var gblocks []genBlock
	for i := 0; i < 200; i++ { // 8 s at 40 ms, a keyframe every second
		key := i%25 == 0
		d := make([]byte, frameSize)
		copy(d, []byte{0, 0, 0, 1, 0x65, byte(i)})
		if !key {
			d[4] = 0x41
		}
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: key, data: d})
	}
	src := buildPlanFixture(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks, nil)
	ctx := context.Background()

	serve := func(noIFrames bool) (*HLSPlan, *noIframeCounter) {
		var c noIframeCounter
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000, FS: c.fs(), NoIFrames: noIFrames})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range plan.Resources() {
			if name == "iframe.m3u8" || strings.HasSuffix(name, ".m4s") && !strings.Contains(name, "00001") {
				continue // every other segment is left unrequested
			}
			if strings.HasSuffix(name, ".m4s") || strings.HasSuffix(name, ".m3u8") || strings.HasSuffix(name, ".mp4") {
				if _, _, err := plan.Resource(ctx, name); err != nil {
					t.Fatalf("Resource(%q): %v", name, err)
				}
			}
		}
		return plan, &c
	}

	defPlan, def := serve(false)
	noPlan, no := serve(true)
	if def.bytes == 0 || no.bytes != def.bytes || no.opens != def.opens {
		t.Fatalf("serving without iframe.m3u8: default %d bytes / %d opens, NoIFrames %d bytes / %d opens",
			def.bytes, def.opens, no.bytes, no.opens)
	}
	t.Logf("plan + master + playlists + init + segment 1: %d bytes, %d opens, either way", def.bytes, def.opens)

	before, opens := no.bytes, no.opens
	if _, _, err := noPlan.Resource(ctx, "iframe.m3u8"); err == nil {
		t.Fatal("iframe.m3u8 served with NoIFrames")
	}
	if no.bytes != before || no.opens != opens {
		t.Fatalf("refused iframe.m3u8 read %d bytes and opened %d files", no.bytes-before, no.opens-opens)
	}

	before = def.bytes
	if _, _, err := defPlan.Resource(ctx, "iframe.m3u8"); err != nil {
		t.Fatal(err)
	}
	if def.bytes == before {
		t.Fatal("default iframe.m3u8 read nothing: the fixture does not show the walk the option avoids")
	}
	t.Logf("default iframe.m3u8 walk: %d bytes; NoIFrames: 0", def.bytes-before)
}
