package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// cmafLadder packages two aligned quality variants (same synthetic content,
// different picture sizes, one shared audio track) with RemuxToABR and
// returns the presentation directory: v1/ and v2/ hold init.mp4 + seg*.m4s,
// v1/ also init_a1.mp4 + seg_a1_*.m4s, and manifest.mpd is combinedDASH's
// manifest over the same files - the reference the external-fragment path is
// compared with. Note the packager writes its own inits with a millisecond
// video timescale and no trex default duration (so no frame rate is
// recoverable from the init alone); an external encoder's init typically
// carries a 90 kHz or frame-based timescale and a default duration.
func cmafLadder(t *testing.T) string {
	t.Helper()
	sr := 44100.0 // the rate fakeASC declares, so the init and the track agree
	fr := 25.0
	ch := uint8(2)
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre", LanguageBCP47: "fr-CA"}
	hd := buildABRVariant(t, mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(1280), Height: u32(720), FrameRate: &fr}, audio)
	sd := buildABRVariant(t, mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360), FrameRate: &fr}, audio)
	dir := filepath.Join(t.TempDir(), "stream")
	if err := RemuxToABR(context.Background(), []string{hd, sd}, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cmafFiles lists the .m4s files of dir whose name starts with prefix, sorted.
func cmafFiles(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".m4s") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no %s*.m4s in %s", prefix, dir)
	}
	return names
}

// cmafPresentation is the ladder as external representations: v1 and v2
// video, v1's audio, with URLs relative to the ladder root.
func cmafPresentation(t *testing.T, dir string) CMAFPresentation {
	t.Helper()
	v1, v2 := filepath.Join(dir, "v1"), filepath.Join(dir, "v2")
	return CMAFPresentation{
		Video: []CMAFRepresentation{
			{Dir: v1, Init: "init.mp4", Segments: cmafFiles(t, v1, "seg0"), URLPrefix: "v1/"},
			{Dir: v2, Init: "init.mp4", Segments: cmafFiles(t, v2, "seg0"), URLPrefix: "v2/"},
		},
		Audio: []CMAFRepresentation{
			{Dir: v1, Init: "init_a1.mp4", Segments: cmafFiles(t, v1, "seg_a1_"), URLPrefix: "v1/"},
		},
	}
}

var bandwidthAttr = regexp.MustCompile(`bandwidth="\d+"`)

// TestDASHFromCMAF_Golden pins the manifest for the packaged ladder read back
// as external fragments. Bandwidth derives from segment byte sizes - the
// packager's business, not this manifest's - so it is normalised before the
// comparison. The golden was reviewed by hand: native timescales, exact tick
// timelines, the two rungs in one switch set, the audio in its own set with
// its BCP-47 language (the init's elng box) and channel configuration.
func TestDASHFromCMAF_Golden(t *testing.T) {
	dir := cmafLadder(t)
	got, err := DASHFromCMAF(context.Background(), cmafPresentation(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	const want = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011" mediaPresentationDuration="PT2.400S" minBufferTime="PT1.000S">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video" segmentAlignment="true" startWithSAP="1">
      <Representation id="v1" bandwidth="N" width="1280" height="720" codecs="avc1.64001F">
        <SegmentTemplate initialization="v1/init.mp4" media="v1/seg$Number%05d$.m4s" startNumber="1" timescale="1000">
          <SegmentTimeline>
            <S t="0" d="1000" r="1"/>
            <S t="2000" d="400"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
      <Representation id="v2" bandwidth="N" width="640" height="360" codecs="avc1.64001F">
        <SegmentTemplate initialization="v2/init.mp4" media="v2/seg$Number%05d$.m4s" startNumber="1" timescale="1000">
          <SegmentTimeline>
            <S t="0" d="1000" r="1"/>
            <S t="2000" d="400"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" contentType="audio" lang="fr-CA">
      <Representation id="a1" bandwidth="N" audioSamplingRate="44100" codecs="mp4a.40.2">
        <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="2"/>
        <SegmentTemplate initialization="v1/init_a1.mp4" media="v1/seg_a1_$Number%05d$.m4s" startNumber="1" timescale="44100">
          <SegmentTimeline>
            <S t="0" d="44100" r="1"/>
            <S t="88200" d="17640"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	norm := bandwidthAttr.ReplaceAllString(string(got), `bandwidth="N"`)
	if norm != want {
		t.Errorf("manifest differs from the golden:\n--- got ---\n%s\n--- want ---\n%s", norm, want)
	}
	// Bandwidth is derived (non-zero) for every representation.
	for _, m := range bandwidthAttr.FindAllString(string(got), -1) {
		if m == `bandwidth="0"` {
			t.Errorf("a representation has bandwidth 0: %s", got)
		}
	}
}

// The external-fragment manifest and the packager's own combined manifest
// describe the same files: same representations, same segment count, same
// presentation duration (the packager rounds to ms, the external path keeps
// ticks - within a millisecond).
func TestDASHFromCMAF_MatchesPackagerManifest(t *testing.T) {
	dir := cmafLadder(t)
	ref := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	got, err := DASHFromCMAF(context.Background(), cmafPresentation(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	mpd := string(got)
	for _, attr := range []string{`id="v1"`, `id="v2"`, `id="a1"`, `width="1280"`, `width="640"`, `codecs="avc1.64001F"`, `codecs="mp4a.40.2"`, `lang="fr-CA"`, `mediaPresentationDuration="PT2.400S"`,
		`<AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="2"/>`} {
		mustContain(t, ref, attr)
		mustContain(t, mpd, attr)
	}
	if a, b := strings.Count(ref, "<Representation "), strings.Count(mpd, "<Representation "); a != b {
		t.Errorf("representations: packager %d, external %d", a, b)
	}
	// The packager times every rendition in ms; the external path keeps each
	// track's own timescale - the same three segments either way.
	if n := strings.Count(ref, `<S t="0" d="1000" r="1"/>`); n != 3 {
		t.Errorf("packager timeline runs = %d, want 3 (v1, v2, a1)", n)
	}
	if n := strings.Count(mpd, `<S t="0" d="1000" r="1"/>`); n != 2 {
		t.Errorf("external video timeline runs = %d, want 2", n)
	}
	mustContain(t, mpd, `<S t="0" d="44100" r="1"/>`)
}

// Declared ID and Bandwidth override the derived ones; RewriteURL applies to
// every URI as for the packager's manifests.
func TestDASHFromCMAF_Overrides(t *testing.T) {
	dir := cmafLadder(t)
	p := cmafPresentation(t, dir)
	p.Video[0].ID, p.Video[0].Bandwidth = "hd", 123456
	p.Video[1].ID = "sd"
	got, err := DASHFromCMAF(context.Background(), p, Options{RewriteURL: func(n string) string { return "https://cdn/x/" + n }})
	if err != nil {
		t.Fatal(err)
	}
	mpd := string(got)
	for _, want := range []string{`id="hd" bandwidth="123456"`, `id="sd"`, `initialization="https://cdn/x/v1/init.mp4"`, `media="https://cdn/x/v2/seg$Number%05d$.m4s"`} {
		mustContain(t, mpd, want)
	}
}

// Rungs whose segments do not cover the same spans are refused before any
// manifest is emitted, with the segment and both spans named.
func TestDASHFromCMAF_RejectsMisalignedRungs(t *testing.T) {
	dir := cmafLadder(t)
	ctx := context.Background()

	t.Run("segment-count", func(t *testing.T) {
		p := cmafPresentation(t, dir)
		p.Video[1].Segments = p.Video[1].Segments[:len(p.Video[1].Segments)-1]
		_, err := DASHFromCMAF(ctx, p)
		if err == nil {
			t.Fatal("a rung with fewer segments must be refused")
		}
		for _, want := range []string{`"v2" has 2 segments`, `"v1" has 3`, "same boundaries"} {
			mustContain(t, err.Error(), want)
		}
	})

	t.Run("shifted-spans", func(t *testing.T) {
		// Same count, the second rung's timeline starts one segment later.
		p := cmafPresentation(t, dir)
		p.Video[0].Segments = p.Video[0].Segments[:2]
		p.Video[1].Segments = p.Video[1].Segments[1:]
		_, err := DASHFromCMAF(ctx, p)
		if err == nil {
			t.Fatal("shifted rungs must be refused")
		}
		for _, want := range []string{`"v2" segment 1 (v2/seg00002.m4s) covers 1.000 s to 2.000 s`, `"v1" covers 0.000 s to 1.000 s`, "same boundaries"} {
			mustContain(t, err.Error(), want)
		}
	})

	t.Run("track-layout", func(t *testing.T) {
		// A muxed rung (video + audio in one init) cannot switch with a
		// video-only rung.
		p := cmafPresentation(t, dir)
		p.Video = append(p.Video, spliceMuxedRung(t, dir))
		_, err := DASHFromCMAF(ctx, p)
		if err == nil {
			t.Fatal("mixed track layouts must be refused")
		}
		for _, want := range []string{`"v3" carries 1 video + 1 audio`, `"v1" carries 1 video`, "same kinds of tracks"} {
			mustContain(t, err.Error(), want)
		}
	})
}

// Files that are not what they are declared to be are refused with the
// representation, the segment and the reason named.
func TestDASHFromCMAF_RejectsBadFiles(t *testing.T) {
	dir := cmafLadder(t)
	ctx := context.Background()
	v1 := filepath.Join(dir, "v1")
	base := func() CMAFPresentation { return cmafPresentation(t, dir) }

	cases := []struct {
		name   string
		mutate func(p *CMAFPresentation)
		want   []string
	}{
		{"no-video", func(p *CMAFPresentation) { p.Video = nil }, []string{"at least one video representation"}},
		{"init-as-segment", func(p *CMAFPresentation) { p.Video[0].Segments = []string{"init.mp4"} },
			[]string{`"v1": segment 1 (init.mp4)`, "no moof box"}},
		{"segment-as-init", func(p *CMAFPresentation) { p.Video[0].Init = "seg00001.m4s" },
			[]string{`"v1"`, "no moov box"}},
		{"missing-file", func(p *CMAFPresentation) { p.Video[0].Segments[0] = "nope.m4s" },
			[]string{`"v1": segment 1 (nope.m4s)`}},
		{"duplicate-segment", func(p *CMAFPresentation) { p.Video[0].Segments[1] = p.Video[0].Segments[0] },
			[]string{"segment 2 (seg00001.m4s) starts at 0.000 s, before segment 1 ends (1.000 s)"}},
		{"audio-under-video", func(p *CMAFPresentation) { p.Video[0].Init = "init_a1.mp4" },
			[]string{"carries no video track", "under Audio"}},
		{"video-under-audio", func(p *CMAFPresentation) { p.Audio[0].Init = "init.mp4" },
			[]string{"carries a video track", "under Video"}},
		{"duplicate-id", func(p *CMAFPresentation) { p.Video[0].ID, p.Video[1].ID = "x", "x" },
			[]string{`id "x" is used twice`}},
		{"no-init", func(p *CMAFPresentation) { p.Video[0].Init = "" }, []string{"no initialisation segment"}},
		{"no-segments", func(p *CMAFPresentation) { p.Video[0].Segments = nil }, []string{"no media segments"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mutate(&p)
			_, err := DASHFromCMAF(ctx, p)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range c.want {
				mustContain(t, err.Error(), w)
			}
		})
	}

	t.Run("truncated-segment", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(v1, "seg00001.m4s"))
		if err != nil {
			t.Fatal(err)
		}
		// Cut inside the moof: the box header promises more than the file holds.
		moof := bytes.Index(data, []byte("moof"))
		if moof < 4 {
			t.Fatal("no moof in the packaged segment")
		}
		if err := os.WriteFile(filepath.Join(v1, "cut.m4s"), data[:moof+20], 0o644); err != nil {
			t.Fatal(err)
		}
		p := base()
		p.Video[0].Segments = []string{"cut.m4s"}
		_, err = DASHFromCMAF(ctx, p)
		if err == nil {
			t.Fatal("a truncated segment must be refused")
		}
		mustContain(t, err.Error(), "truncated")
	})

	t.Run("garbage-segment", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(v1, "junk.m4s"), bytes.Repeat([]byte{0xFF}, 64), 0o644); err != nil {
			t.Fatal(err)
		}
		p := base()
		p.Video[0].Segments = []string{"junk.m4s"}
		if _, err := DASHFromCMAF(ctx, p); err == nil {
			t.Fatal("garbage must be refused")
		}
	})

	t.Run("progressive-init", func(t *testing.T) {
		// A progressive MP4 (no mvex) is not an initialisation segment.
		src, _ := buildLacedFixture(t)
		prog := filepath.Join(v1, "prog.mp4")
		if err := RemuxToMP4(ctx, src, prog); err != nil {
			t.Fatal(err)
		}
		p := base()
		p.Video[0].Init = "prog.mp4"
		_, err := DASHFromCMAF(ctx, p)
		if err == nil {
			t.Fatal("a progressive file must be refused as init")
		}
		mustContain(t, err.Error(), "no mvex box")
	})
}

// Names that do not count up are listed one by one (SegmentList), under the
// main profile; the timeline and init stay the same.
func TestDASHFromCMAF_SegmentList(t *testing.T) {
	dir := cmafLadder(t)
	v1 := filepath.Join(dir, "v1")
	names := []string{"first.m4s", "second.m4s", "third.m4s"}
	for i, n := range cmafFiles(t, v1, "seg0") {
		data, err := os.ReadFile(filepath.Join(v1, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(v1, names[i]), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := CMAFPresentation{Video: []CMAFRepresentation{{Dir: v1, Init: "init.mp4", Segments: names}}}
	got, err := DASHFromCMAF(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	mpd := string(got)
	for _, want := range []string{
		`profiles="urn:mpeg:dash:profile:isoff-main:2011"`,
		`<SegmentList timescale="1000">`,
		`<Initialization sourceURL="init.mp4"/>`,
		`<S t="0" d="1000" r="1"/>`,
		`<SegmentURL media="first.m4s"/>`,
		`<SegmentURL media="third.m4s"/>`,
	} {
		mustContain(t, mpd, want)
	}
	if strings.Contains(mpd, "SegmentTemplate") {
		t.Error("arbitrary names must not be addressed by a template")
	}
}

// A muxed rung (video + audio in the same init and segments) is described
// with both codecs, timed on its video track.
func TestDASHFromCMAF_MuxedRepresentation(t *testing.T) {
	dir := cmafLadder(t)
	p := CMAFPresentation{Video: []CMAFRepresentation{spliceMuxedRung(t, dir)}}
	got, err := DASHFromCMAF(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	mpd := string(got)
	for _, want := range []string{`codecs="avc1.64001F,mp4a.40.2"`, `width="1280"`, `timescale="1000"`, `<S t="0" d="1000" r="1"/>`} {
		mustContain(t, mpd, want)
	}
	if strings.Contains(mpd, `contentType="audio"`) {
		t.Error("a muxed rung has no separate audio AdaptationSet")
	}
}

// spliceMuxedRung builds muxed/ from v1's demuxed video and audio files: one
// init whose moov carries both traks (the audio re-numbered track 2) and
// segments holding the video fragment followed by the audio fragment.
func spliceMuxedRung(t *testing.T, dir string) CMAFRepresentation {
	t.Helper()
	v1 := filepath.Join(dir, "v1")
	out := filepath.Join(dir, "muxed")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join(v1, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	top := func(data []byte) map[string]memBox {
		boxes, err := iterBoxes(data)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]memBox{}
		for _, b := range boxes {
			m[b.typ] = b
		}
		return m
	}
	const audioID = 2

	vInit, aInit := top(read("init.mp4")), top(read("init_a1.mp4"))
	vMoov, aMoov := top(vInit["moov"].payload), top(aInit["moov"].payload)
	// Re-number the audio track in place (tkhd track_ID, trex track_ID).
	tkhd := top(aMoov["trak"].payload)["tkhd"].payload
	idOff := 12
	if tkhd[0] == 1 {
		idOff = 20
	}
	binary.BigEndian.PutUint32(tkhd[idOff:], audioID)
	aTrex := top(aMoov["mvex"].payload)["trex"].payload
	binary.BigEndian.PutUint32(aTrex[4:], audioID)
	vTrex := top(vMoov["mvex"].payload)["trex"].payload
	moov := container("moov",
		box("mvhd", vMoov["mvhd"].payload),
		box("trak", vMoov["trak"].payload),
		box("trak", aMoov["trak"].payload),
		container("mvex", box("trex", vTrex), box("trex", aTrex)))
	init := append(box("ftyp", vInit["ftyp"].payload), moov...)
	if err := os.WriteFile(filepath.Join(out, "init.mp4"), init, 0o644); err != nil {
		t.Fatal(err)
	}

	vSegs, aSegs := cmafFiles(t, v1, "seg0"), cmafFiles(t, v1, "seg_a1_")
	if len(vSegs) != len(aSegs) {
		t.Fatalf("video %d segments, audio %d", len(vSegs), len(aSegs))
	}
	for i := range vSegs {
		a := top(read(aSegs[i]))
		tfhd := top(top(a["moof"].payload)["traf"].payload)["tfhd"].payload
		binary.BigEndian.PutUint32(tfhd[4:], audioID)
		seg := append(read(vSegs[i]), box("moof", a["moof"].payload)...)
		seg = append(seg, box("mdat", a["mdat"].payload)...)
		if err := os.WriteFile(filepath.Join(out, vSegs[i]), seg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return CMAFRepresentation{Dir: out, Init: "init.mp4", Segments: vSegs, URLPrefix: "muxed/"}
}

func TestNumberTemplate(t *testing.T) {
	cases := []struct {
		names []string
		media string
		start int64
		ok    bool
	}{
		{[]string{"seg00001.m4s", "seg00002.m4s", "seg00003.m4s"}, "seg$Number%05d$.m4s", 1, true},
		{[]string{"seg00001.m4s"}, "", 0, false}, // one name: nothing says which digits count
		{[]string{"seg_a1_00007.m4s", "seg_a1_00008.m4s"}, "seg_a1_$Number%05d$.m4s", 7, true},
		{[]string{"s9.m4s", "s10.m4s", "s11.m4s"}, "s$Number$.m4s", 9, true},
		{[]string{"08.m4s", "09.m4s", "10.m4s"}, "$Number%02d$.m4s", 8, true},
		{[]string{"chunk-1", "chunk-2"}, "chunk-$Number$", 1, true},
		{[]string{"v$1.m4s", "v$2.m4s"}, "v$$$Number$.m4s", 1, true},
		{[]string{"seg00001.m4s", "seg00003.m4s"}, "", 0, false}, // not consecutive
		{[]string{"a.m4s", "b.m4s"}, "", 0, false},               // no number
		{[]string{"x.m4s"}, "", 0, false},                        // no number
		{[]string{"seg00001.m4s", "seg2.m4s"}, "", 0, false},     // padded, mixed widths
		{[]string{"1a.m4s", "2b.m4s"}, "", 0, false},             // differ beyond the number
		{nil, "", 0, false},
	}
	for _, c := range cases {
		media, start, ok := numberTemplate(c.names)
		if media != c.media || start != c.start || ok != c.ok {
			t.Errorf("numberTemplate(%q) = (%q, %d, %v), want (%q, %d, %v)", c.names, media, start, ok, c.media, c.start, c.ok)
		}
	}
}

func TestDashTimelineSpans(t *testing.T) {
	// The millisecond wrapper renders exactly as before the generalisation.
	if got, want := dashTimeline([]float64{2, 2, 1.5}), "          <SegmentTimeline>\n            <S t=\"0\" d=\"2000\" r=\"1\"/>\n            <S t=\"4000\" d=\"1500\"/>\n          </SegmentTimeline>\n"; got != want {
		t.Errorf("dashTimeline:\n%s\nwant:\n%s", got, want)
	}
	// A gap breaks the run and restates t; equal durations after it run again.
	got := dashTimelineSpans([]int64{0, 100, 250, 350}, []int64{100, 100, 100, 100})
	want := "          <SegmentTimeline>\n            <S t=\"0\" d=\"100\" r=\"1\"/>\n            <S t=\"250\" d=\"100\" r=\"1\"/>\n          </SegmentTimeline>\n"
	if got != want {
		t.Errorf("dashTimelineSpans:\n%s\nwant:\n%s", got, want)
	}
}

func TestSameTicks(t *testing.T) {
	if !sameTicks(90000, 90000, 12800, 12800) {
		t.Error("1 s at 90 kHz and 1 s at 12.8 kHz are the same instant")
	}
	if sameTicks(90000, 90000, 12801, 12800) {
		t.Error("one tick off must not compare equal")
	}
	// Beyond 64-bit products: 2^40/2^30 == 2^41/2^31.
	if !sameTicks(1<<40, 1<<30, 1<<41, 1<<31) {
		t.Error("large values must compare without overflow")
	}
	if sameTicks(1<<40, 1<<30, 1<<41+1, 1<<31) {
		t.Error("large values one tick apart must differ")
	}
}

// TestHLSFromCMAF_Golden pins the HLS side over the same ladder: one master,
// one media playlist per representation, the same files referenced.
func TestHLSFromCMAF_Golden(t *testing.T) {
	dir := cmafLadder(t)
	got, err := HLSFromCMAF(context.Background(), cmafPresentation(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("playlists = %d, want 4 (master + v1 + v2 + a1): %v", len(got), got)
	}
	bwLine := regexp.MustCompile(`BANDWIDTH=\d+`)
	master := bwLine.ReplaceAllString(string(got["master.m3u8"]), "BANDWIDTH=N")
	const wantMaster = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="fr-CA",AUTOSELECT=YES,LANGUAGE="fr-CA",DEFAULT=YES,URI="a1.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=N,RESOLUTION=1280x720,CODECS="avc1.64001F,mp4a.40.2",AUDIO="aud"
v1.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=N,RESOLUTION=640x360,CODECS="avc1.64001F,mp4a.40.2",AUDIO="aud"
v2.m3u8
`
	if master != wantMaster {
		t.Errorf("master differs:\n--- got ---\n%s\n--- want ---\n%s", master, wantMaster)
	}
	const wantV2 = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:1
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="v2/init.mp4"
#EXTINF:1.000,
v2/seg00001.m4s
#EXTINF:1.000,
v2/seg00002.m4s
#EXTINF:0.400,
v2/seg00003.m4s
#EXT-X-ENDLIST
`
	if v2 := string(got["v2.m3u8"]); v2 != wantV2 {
		t.Errorf("v2.m3u8 differs:\n--- got ---\n%s\n--- want ---\n%s", v2, wantV2)
	}
	a1 := string(got["a1.m3u8"])
	for _, want := range []string{`#EXT-X-MAP:URI="v1/init_a1.mp4"`, "#EXTINF:1.000,\nv1/seg_a1_00001.m4s\n", "#EXTINF:0.400,\nv1/seg_a1_00003.m4s\n#EXT-X-ENDLIST\n"} {
		mustContain(t, a1, want)
	}
	// The same files, the same URIs as the DASH manifest over this ladder.
	mpd, err := DASHFromCMAF(context.Background(), cmafPresentation(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"v1/init.mp4", "v2/init.mp4", "v1/init_a1.mp4"} {
		mustContain(t, string(mpd), uri)
		mustContain(t, string(got["master.m3u8"])+string(got["v1.m3u8"])+string(got["v2.m3u8"])+a1, uri)
	}
}

// HLS carries one playlist per rung, so misaligned rungs are accepted there
// (a switch realigns on the next segment) while DASH refuses them; the
// per-representation checks still apply to both.
func TestHLSFromCMAF_AcceptsMisalignedRungs(t *testing.T) {
	dir := cmafLadder(t)
	ctx := context.Background()
	p := cmafPresentation(t, dir)
	p.Video[1].Segments = p.Video[1].Segments[:2]
	if _, err := DASHFromCMAF(ctx, p); err == nil {
		t.Fatal("DASH must refuse misaligned rungs")
	}
	got, err := HLSFromCMAF(ctx, p)
	if err != nil {
		t.Fatalf("HLS must accept misaligned rungs: %v", err)
	}
	if n := strings.Count(string(got["v2.m3u8"]), "#EXTINF"); n != 2 {
		t.Errorf("v2 segments = %d, want 2", n)
	}
	p.Video[0].Segments[1] = p.Video[0].Segments[0] // out of order: refused by both
	if _, err := HLSFromCMAF(ctx, p); err == nil || !strings.Contains(err.Error(), "playback order") {
		t.Errorf("HLS must refuse an out-of-order representation, got %v", err)
	}
	if _, err := HLSFromCMAF(ctx, CMAFPresentation{}); err == nil {
		t.Error("HLS must refuse a presentation without video")
	}
}

// RewriteURL applies to every playlist URI, as for the packager's playlists.
func TestHLSFromCMAF_RewriteURL(t *testing.T) {
	dir := cmafLadder(t)
	got, err := HLSFromCMAF(context.Background(), cmafPresentation(t, dir), Options{RewriteURL: func(n string) string { return "https://cdn/x/" + n }})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(got["master.m3u8"]), `URI="https://cdn/x/a1.m3u8"`)
	mustContain(t, string(got["master.m3u8"]), "\nhttps://cdn/x/v1.m3u8\n")
	mustContain(t, string(got["v1.m3u8"]), `#EXT-X-MAP:URI="https://cdn/x/v1/init.mp4"`)
	mustContain(t, string(got["v1.m3u8"]), "\nhttps://cdn/x/v1/seg00002.m4s\n")
}

// The channel configuration follows the codec: Dolby mask for AC-3/E-AC-3
// on a conventional layout, MPEG count otherwise, nothing when unknown.
func TestDashAudioChannelConfiguration(t *testing.T) {
	ch := func(n uint8) *uint8 { return &n }
	const mpeg = `<AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="%s"/>` + "\n"
	const dolby = `<AudioChannelConfiguration schemeIdUri="tag:dolby.com,2014:dash:audio_channel_configuration:2011" value="%s"/>` + "\n"
	cases := []struct {
		track mkv.Track
		want  string
	}{
		{mkv.Track{Codec: "aac", Channels: ch(2)}, fmt.Sprintf(mpeg, "2")},
		{mkv.Track{Codec: "opus", Channels: ch(6)}, fmt.Sprintf(mpeg, "6")},
		{mkv.Track{Codec: "ac3", Channels: ch(2)}, fmt.Sprintf(dolby, "A000")},
		{mkv.Track{Codec: "ac3", Channels: ch(6)}, fmt.Sprintf(dolby, "F801")},
		{mkv.Track{Codec: "eac3", Channels: ch(8)}, fmt.Sprintf(dolby, "FA01")},
		{mkv.Track{Codec: "eac3", Channels: ch(1)}, fmt.Sprintf(dolby, "4000")},
		{mkv.Track{Codec: "eac3", Channels: ch(7)}, fmt.Sprintf(mpeg, "7")}, // 6.1 or 7.0: no single layout, the count is honest
		{mkv.Track{Codec: "aac"}, ""},
		{mkv.Track{Codec: "ac3", Channels: ch(0)}, ""},
	}
	for _, c := range cases {
		if got := dashAudioChannelConfiguration(&c.track, ""); got != c.want {
			t.Errorf("%s/%v: got %q, want %q", c.track.Codec, c.track.Channels, got, c.want)
		}
	}
	if got := dashAudioChannelConfiguration(&mkv.Track{Codec: "aac", Channels: ch(2)}, "    "); !strings.HasPrefix(got, "    <Audio") {
		t.Errorf("indent not applied: %q", got)
	}
}
