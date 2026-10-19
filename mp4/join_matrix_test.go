package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// joinSources are the real fixtures a join of unrelated files runs on: B-frames, a QuickTime file, an audio edit list, keyframes between milliseconds, HEVC.
var joinSources = []string{"bframes_aac.mp4", "bframes_aac.mov", "negative_cts.mp4", "offset_ts48k.mp4", "fps30.mp4", "hevc_opengop.mp4"}

// chapterSet is what each file of a join case carries: none, two titled the same in every file, or enough for the join to pass 255.
type chapterSet int

const (
	noChapters chapterSet = iota
	twoChapters
	manyChapters
)

// TestJoinIndependentMatrix: three copies of a real fixture no split linked, one declaring other bitrates and the first with an unknown movie duration, join end to end with every chapter kept, shifted and capped at 255.
func TestJoinIndependentMatrix(t *testing.T) {
	for _, name := range joinSources {
		for _, rates := range []bool{false, true} {
			for _, unknown := range []bool{false, true} {
				for _, cs := range []chapterSet{noChapters, twoChapters, manyChapters} {
					t.Run(fmt.Sprintf("%s/rates=%v/unknown=%v/chapters=%d", name, rates, unknown, cs), func(t *testing.T) {
						var files []string
						var want []mkv.Chapter
						var offset, total int64
						for i := range 3 {
							f := copyFixture(t, name)
							withChapterSet(t, f, cs)
							m := mustMeta(t, f)
							for _, ch := range m.Chapters {
								ch.StartMs += offset
								if ch.EndMs > 0 {
									ch.EndMs += offset
								}
								want = append(want, ch)
							}
							offset += m.DurationMs
							total += m.DurationMs
							if rates && i == 1 {
								withRealDeclaredBitrates(t, f, 800_000)
							}
							if unknown && i == 0 {
								withUnknownMovieDuration(t, f)
							}
							files = append(files, f)
						}
						if len(want) > maxChapters {
							want = want[:maxChapters]
							want[maxChapters-1].EndMs = total // an MP4 stores starts: the last chapter kept runs to the end
						}
						dst := filepath.Join(t.TempDir(), "j"+filepath.Ext(name))
						if err := Join(context.Background(), files, dst); err != nil {
							t.Fatal(err)
						}
						checkJoinedEndToEnd(t, files, dst)
						jm := mustMeta(t, dst)
						if d := jm.DurationMs - total; d < -1 || d > 1 {
							t.Errorf("the join lasts %d ms, its files %d", jm.DurationMs, total)
						}
						if len(jm.Chapters) != len(want) {
							t.Fatalf("%d chapters, want %d", len(jm.Chapters), len(want))
						}
						for i, w := range want {
							g := jm.Chapters[i]
							if g.Title != w.Title || absMs(g.StartMs-w.StartMs) > 1 || absMs(g.EndMs-w.EndMs) > 1 {
								t.Errorf("chapter %d: %+v, want %+v", i+1, g, w)
							}
						}
						if n := chapterTrackCount(t, dst); len(want) > 0 && n != len(want) {
							t.Errorf("the chapter track holds %d samples, the chapters %d", n, len(want))
						}
						if tags := splitLinkTags(t, dst); len(tags) != 0 {
							t.Errorf("the join carries %v", tags)
						}
						sd, _ := os.ReadFile(files[0])
						jd, _ := os.ReadFile(dst)
						if ws, gs := trakGroups(t, sd), trakGroups(t, jd); fmt.Sprint(ws) != fmt.Sprint(gs) {
							t.Errorf("alternate groups %v, the first file's %v", gs, ws)
						}
					})
				}
			}
		}
	}
}

// TestJoinAudioGapMatrix: a file whose sound stops short of its picture joins the next one with its last audio sample held up to 500 ms, and is refused past it.
func TestJoinAudioGapMatrix(t *testing.T) {
	for _, name := range []string{"bframes_aac.mp4", "bframes_aac.mov", "negative_cts.mp4", "offset_ts48k.mp4"} {
		for _, gap := range []int64{100, 700} {
			for _, cs := range []chapterSet{noChapters, twoChapters} {
				t.Run(fmt.Sprintf("%s/gap=%d/chapters=%d", name, gap, cs), func(t *testing.T) {
					src := copyFixture(t, name)
					lead := soundLeadMs(t, src) // the next file's sound starts that late too
					short := shortSound(t, src, gap)
					withChapterSet(t, short, cs)
					withChapterSet(t, src, cs)
					dst := filepath.Join(t.TempDir(), "j"+filepath.Ext(name))
					err := Join(context.Background(), []string{short, src}, dst)
					if gap+lead > 500 { // the documented limit, not the constant
						if err == nil || !strings.Contains(err.Error(), "more than the 500 ms") {
							t.Errorf("a %d ms audio gap: %v", gap, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					checkJoinedEndToEnd(t, []string{short, src}, dst)
					if w, g := mustMeta(t, short).DurationMs+mustMeta(t, src).DurationMs, mustMeta(t, dst).DurationMs; absMs(g-w) > 1 {
						t.Errorf("the join lasts %d ms, want %d", g, w)
					}
					if want, got := 2*len(mustMeta(t, src).Chapters), len(mustMeta(t, dst).Chapters); got != want {
						t.Errorf("%d chapters, want %d", got, want)
					}
				})
			}
		}
	}
}

// TestJoinReadsTheQuickTimeWave: a MOV's esds sits in a wave box; its bitrates are ignored there too, its AudioSpecificConfig still compared.
func TestJoinReadsTheQuickTimeWave(t *testing.T) {
	a, b := copyFixture(t, "bframes_aac.mov"), copyFixture(t, "bframes_aac.mov")
	rewriteTailMoov(t, b, func(p []byte) []byte {
		i := strings.Index(string(p), "\x05\x80\x80\x80\x05") // DecoderSpecificInfo: the 5-byte AudioSpecificConfig follows
		if i < 0 || !strings.Contains(string(p), "wave") {
			t.Fatal("no AudioSpecificConfig inside a wave")
		}
		p = append([]byte(nil), p...)
		p[i+6] ^= 0x08 // another channel configuration
		return p
	})
	if err := Join(context.Background(), []string{a, b}, filepath.Join(t.TempDir(), "j.mov")); err == nil || !strings.Contains(err.Error(), "track 2 is encoded differently") {
		t.Errorf("join of MOVs with another AudioSpecificConfig: %v", err)
	}
}

// checkJoinedEndToEnd requires every track of dst to hold the samples of files one after the other, flags and bytes unchanged, less the priming and padding their edit lists hide at a seam.
func checkJoinedEndToEnd(t *testing.T, files []string, dst string) {
	t.Helper()
	got := sampleSeqs(t, dst)
	want := make([][]string, len(got))
	var prev *cutModel
	for k, f := range files {
		seqs := sampleSeqs(t, f)
		if len(seqs) != len(got) {
			t.Fatalf("%s has %d tracks, the join %d", filepath.Base(f), len(seqs), len(got))
		}
		m := mustCutModel(t, f)
		for ti, seq := range seqs {
			from, to := shownAtSeam(&m.tracks[ti], m.movieTS, k > 0, k < len(files)-1)
			want[ti] = append(want[ti], keptAtSeam(&m.tracks[ti], bodies(seq), from, to, unlinkedAfter(m, prev))...)
		}
		prev = m
	}
	for ti := range got {
		if g := bodies(got[ti]); strings.Join(g, ",") != strings.Join(want[ti], ",") {
			t.Errorf("track %d: the join does not hold its files end to end", ti+1)
			diffFirst(t, want[ti], g)
		}
	}
	// Independently of the seam rule above: every picture of file k presents after the files before it, at its own time.
	jm := mustCutModel(t, dst)
	jv := &jm.tracks[jm.ref]
	var offset int64
	idx := 0
	prev = nil
	for _, f := range files {
		m := mustCutModel(t, f)
		v := &m.tracks[m.ref]
		unlinked := unlinkedAfter(m, prev)
		for i := range v.tt.samples {
			if unlinked && leadingHidden(v, i) {
				continue
			}
			if idx >= len(jv.tt.samples) {
				t.Fatalf("the join holds %d pictures, fewer than its files", len(jv.tt.samples))
			}
			if want, got := offset+v.ptNs(i, m.movieTS), jv.ptNs(idx, jm.movieTS); absMs((got-want)/1_000_000) > 1 {
				t.Fatalf("%s picture %d presents at %d ms in the join, want %d", filepath.Base(f), i+1, got/1_000_000, want/1_000_000)
			}
			idx++
		}
		offset += m.movieNs
		prev = m
	}
	if idx != len(jv.tt.samples) {
		t.Fatalf("the join holds %d pictures, its files %d", len(jv.tt.samples), idx)
	}
}

// unlinkedAfter says whether file m follows prev without continuing it (the first file follows nothing).
func unlinkedAfter(m, prev *cutModel) bool {
	return prev != nil && !m.link.continues(prev.link)
}

// leadingHidden says whether sample i is a picture after the track's first keyframe that its edit list hides: a leading picture of an open GOP.
func leadingHidden(c *cutTrack, i int) bool {
	ss := c.tt.samples
	if ss[i].sync || ss[i].dts+int64(ss[i].cto) >= c.edit.mediaTime {
		return false
	}
	for _, x := range ss[:i] {
		if x.sync {
			return true
		}
	}
	return false
}

// keptAtSeam is seq[from:to] less, after a file it does not continue, the leading pictures its edit list hides.
func keptAtSeam(c *cutTrack, seq []string, from, to int, unlinked bool) []string {
	if !unlinked {
		return seq[from:to]
	}
	var out []string
	for i := from; i < to; i++ {
		if !leadingHidden(c, i) {
			out = append(out, seq[i])
		}
	}
	return out
}

// shownAtSeam is the run of an all-sync track a seam keeps: after the priming wholly ahead of its media start (head), up to the padding past its presentation end (tail).
func shownAtSeam(c *cutTrack, movieTS uint32, head, tail bool) (from, to int) {
	ss := c.tt.samples
	to = len(ss)
	for _, x := range ss {
		if !x.sync {
			return 0, to
		}
	}
	for head && from < to && ss[from].dts+int64(ss[from].cto)+int64(ss[from].dur) <= c.edit.mediaTime {
		from++
	}
	for tail && c.edit.segDur >= 0 && to > from && c.ptNs(to-1, movieTS) >= c.presentEndNs(movieTS) {
		to--
	}
	return from, to
}

// soundLeadMs is how late a file's audio track starts on its movie timeline.
func soundLeadMs(t *testing.T, path string) int64 {
	t.Helper()
	m := mustCutModel(t, path)
	for i := range m.tracks {
		if c := &m.tracks[i]; c.handler == "soun" {
			return c.ptNs(0, m.movieTS) / 1_000_000
		}
	}
	t.Fatal("no audio track")
	return 0
}

// withChapterSet gives a file the chapters cs names, spread over its length.
func withChapterSet(t *testing.T, path string, cs chapterSet) {
	t.Helper()
	n := map[chapterSet]int{twoChapters: 2, manyChapters: 100}[cs]
	if n == 0 {
		return
	}
	dur := mustMeta(t, path).DurationMs
	chs := make([]mkv.Chapter, n)
	for i := range chs {
		chs[i] = mkv.Chapter{ID: uint64(i + 1), Title: fmt.Sprintf("Chapter %d", i+1), StartMs: int64(i) * dur / int64(n)}
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Chapters = chs }); err != nil {
		t.Fatal(err)
	}
}

// shortSound muxes src's picture with its sound cut gap ms before the end, as a file whose audio stops early.
func shortSound(t *testing.T, src string, gap int64) string {
	t.Helper()
	ctx, dir, ext := context.Background(), t.TempDir(), filepath.Ext(src)
	var video, audio uint64
	for _, tr := range mustMeta(t, src).Tracks {
		switch tr.Type {
		case mkv.VideoTrack:
			video = tr.ID
		case mkv.AudioTrack:
			audio = tr.ID
		}
	}
	sound := filepath.Join(dir, "sound"+ext)
	if err := RemoveTrack(ctx, src, sound, []uint64{video}); err != nil {
		t.Fatal(err)
	}
	parts := splitInto(t, sound, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: mustMeta(t, src).DurationMs - gap}}})
	out := filepath.Join(dir, "short"+ext)
	if err := Mux(ctx, mkv.MuxOptions{OutputPath: out, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: video}, {SourcePath: parts[0], TrackID: 1}}}); err != nil {
		t.Fatal(err)
	}
	if audio == 0 {
		t.Fatal("no audio track")
	}
	return out
}

// withRealDeclaredBitrates sets the bitrates an encoder declares per file, in the video btrt and the esds, as a real clip of another length carries.
func withRealDeclaredBitrates(t *testing.T, path string, rate uint32) {
	t.Helper()
	btrt := box("btrt", binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 0), rate), rate))
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(_ int, trak []byte) []byte {
			out, err := rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
				var kids [][]byte
				for _, b := range sb {
					if b.typ != "stsd" {
						kids = append(kids, box(b.typ, b.payload))
						continue
					}
					entries, err := iterBoxes(b.payload[8:])
					if err != nil {
						return nil, err
					}
					var w bw
					w.bytes(b.payload[:8])
					for _, e := range entries {
						payload := append([]byte(nil), e.payload...)
						switch e.typ {
						case "avc1", "hvc1", "hev1":
							kept := payload[:78]
							inner, err := iterBoxes(payload[78:])
							if err != nil {
								return nil, err
							}
							for _, k := range inner {
								if k.typ != "btrt" {
									kept = append(kept, box(k.typ, k.payload)...)
								}
							}
							payload = append(kept, btrt...)
						case "mp4a":
							// DecoderConfigDescriptor: objectTypeIndication 0x40, streamType 0x15, bufferSizeDB (3 bytes), maxBitrate, avgBitrate.
							i := strings.Index(string(payload), "\x40\x15")
							if i < 0 {
								t.Fatal("no esds DecoderConfigDescriptor to patch")
							}
							binary.BigEndian.PutUint32(payload[i+5:], rate)
							binary.BigEndian.PutUint32(payload[i+9:], rate)
						}
						w.bytes(box(e.typ, payload))
					}
					kids = append(kids, box("stsd", w.b))
				}
				return kids, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		})
	})
}

func absMs(d int64) int64 {
	if d < 0 {
		return -d
	}
	return d
}

// TestJoinReadsOneWaveLevel: QuickTime puts one wave in a sample entry; one inside it is compared byte for byte,
// never walked (its btrt kept), and a nest of 100 000 is copied as it is: the walk no longer recurses at all.
func TestJoinReadsOneWaveLevel(t *testing.T) {
	describe := func(rate byte) []byte {
		var w bw
		decodingKids(&w, []memBox{{typ: "wave", payload: box("wave", box("btrt", []byte{0, 0, 0, rate}))}}, true)
		return w.b
	}
	if bytes.Equal(describe(1), describe(2)) {
		t.Error("a wave inside a wave was walked, its btrt dropped")
	}
	const depth = 100_000
	core := box("frma", []byte("mp4a"))
	nest := make([]byte, 8*depth+len(core))
	for i := range depth {
		binary.BigEndian.PutUint32(nest[8*i:], uint32(len(nest)-8*i))
		copy(nest[8*i+4:], "wave")
	}
	copy(nest[8*depth:], core)
	var w bw
	decodingKids(&w, []memBox{{typ: "wave", payload: nest}}, true)
	if !bytes.Contains(w.b, nest) {
		t.Error("the nested waves were not kept verbatim")
	}
}
