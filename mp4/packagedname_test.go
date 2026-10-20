package mp4

import (
	"math/rand/v2"
	"regexp"
	"testing"
)

// packagedNameSpec is the pattern isPackagedName spells out, kept here as its oracle.
var packagedNameSpec = regexp.MustCompile(`^((v|p)\d+/)?(master\.m3u8|playlist\.m3u8|iframe\.m3u8|audio\d+\.m3u8|sub\d+(_\d+)?\.(m3u8|vtt)|manifest\.mpd|init(_a\d+)?\.mp4|seg(_a\d+_)?\d+\.m4s|stream(_a\d+)?\.mp4|\.mkvgo-hls-t\d+\.tmp)$`)

// isPackagedName names exactly the files the pattern names: every name a packaging pass writes, nothing else.
func TestIsPackagedNameMatchesItsPattern(t *testing.T) {
	names := []string{
		"master.m3u8", "playlist.m3u8", "iframe.m3u8", "manifest.mpd", "audio1.m3u8", "audio12.m3u8", "sub1.m3u8",
		"sub2.vtt", "sub1_00003.vtt", "sub1_3.m3u8", "init.mp4", "init_a1.mp4", "seg00001.m4s", "seg_a1_00002.m4s",
		"stream.mp4", "stream_a2.mp4", ".mkvgo-hls-t7.tmp", "v1/master.m3u8", "v12/seg00001.m4s", "p3/init_a1.mp4",
		"", "master.m3u", "audio.m3u8", "audioX.m3u8", "sub.vtt", "sub1_.vtt", "sub1_a.vtt", "sub1.srt", "init_a.mp4",
		"init_1.mp4", "seg.m4s", "seg_a1.m4s", "seg_a1_.m4s", "seg_a_1.m4s", "seg1.mp4", "stream_a.mp4",
		".mkvgo-hls-t.tmp", "v/master.m3u8", "x1/master.m3u8", "v1master.m3u8", "v1/v2/master.m3u8", "p1.mp4",
		"seg١.m4s", "Master.m3u8", "master.m3u8/", "/master.m3u8", "playlist.m3u8x",
	}
	check := func(name string) {
		if got, want := isPackagedName(name), packagedNameSpec.MatchString(name); got != want {
			t.Errorf("isPackagedName(%q) = %v, the pattern says %v", name, got, want)
		}
	}
	for _, name := range names {
		check(name)
	}
	// Names drawn from the pattern's own grammar, half of them then altered by one character: both sides of the
	// boundary are drawn, the names packaged and those one edit away.
	r := rand.New(rand.NewPCG(1, 2))
	num := func() string { return []string{"0", "1", "07", "123"}[r.IntN(4)] }
	opt := func(s string) string {
		if r.IntN(2) == 0 {
			return s
		}
		return ""
	}
	forms := []func() string{
		func() string {
			return []string{"master.m3u8", "playlist.m3u8", "iframe.m3u8", "manifest.mpd"}[r.IntN(4)]
		},
		func() string { return "audio" + num() + ".m3u8" },
		func() string { return "sub" + num() + opt("_"+num()) + []string{".m3u8", ".vtt"}[r.IntN(2)] },
		func() string { return "init" + opt("_a"+num()) + ".mp4" },
		func() string { return "seg" + opt("_a"+num()+"_") + num() + ".m4s" },
		func() string { return "stream" + opt("_a"+num()) + ".mp4" },
		func() string { return ".mkvgo-hls-t" + num() + ".tmp" },
	}
	const alphabet = "vp0123456789/_a.msegtrxdi"
	matched := 0
	for range 200_000 {
		name := opt([]string{"v", "p"}[r.IntN(2)]+num()+"/") + forms[r.IntN(len(forms))]()
		if r.IntN(2) == 0 {
			b, k := []byte(name), r.IntN(len(name))
			switch r.IntN(3) {
			case 0:
				b[k] = alphabet[r.IntN(len(alphabet))]
			case 1:
				b = append(b[:k], b[k+1:]...)
			default:
				b = append(b[:k], append([]byte{alphabet[r.IntN(len(alphabet))]}, b[k:]...)...)
			}
			name = string(b)
		}
		if packagedNameSpec.MatchString(name) {
			matched++
		}
		check(name)
	}
	if matched < 50_000 || matched > 150_000 {
		t.Errorf("%d of the drawn names are packaged ones: the draw does not reach both sides of the boundary", matched)
	}
}
