package mp4

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// reviewFixture is a Matroska of an HEVC video (4000-byte frames, 25 fps) and the given audio and subtitle tracks:
// AAC tracks fed 50-byte frames every 32 ms, AC-3 ones 768-byte frames, each subtitle track a cue a second.
func reviewFixture(t *testing.T, audio, subs []mkv.Track) string {
	t.Helper()
	sr := 48000.0
	hvcC := append(codecstrHVCC(0, 0, 1, 0x60000000, [6]byte{0xB0, 0, 0, 0, 0, 0}, 93), make([]byte, 10)...)
	hvcC[0], hvcC[21], hvcC[22] = 1, 0xF3, 0 // version 1, 4-byte NAL lengths, no parameter set arrays
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Width: u32(640), Height: u32(360), CodecPrivate: hvcC}
	ac3 := make([]byte, 768)
	copy(ac3, makeAC3(0, 20, 8, 0, 7, 1))
	var blocks []genBlock
	for i := range 150 {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: make([]byte, 4000)})
	}
	for k := range audio {
		a := &audio[k]
		a.Type, a.SampleRate = mkv.AudioTrack, &sr
		for i := range 187 {
			data := cencAudioSample(i)
			if a.Codec == "ac3" {
				data = ac3
			}
			blocks = append(blocks, genBlock{track: a.ID, pts: int64(i) * 32, key: true, data: data})
		}
	}
	for k := range subs {
		subs[k].Type, subs[k].Codec = mkv.SubtitleTrack, "srt"
		for i := range 6 {
			blocks = append(blocks, genBlock{track: subs[k].ID, pts: int64(i) * 1000, key: true, data: []byte("cue")})
		}
	}
	sortGenBlocks(blocks)
	return buildMKV(t, append(append([]mkv.Track{video}, audio...), subs...), blocks)
}

func aacTrack(id uint64, lang string) mkv.Track {
	two := uint8(2)
	return mkv.Track{ID: id, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, Channels: &two, Language: lang}
}

func ac3Track(id uint64, lang string) mkv.Track {
	six := uint8(6)
	return mkv.Track{ID: id, Codec: "ac3", Channels: &six, Language: lang}
}

func toHLS(t *testing.T, src string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAudioGroupsKeepEveryLanguage: a French AAC and an English AC-3 stay in one group - split by codec, each
// variant would offer one language, and Safari lists only its variant's group - while French and English in both
// codecs split, each group holding both languages.
func TestAudioGroupsKeepEveryLanguage(t *testing.T) {
	m := readTextFile(t, filepath.Join(toHLS(t, reviewFixture(t, []mkv.Track{aacTrack(2, "fre"), ac3Track(3, "eng")}, nil)), "master.m3u8"))
	if n := strings.Count(m, "#EXT-X-STREAM-INF:"); n != 1 || strings.Count(m, `GROUP-ID="aud"`) != 2 ||
		!strings.Contains(m, `LANGUAGE="fre"`) || !strings.Contains(m, `LANGUAGE="eng"`) || !strings.Contains(m, `,mp4a.40.2,ac-3"`) {
		t.Errorf("fre AAC + eng AC-3: want one group with both languages, one variant naming both codecs:\n%s", m)
	}
	m = readTextFile(t, filepath.Join(toHLS(t, reviewFixture(t, []mkv.Track{aacTrack(2, "fre"), aacTrack(3, "eng"), ac3Track(4, "fre"), ac3Track(5, "eng")}, nil)), "master.m3u8"))
	if n := strings.Count(m, "#EXT-X-STREAM-INF:"); n != 2 || strings.Count(m, `GROUP-ID="aud-ac-3"`) != 2 || strings.Count(m, `GROUP-ID="aud-mp4a.40.2"`) != 2 {
		t.Errorf("fre and eng in both codecs: want one group per codec, each with both languages:\n%s", m)
	}
}

// TestSubtitleGroupOneDefault: subtitle tracks that all read as default (FlagDefault absent) give the "subs" group one
// DEFAULT=YES at most, as HLS requires - in to-hls, the ABR and the concat masters.
func TestSubtitleGroupOneDefault(t *testing.T) {
	subs := []mkv.Track{{ID: 3, Language: "fre", IsDefault: true}, {ID: 4, Language: "eng", IsDefault: true}, {ID: 5, Language: "ger", IsDefault: true}}
	src := reviewFixture(t, []mkv.Track{aacTrack(2, "fre")}, subs)
	count := func(m string) int {
		n := 0
		for _, l := range strings.Split(m, "\n") {
			if strings.HasPrefix(l, "#EXT-X-MEDIA:TYPE=SUBTITLES") && strings.Contains(l, "DEFAULT=YES") {
				n++
			}
		}
		return n
	}
	if m := readTextFile(t, filepath.Join(toHLS(t, src), "master.m3u8")); count(m) > 1 {
		t.Errorf("to-hls: %d subtitle defaults:\n%s", count(m), m)
	}
	abr := filepath.Join(t.TempDir(), "abr")
	if err := RemuxToABR(context.Background(), []string{src, src}, abr, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if m := readTextFile(t, filepath.Join(abr, "master.m3u8")); count(m) > 1 {
		t.Errorf("abr: %d subtitle defaults:\n%s", count(m), m)
	}
	cc := filepath.Join(t.TempDir(), "concat")
	if err := RemuxConcatToHLS(context.Background(), []string{src, src}, cc, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if m := readTextFile(t, filepath.Join(cc, "master.m3u8")); count(m) > 1 {
		t.Errorf("concat: %d subtitle defaults:\n%s", count(m), m)
	}
}

// segmentBits is the bits of each media segment of the rendition whose segments are named prefix%05d.m4s in dir.
func segmentBits(t *testing.T, dir, prefix string, n int) []float64 {
	t.Helper()
	out := make([]float64, n)
	for k := range n {
		st, err := os.Stat(filepath.Join(dir, prefix+strconv.FormatInt(int64(100000+k+1), 10)[1:]+".m4s"))
		if err != nil {
			t.Fatal(err)
		}
		out[k] = float64(st.Size()) * 8
	}
	return out
}

// TestBandwidthPerVariant: each HLS variant announces its video and the heaviest audio track of its own group, not
// every audio track's; the DASH video Representation announces the video alone.
func TestBandwidthPerVariant(t *testing.T) {
	dir := toHLS(t, reviewFixture(t, []mkv.Track{aacTrack(2, "fre"), aacTrack(3, "eng"), ac3Track(4, "fre"), ac3Track(5, "eng")}, nil))
	pl := readTextFile(t, filepath.Join(dir, "playlist.m3u8"))
	var durs []float64
	for _, m := range regexp.MustCompile(`#EXTINF:([0-9.]+),`).FindAllStringSubmatch(pl, -1) {
		d, _ := strconv.ParseFloat(m[1], 64)
		durs = append(durs, d)
	}
	n := len(durs)
	video := segmentBits(t, dir, "seg", n)
	peak := func(parts ...[]float64) int64 {
		var p float64
		for k := range n {
			var b float64
			for _, part := range parts {
				b += part[k]
			}
			p = max(p, b/durs[k])
		}
		return int64(p)
	}
	a := func(i int) []float64 { return segmentBits(t, dir, "seg_a"+strconv.Itoa(i)+"_", n) }
	heaviest := func(x, y []float64) []float64 { // a player plays one track of the group at a time
		out := make([]float64, n)
		for k := range n {
			out[k] = max(x[k], y[k])
		}
		return out
	}
	wantAAC, wantAC3 := peak(video, heaviest(a(1), a(2))), peak(video, heaviest(a(3), a(4)))
	m := readTextFile(t, filepath.Join(dir, "master.m3u8"))
	got := map[string]int64{}
	for _, l := range strings.Split(m, "\n") {
		if g := regexp.MustCompile(`^#EXT-X-STREAM-INF:BANDWIDTH=(\d+),.*AUDIO="([^"]+)"`).FindStringSubmatch(l); g != nil {
			got[g[2]], _ = strconv.ParseInt(g[1], 10, 64)
		}
	}
	if got["aud-mp4a.40.2"] != wantAAC || got["aud-ac-3"] != wantAC3 {
		t.Errorf("variant BANDWIDTH AAC %d, AC-3 %d; want %d (video + heaviest AAC), %d (video + heaviest AC-3)\n%s", got["aud-mp4a.40.2"], got["aud-ac-3"], wantAAC, wantAC3, m)
	}
	mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	if g := regexp.MustCompile(`<Representation id="v" bandwidth="(\d+)"`).FindStringSubmatch(mpd); g == nil || g[1] != strconv.FormatInt(peak(video), 10) {
		t.Errorf("DASH video Representation %v, want the video alone, %d", g, peak(video))
	}
}

// TestBandwidthHeaviestTrackOfGroup: a variant plays one audio track of its group at a time, so its BANDWIDTH and
// AVERAGE-BANDWIDTH count the video and the heaviest of the group's tracks, not their sum.
func TestBandwidthHeaviestTrackOfGroup(t *testing.T) {
	dir := toHLS(t, reviewFixture(t, []mkv.Track{aacTrack(2, "fre"), aacTrack(3, "eng")}, nil))
	pl := readTextFile(t, filepath.Join(dir, "playlist.m3u8"))
	var durs []float64
	for _, m := range regexp.MustCompile(`#EXTINF:([0-9.]+),`).FindAllStringSubmatch(pl, -1) {
		d, _ := strconv.ParseFloat(m[1], 64)
		durs = append(durs, d)
	}
	n := len(durs)
	video, fre, eng := segmentBits(t, dir, "seg", n), segmentBits(t, dir, "seg_a1_", n), segmentBits(t, dir, "seg_a2_", n)
	var peak, bits, secs float64
	for k := range n {
		b := video[k] + max(fre[k], eng[k])
		peak = max(peak, b/durs[k])
		bits += b
		secs += durs[k]
	}
	want := regexp.MustCompile(`BANDWIDTH=` + strconv.FormatInt(int64(peak), 10) + `,AVERAGE-BANDWIDTH=` + strconv.FormatInt(int64(bits/secs), 10) + `\b`)
	if m := readTextFile(t, filepath.Join(dir, "master.m3u8")); !want.MatchString(m) {
		t.Errorf("want BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d (video + the heaviest audio):\n%s", int64(peak), int64(bits/secs), m)
	}
}

// TestRenditionNamesUnique: renditions of one group never share a NAME - untitled tracks of one language get " 2",
// " 3" - in to-hls, the ABR and the concat masters.
func TestRenditionNamesUnique(t *testing.T) {
	src := reviewFixture(t, []mkv.Track{aacTrack(2, "fre"), aacTrack(3, "fre")},
		[]mkv.Track{{ID: 4, Language: "und"}, {ID: 5, Language: "und"}, {ID: 6, Language: "und"}})
	check := func(name, m string) {
		t.Helper()
		seen := map[string]bool{}
		for _, l := range strings.Split(m, "\n") {
			g := regexp.MustCompile(`^#EXT-X-MEDIA:TYPE=(\w+),GROUP-ID="([^"]+)",NAME="([^"]+)"`).FindStringSubmatch(l)
			if g == nil {
				continue
			}
			key := g[1] + "/" + g[2] + "/" + g[3]
			if seen[key] {
				t.Errorf("%s: NAME %q twice in group %s:\n%s", name, g[3], g[2], m)
			}
			seen[key] = true
		}
		if len(seen) != 5 {
			t.Errorf("%s: %d renditions, want 5:\n%s", name, len(seen), m)
		}
	}
	check("to-hls", readTextFile(t, filepath.Join(toHLS(t, src), "master.m3u8")))
	abr := filepath.Join(t.TempDir(), "abr")
	if err := RemuxToABR(context.Background(), []string{src, src}, abr, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	check("abr", readTextFile(t, filepath.Join(abr, "master.m3u8")))
	cc := filepath.Join(t.TempDir(), "concat")
	if err := RemuxConcatToHLS(context.Background(), []string{src, src}, cc, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	check("concat", readTextFile(t, filepath.Join(cc, "master.m3u8")))
}
