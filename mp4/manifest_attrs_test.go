package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// manifestFixture is a Matroska of an H.264 video and the given audio tracks (ID 2 on), each fed the sample of its
// codec, written to a temp file.
func manifestFixture(t *testing.T, video mkv.Track, audio []mkv.Track, samples []func(int) []byte) string {
	t.Helper()
	var blocks []genBlock
	for i := range 60 {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for k := range audio {
		for i := range 75 {
			blocks = append(blocks, genBlock{track: audio[k].ID, pts: int64(i) * 32, key: true, data: samples[k](i)})
		}
	}
	sortGenBlocks(blocks)
	return buildMKV(t, append([]mkv.Track{video}, audio...), blocks)
}

func manifestVideo() mkv.Track {
	return mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
}

func ac3Sample() func(int) []byte {
	f := make([]byte, 768)
	copy(f, makeAC3(0, 20, 8, 0, 7, 1))
	return func(int) []byte { return f }
}

// TestHLSAudioGroupPerCodec: audio tracks of two codecs, each language in both, form two groups, and the master has
// one variant per group, its CODECS naming the video and that group's codec - a player that cannot decode one picks
// the other, with every language still offered.
func TestHLSAudioGroupPerCodec(t *testing.T) {
	sr := 48000.0
	two, six := uint8(2), uint8(6)
	src := manifestFixture(t, manifestVideo(), []mkv.Track{
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two, Language: "eng"},
		{ID: 3, Type: mkv.AudioTrack, Codec: "ac3", SampleRate: &sr, Channels: &six, Language: "fre"},
		{ID: 4, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two, Language: "fre"},
		{ID: 5, Type: mkv.AudioTrack, Codec: "ac3", SampleRate: &sr, Channels: &six, Language: "eng"},
	}, []func(int) []byte{cencAudioSample, ac3Sample(), cencAudioSample, ac3Sample()})
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	m := readTextFile(t, filepath.Join(dir, "master.m3u8"))
	for _, want := range []string{
		`GROUP-ID="aud-mp4a.40.2"`, `GROUP-ID="aud-ac-3"`,
		`CODECS="avc1.64001F,mp4a.40.2",AUDIO="aud-mp4a.40.2"`, `CODECS="avc1.64001F,ac-3",AUDIO="aud-ac-3"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("master lacks %s:\n%s", want, m)
		}
	}
	if n := strings.Count(m, "#EXT-X-STREAM-INF:"); n != 2 {
		t.Errorf("%d variants, want one per audio group (2):\n%s", n, m)
	}
	if n := strings.Count(m, "DEFAULT=YES"); n != 2 {
		t.Errorf("%d defaults, want one per group (2):\n%s", n, m)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if pm := string(plan.MasterPlaylist()); strings.Count(pm, "#EXT-X-STREAM-INF:") != 2 || !strings.Contains(pm, `AUDIO="aud-ac-3"`) {
		t.Errorf("plan master is not split by codec:\n%s", pm)
	}
}

// TestManifestVideoRangeAndDolbyVision: an HDR video says its range - VIDEO-RANGE in HLS, the CICP properties in
// DASH - and a cross-compatible Dolby Vision stream its Dolby codec in SUPPLEMENTAL-CODECS.
func TestManifestVideoRangeAndDolbyVision(t *testing.T) {
	pq, hlg, bt2020, nc := uint16(16), uint16(18), uint16(9), uint16(9)
	for _, tc := range []struct {
		name string
		t    mkv.Track
		want string
	}{
		{"HDR10", mkv.Track{Codec: "hevc", ColorTransfer: &pq}, ",VIDEO-RANGE=PQ"},
		{"HLG", mkv.Track{Codec: "hevc", ColorTransfer: &hlg}, ",VIDEO-RANGE=HLG"},
		{"SDR", mkv.Track{Codec: "hevc"}, ""},
		{"DV 8.1", mkv.Track{Codec: "hevc", DolbyVision: &mkv.DolbyVision{Profile: 8, Level: 6, BLSignalCompatID: 1}},
			`,VIDEO-RANGE=PQ,SUPPLEMENTAL-CODECS="dvh1.08.06/db1p"`},
		{"DV 8.4", mkv.Track{Codec: "hevc", DolbyVision: &mkv.DolbyVision{Profile: 8, Level: 6, BLSignalCompatID: 4}},
			`,VIDEO-RANGE=HLG,SUPPLEMENTAL-CODECS="dvh1.08.06/db4h"`},
		{"DV 5", mkv.Track{Codec: "hevc", DolbyVision: &mkv.DolbyVision{Profile: 5, Level: 6}}, ",VIDEO-RANGE=PQ"},
	} {
		if got := hlsVideoAttrs(&tc.t); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	video := manifestVideo()
	video.ColorTransfer, video.ColorPrimaries, video.ColorSpace = &pq, &bt2020, &nc
	sr := 48000.0
	two := uint8(2)
	src := manifestFixture(t, video, []mkv.Track{{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two}},
		[]func(int) []byte{cencAudioSample})
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if m := readTextFile(t, filepath.Join(dir, "master.m3u8")); !strings.Contains(m, ",VIDEO-RANGE=PQ") {
		t.Errorf("to-hls master of an HDR10 video lacks VIDEO-RANGE=PQ:\n%s", m)
	}
	mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	for _, want := range []string{
		`<SupplementalProperty schemeIdUri="urn:mpeg:mpegB:cicp:ColourPrimaries" value="9"/>`,
		`<SupplementalProperty schemeIdUri="urn:mpeg:mpegB:cicp:TransferCharacteristics" value="16"/>`,
		`<SupplementalProperty schemeIdUri="urn:mpeg:mpegB:cicp:MatrixCoefficients" value="9"/>`,
	} {
		if !strings.Contains(mpd, want) {
			t.Errorf("manifest.mpd lacks %s:\n%s", want, mpd)
		}
	}
}

// TestManifestAccessibility: a track's disposition flags become CHARACTERISTICS in HLS and Accessibility / Role in
// DASH: audio description, hard of hearing, commentary, SDH and forced subtitles.
func TestManifestAccessibility(t *testing.T) {
	ad := mkv.Track{Language: "eng", VisualImpaired: true}
	hoh := mkv.Track{Language: "eng", HearingImpaired: true}
	comm := mkv.Track{Language: "eng", Commentary: true}
	if got := hlsAudioMedia(rendition{t: &ad}, 1, false, "aud", "a.m3u8"); !strings.Contains(got, `CHARACTERISTICS="public.accessibility.describes-video"`) {
		t.Errorf("audio description: %s", got)
	}
	if got := hlsAudioMedia(rendition{t: &hoh}, 1, false, "aud", "a.m3u8"); !strings.Contains(got, `CHARACTERISTICS="public.accessibility.enhances-speech-intelligibility"`) {
		t.Errorf("hard of hearing audio: %s", got)
	}
	sdh := mkv.Track{Language: "eng", HearingImpaired: true}
	if got := hlsSubtitleMedia(&sdh, 1, false, "s.m3u8"); !strings.Contains(got, `CHARACTERISTICS="public.accessibility.transcribes-spoken-dialog,public.accessibility.describes-music-and-sound"`) {
		t.Errorf("SDH subtitles: %s", got)
	}
	forced := mkv.Track{Language: "eng", IsForced: true}
	if got := hlsSubtitleMedia(&forced, 1, false, "s.m3u8"); !strings.Contains(got, "FORCED=YES") || strings.Contains(got, "CHARACTERISTICS") {
		t.Errorf("forced subtitles: %s", got)
	}
	for _, tc := range []struct {
		name string
		t    mkv.Track
		text bool
		want []string
	}{
		{"audio description", ad, false, []string{`<Accessibility schemeIdUri="urn:tva:metadata:cs:AudioPurposeCS:2007" value="1"/>`, `<Role schemeIdUri="urn:mpeg:dash:role:2011" value="description"/>`}},
		{"hard of hearing audio", hoh, false, []string{`<Accessibility schemeIdUri="urn:tva:metadata:cs:AudioPurposeCS:2007" value="2"/>`}},
		{"commentary", comm, false, []string{`<Role schemeIdUri="urn:mpeg:dash:role:2011" value="commentary"/>`}},
		{"main", mkv.Track{IsDefault: true}, false, []string{`value="main"`}},
		{"SDH", sdh, true, []string{`value="caption"`}},
		{"forced", forced, true, []string{`value="forced-subtitle"`}},
		{"plain subtitles", mkv.Track{}, true, []string{`value="subtitle"`}},
	} {
		got := dashRoles(&tc.t, tc.text, "")
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %s", tc.name, got, w)
			}
		}
	}

	// End to end: a commentary track is said so by both manifests of to-hls.
	sr := 48000.0
	two := uint8(2)
	src := manifestFixture(t, manifestVideo(), []mkv.Track{
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two, IsDefault: true},
		{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two, VisualImpaired: true},
	}, []func(int) []byte{cencAudioSample, cencAudioSample})
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if m := readTextFile(t, filepath.Join(dir, "master.m3u8")); !strings.Contains(m, "public.accessibility.describes-video") {
		t.Errorf("master lacks the audio description's characteristic:\n%s", m)
	}
	if mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd")); !strings.Contains(mpd, `value="description"`) || !strings.Contains(mpd, `value="main"`) {
		t.Errorf("manifest.mpd lacks the roles:\n%s", mpd)
	}
}

// eac3AtmosFrame is a 5.1 E-AC-3 syncframe (48 kHz, 6 blocks, 768 bytes) whose addbsi signals Dolby Atmos (JOC)
// with the given complexity index.
func eac3AtmosFrame(complexity uint32) []byte {
	var w bitWriter
	w.write(0x0B77, 16)
	w.write(0, 2)    // strmtyp: independent
	w.write(0, 3)    // substreamid
	w.write(383, 11) // frmsiz: 768 bytes
	w.write(0, 2)    // fscod 48 kHz
	w.write(3, 2)    // numblkscod: 6 blocks
	w.write(7, 3)    // acmod 3/2
	w.write(1, 1)    // lfeon
	w.write(16, 5)   // bsid
	w.write(31, 5)   // dialnorm
	w.write(0, 1)    // compre
	w.write(0, 1)    // mixmdate
	w.write(0, 1)    // infomdate
	w.write(1, 1)    // addbsie
	w.write(1, 6)    // addbsil: two bytes
	w.write(0, 7)
	w.write(1, 1) // flag_ec3_extension_type_a
	w.write(complexity, 8)
	f := w.bytes()
	return append(f, make([]byte, 768-len(f))...)
}

// TestEAC3Atmos: an E-AC-3 stream whose addbsi signals JOC carries it in its dec3 (flag_ec3_extension_type_a and the
// complexity index), and the manifests announce Atmos: CHANNELS="16/JOC" in HLS, the EC3_ExtensionType property in
// DASH. A plain 5.1 says neither.
func TestEAC3Atmos(t *testing.T) {
	if got := eac3JOC(eac3AtmosFrame(16)); got != 16 {
		t.Fatalf("eac3JOC = %d, want 16", got)
	}
	if got := eac3JOC(eac3SubFrame(0, 0, 7, 1, -1, 768)); got != 0 {
		t.Fatalf("eac3JOC of a plain 5.1 = %d, want 0", got)
	}
	six := uint8(6)
	entry, err := eac3Entry(&mkv.Track{ID: 2, Codec: "eac3", Channels: &six}, eac3AtmosFrame(16))
	if err != nil {
		t.Fatal(err)
	}
	if got := entryJOC(entry); got != 16 {
		t.Fatalf("dec3 carries complexity %d, want 16", got)
	}
	sr := 48000.0
	src := manifestFixture(t, manifestVideo(), []mkv.Track{{ID: 2, Type: mkv.AudioTrack, Codec: "eac3", SampleRate: &sr, Channels: &six}},
		[]func(int) []byte{func(int) []byte { return eac3AtmosFrame(16) }})
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if m := readTextFile(t, filepath.Join(dir, "master.m3u8")); !strings.Contains(m, `CHANNELS="16/JOC"`) {
		t.Errorf("master does not announce Atmos:\n%s", m)
	}
	if mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd")); !strings.Contains(mpd, `schemeIdUri="tag:dolby.com,2018:dash:EC3_ExtensionType:2018" value="JOC"`) {
		t.Errorf("manifest.mpd does not announce Atmos:\n%s", mpd)
	}
}
