package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An audio Representation declares the track's own bit rate when anything
// knows it - its samples, else the container's figure (BPS / btrt) - and 0
// only when nothing does; the track title rides a Label, escaped.
func TestDASHAudioMetadata(t *testing.T) {
	bps := uint32(128000)
	withSamples := &fragTrack{outTrack: &outTrack{}, samples: []fragSample{{size: 4000, ptsMs: 0}, {size: 4000, ptsMs: 500}, {size: 4000, ptsMs: 1000}}}
	withSamples.outTrack.mkv.Bitrate = &bps
	if got := dashAudioBandwidth(withSamples); got != 96000 {
		t.Fatalf("12000 bytes over 1 s of samples: %d bit/s, want 96000", got)
	}
	tagged := &fragTrack{outTrack: &outTrack{}}
	tagged.outTrack.mkv.Bitrate = &bps
	if got := dashAudioBandwidth(tagged); got != 128000 {
		t.Fatalf("the container's BPS: %d, want 128000", got)
	}
	if got := dashAudioBandwidth(&fragTrack{outTrack: &outTrack{}}); got != 0 {
		t.Fatalf("nothing known: %d, want 0", got)
	}
	if got := dashLabel(&mkv.Track{Name: `Director's cut & "notes" <fr>`}, "  "); got != "  <Label>Director&#39;s cut &amp; &#34;notes&#34; &lt;fr&gt;</Label>\n" {
		t.Fatalf("label: %q", got)
	}
	if got := dashLabel(&mkv.Track{}, "  "); got != "" {
		t.Fatalf("no name, no label: %q", got)
	}
}

// The Label sits where the schema puts it - after ContentProtection - in an
// encrypted manifest, and a named subtitle track gets one too.
func TestDASHLabelPlacement(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	ch := uint8(2)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Name: "Commentary"}
	src := buildABRVariant(t, video, audio)
	dir := t.TempDir()
	cenc := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "k"}
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 1000, CENC: cenc}); err != nil {
		t.Fatal(err)
	}
	mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	audioSet := mpd[strings.Index(mpd, `contentType="audio"`):]
	cp, label := strings.Index(audioSet, "<ContentProtection"), strings.Index(audioSet, "<Label>Commentary</Label>")
	if cp < 0 || label < 0 {
		t.Fatalf("audio set lacks ContentProtection (%d) or Label (%d):\n%s", cp, label, audioSet)
	}
	if label < cp {
		t.Errorf("Label must follow ContentProtection (schema order):\n%s", audioSet)
	}
	// The single-variant manifest without CENC and the ABR one carry the
	// subtitle Label from the track name.
	subSrc := buildMKV(t, []mkv.Track{video, {ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Name: "Français"}}, subtitleFixtureBlocks())
	subDir := t.TempDir()
	if err := RemuxToHLS(ctx, subSrc, subDir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	mpd = readTextFile(t, filepath.Join(subDir, "manifest.mpd"))
	textSet := mpd[strings.Index(mpd, `mimeType="text/vtt"`):]
	if !strings.Contains(textSet, "<Label>Français</Label>") {
		t.Errorf("subtitle set lacks its Label:\n%s", textSet)
	}
}

// subtitleFixtureBlocks is a keyframed video plus a few text cues.
func subtitleFixtureBlocks() []genBlock {
	var blocks []genBlock
	for i := 0; i < 60; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := 0; i < 3; i++ {
		blocks = append(blocks, genBlock{track: 3, pts: int64(i) * 700, key: true, data: []byte("cue numero " + string(rune('1'+i)))})
	}
	sortGenBlocks(blocks)
	return blocks
}
