package mp4

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/poison"
	"github.com/gravity-zero/mkvgo/mkv"
)

// encryptionModes are the encrypted packagings Open streams: AES-128 with one
// key and with a key rotated every segment, CENC as cenc and as cbcs.
func encryptionModes() map[string]Options {
	key, kid := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	iv := []byte("ivivivivivivivi!")
	return map[string]Options{
		"aes-128": {Encrypt: &HLSEncryption{Key: key, KeyURI: "key.bin"}},
		"aes-128-rotating": {Encrypt: &HLSEncryption{RotateEverySegments: 1, Keys: []HLSKey{
			{Key: key, KeyURI: "k1.bin"}, {Key: kid, KeyURI: "k2.bin", IV: iv}}}},
		"cenc": {CENC: &CENCOptions{Scheme: "cenc", Key: key, KeyID: kid, IV: iv[:8], InlineKey: true}},
		"cbcs": {CENC: &CENCOptions{Scheme: "cbcs", Key: key, KeyID: kid, IV: iv, InlineKey: true}},
	}
}

// slicesMP4: 8 s of 320x240 H.264 coded in 4 slices per frame (4 NAL units,
// so 4 CENC subsamples a sample) and mono AAC, interleaved sample by sample.
//
//	ffmpeg -f lavfi -i testsrc2=size=320x240:rate=25 -f lavfi -i sine=frequency=660:sample_rate=48000 -t 8 \
//	  -c:v libx264 -preset veryfast -b:v 150k -g 50 -x264-params slices=4 -c:a aac -b:a 48k -ac 1 \
//	  -map_metadata -1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact slices.mp4
const slicesMP4 = "../internal/testdata/slices.mp4"

// encryptedSources are the MP4 fixtures and their Matroska remuxes.
func encryptedSources(t *testing.T) []string {
	t.Helper()
	srcs := []string{interleavedMP4, slicesMP4}
	for _, m := range []string{interleavedMP4, slicesMP4} {
		dst := filepath.Join(t.TempDir(), strings.TrimSuffix(filepath.Base(m), ".mp4")+".mkv")
		if err := RemuxFromMP4(context.Background(), m, dst); err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, dst)
	}
	return srcs
}

// Open streams the encrypted segments of MP4 and Matroska sources (real
// H.264, one and four slices a frame, and AAC) byte for byte like Resource -
// every sample's IV, counter and subsamples included: whole, a middle range
// and random ranges, every rendition, with no segment built in memory.
func TestOpenStreamsEncryptedSegments(t *testing.T) {
	copyFromSource(t)
	ctx := context.Background()
	for _, src := range encryptedSources(t) {
		for mode, o := range encryptionModes() {
			t.Run(filepath.Base(src)+"/"+mode, func(t *testing.T) {
				o.SegmentMs = 2000
				plan, err := PlanHLS(ctx, src, o)
				if err != nil {
					t.Fatal(err)
				}
				assertOpenMatchesResource(t, plan)
				if strings.HasPrefix(filepath.Base(src), "slices") && o.CENC != nil {
					h, err := plan.Open(ctx, "seg00002.m4s")
					if err != nil {
						t.Fatal(err)
					}
					for x, e := range h.cenc.td.entries {
						if o.CENC.Scheme == "cbcs" && len(e.subs) < 4 {
							t.Fatalf("sample %d: %d subsamples, want the 4 slices", x, len(e.subs))
						}
						for _, sub := range e.subs {
							if o.CENC.Scheme == "cenc" && sub.protected%16 != 0 {
								t.Fatalf("sample %d: %d protected bytes, not whole AES blocks", x, sub.protected)
							}
						}
					}
				}
				if st := plan.Stats(); st.StreamedSegments == 0 || st.StreamFallbacks != 0 {
					t.Errorf("stats: %+v", st)
				}
			})
		}
	}
}

// streamHashes is ffmpeg's per-stream sha256 of src's packets, keyed "v"/"a".
func streamHashes(t *testing.T, ffmpeg string, args ...string) map[string]string {
	t.Helper()
	out, err := exec.Command(ffmpeg, append(append([]string{"-v", "error"}, args...), "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-format_version", "1", "-")...).Output()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v", args, err)
	}
	h := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Split(l, ","); len(f) == 3 {
			h[f[1]] = f[2]
		}
	}
	return h
}

// Each encrypted mode, streamed through Open from an MP4 and a Matroska
// source, decrypts with ffmpeg to the source's packets: AES-128 through the
// HLS playlists and their key files, CENC with -decryption_key on each
// rendition's init and segments. Opt-in: MKVGO_FFMPEG names the ffmpeg to use.
func TestOpenEncryptedDecryptsWithFFmpeg(t *testing.T) {
	ffmpeg := os.Getenv("MKVGO_FFMPEG")
	if ffmpeg == "" {
		t.Skip("set MKVGO_FFMPEG to an ffmpeg binary")
	}
	ctx := context.Background()
	for _, src := range encryptedSources(t) {
		orig := interleavedMP4
		if strings.HasPrefix(filepath.Base(src), "slices") {
			orig = slicesMP4
		}
		want := streamHashes(t, ffmpeg, "-i", orig, "-map", "0:v", "-map", "0:a")
		for mode, o := range encryptionModes() {
			t.Run(filepath.Base(src)+"/"+mode, func(t *testing.T) {
				o.SegmentMs = 2000
				plan, err := PlanHLS(ctx, src, o)
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				for _, name := range plan.Resources() {
					h, err := plan.Open(ctx, name)
					if err != nil {
						t.Fatal(err)
					}
					f, err := os.Create(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.WriteTo(ctx, f); err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
				if o.Encrypt != nil {
					keys := o.Encrypt.Keys
					if len(keys) == 0 {
						keys = []HLSKey{{Key: o.Encrypt.Key, KeyURI: o.Encrypt.KeyURI}}
					}
					for _, k := range keys {
						if err := os.WriteFile(filepath.Join(dir, k.KeyURI), k.Key, 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				got := map[string]string{}
				for kind, pl := range map[string]string{"v": "playlist.m3u8", "a": "audio1.m3u8"} {
					var h map[string]string
					if o.Encrypt != nil {
						h = streamHashes(t, ffmpeg, "-allowed_extensions", "ALL", "-i", filepath.Join(dir, pl), "-map", "0:"+kind)
					} else {
						joined := filepath.Join(dir, kind+".mp4")
						concatRendition(t, dir, pl, joined)
						h = streamHashes(t, ffmpeg, "-decryption_key", hex.EncodeToString(o.CENC.Key), "-i", joined, "-map", "0:"+kind)
					}
					got[kind] = h[kind]
				}
				for kind := range want {
					if got[kind] != want[kind] {
						t.Errorf("%s stream: decrypted %s, source %s", kind, got[kind], want[kind])
					}
				}
			})
		}
	}
}

// concatRendition writes a media playlist's init segment and its segments, in order, to dst.
func concatRendition(t *testing.T, dir, playlist, dst string) {
	t.Helper()
	pl, err := os.ReadFile(filepath.Join(dir, playlist))
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, l := range strings.Split(string(pl), "\n") {
		if i := strings.Index(l, `#EXT-X-MAP:URI="`); i >= 0 {
			parts = append(parts, strings.TrimSuffix(l[i+len(`#EXT-X-MAP:URI="`):], `"`))
		} else if l != "" && !strings.HasPrefix(l, "#") {
			parts = append(parts, l)
		}
	}
	var all []byte
	for _, p := range parts {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	if err := os.WriteFile(dst, all, 0o644); err != nil {
		t.Fatal(err)
	}
}

// failingFS opens files read 4 KiB at a time whose reads fail once armed and
// past a byte budget.
type failingFS struct {
	armed  atomic.Bool
	budget atomic.Int64
	read   atomic.Int64 // bytes read once armed
}

func (f *failingFS) fs() *mkv.FS {
	return &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &failingFile{ReadSeekCloser: file, fs: f}, nil
	}}
}

type failingFile struct {
	mkv.ReadSeekCloser
	fs *failingFS
}

func (f *failingFile) Read(p []byte) (int, error) {
	p = p[:min(len(p), 4096)]
	if f.fs.armed.Load() {
		if left := f.fs.budget.Add(-int64(len(p))); left < 0 {
			return 0, errors.New("source read failed")
		}
		f.fs.read.Add(int64(len(p)))
	}
	return f.ReadSeekCloser.Read(p)
}

// A source that fails in the middle of an encrypted segment ends the write
// with an error after at most a prefix of the segment's ciphertext (what the
// write had flushed), never a plaintext byte; and a plan that cannot stream, falling back
// to building in memory, serves the same ciphertext as Resource.
func TestOpenEncryptedNeverServesPlaintext(t *testing.T) {
	ctx := context.Background()
	for mode, o := range encryptionModes() {
		t.Run(mode, func(t *testing.T) {
			o.SegmentMs = 2000
			ref, err := PlanHLS(ctx, slicesMP4, o)
			if err != nil {
				t.Fatal(err)
			}
			want, _, err := ref.Resource(ctx, "seg00002.m4s")
			if err != nil {
				t.Fatal(err)
			}
			plain, _, err := mustPlan(t, slicesMP4, Options{SegmentMs: 2000}).Resource(ctx, "seg00002.m4s")
			if err != nil {
				t.Fatal(err)
			}

			ff := &failingFS{}
			so := o
			so.FS, so.StreamFromFS = ff.fs(), true
			plan := mustPlan(t, slicesMP4, so)
			h, err := plan.Open(ctx, "seg00002.m4s")
			if err != nil {
				t.Fatal(err)
			}
			ff.budget.Store(int64(len(want)) / 3)
			ff.armed.Store(true)
			var got bytes.Buffer
			if _, err := h.WriteTo(ctx, &got); err == nil {
				t.Fatal("a source failing midway must fail the write")
			}
			if ff.read.Load() == 0 || got.Len() >= len(want) || !bytes.Equal(got.Bytes(), want[:got.Len()]) {
				t.Fatalf("source read %d bytes, then %d of %d bytes written: not a prefix of the ciphertext", ff.read.Load(), got.Len(), len(want))
			}

			fo := o
			fo.FS = countingFS(&readTally{}) // no StreamFromFS: Open builds in memory
			fb := mustPlan(t, slicesMP4, fo)
			fh, err := fb.Open(ctx, "seg00002.m4s")
			if err != nil {
				t.Fatal(err)
			}
			var all bytes.Buffer
			if _, err := fh.WriteTo(ctx, &all); err != nil {
				t.Fatal(err)
			}
			if fb.Stats().StreamFallbacks == 0 || !bytes.Equal(all.Bytes(), want) || bytes.Equal(all.Bytes(), plain) {
				t.Errorf("fallback: %d bytes, equal to the ciphertext %v, to the plaintext %v", all.Len(), bytes.Equal(all.Bytes(), want), bytes.Equal(all.Bytes(), plain))
			}
		})
	}
}

func mustPlan(t *testing.T, src string, o Options) *HLSPlan {
	t.Helper()
	p, err := PlanHLS(context.Background(), src, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Two encrypted requests in a row, different titles and keys: the second
// serves exactly its own bytes, none of the first's plaintext or ciphertext,
// and the pooled buffers they used come back zeroed.
func TestEncryptedStreamLeavesNothingInPooledBuffers(t *testing.T) {
	ctx := context.Background()
	modes := encryptionModes()
	first := modes["cenc"]
	first.SegmentMs = 2000
	second := modes["aes-128"]
	second.SegmentMs = 2000
	second.Encrypt = &HLSEncryption{Key: []byte("another key 16b!"), KeyURI: "k"}
	serve := func(src string, o Options) []byte {
		h, err := mustPlan(t, src, o).Open(ctx, "seg00002.m4s")
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := h.WriteTo(ctx, &b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	a := serve(slicesMP4, first)
	aPlain, _, err := mustPlan(t, slicesMP4, Options{SegmentMs: 2000}).Resource(ctx, "seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	wiped := byte(0)
	if poison.On {
		wiped = poison.Byte
	}
	for _, buf := range [][]byte{(*streamBufs.Get().(*[]byte)), (*stageBufs.Get().(*[]byte))[:stageBytes]} {
		if i := slices.IndexFunc(buf, func(c byte) bool { return c != 0 && c != wiped }); i >= 0 {
			t.Fatalf("a pooled buffer comes back holding byte %d of an encrypted request", i)
		}
	}
	b := serve(interleavedMP4, second)
	want, _, err := mustPlan(t, interleavedMP4, second).Resource(ctx, "seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, want) {
		t.Fatal("the second request does not serve its own bytes")
	}
	for _, prev := range [][]byte{a, aPlain} { // the media bytes, past the boxes every segment frames alike
		media := prev[bytes.Index(prev, []byte("mdat"))+4:]
		for i := 0; i+32 <= len(media); i += 32 {
			if bytes.Contains(b, media[i:i+32]) {
				t.Fatalf("32 media bytes at %d of the first request reappear in the second", i)
			}
		}
	}
}

// A large segment served in the clear, then a far shorter encrypted one of
// another source (cenc, AES-128): the pooled buffers the first filled with
// plaintext give the second none of it - it is exactly its own ciphertext.
func TestEncryptedAfterClearCarriesNoStaleBytes(t *testing.T) {
	ctx := context.Background()
	serve := func(src string, o Options) []byte {
		h, err := mustPlan(t, src, o).Open(ctx, "seg00001.m4s")
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := h.WriteTo(ctx, &b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	small := filepath.Join(cryptVecDir, "src.mkv")
	for _, mode := range []string{"cenc", "aes-128"} {
		clear := serve(interleavedMP4, Options{SegmentMs: 2000})
		enc := serve(small, cryptVecOptions(mode))
		want, _, err := mustPlan(t, small, cryptVecOptions(mode)).Resource(ctx, "seg00001.m4s")
		if err != nil {
			t.Fatal(err)
		}
		if len(enc) >= len(clear) || !bytes.Equal(enc, want) {
			t.Fatalf("%s: %d bytes after a %d-byte clear segment, its own ciphertext %v", mode, len(enc), len(clear), bytes.Equal(enc, want))
		}
		media := clear[bytes.Index(clear, []byte("mdat"))+4:]
		for i := 0; i+32 <= len(media); i += 16 {
			if bytes.Contains(enc, media[i:i+32]) {
				t.Fatalf("%s: 32 clear bytes at %d of the first segment reappear in the encrypted one", mode, i)
			}
		}
	}
}
