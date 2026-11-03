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

// entryChannels is the channelcount of the first AudioSampleEntry of type typ in data, -1 when there is none.
func entryChannels(data []byte, typ string) int {
	i := bytes.Index(data, []byte(typ))
	if i < 4 || i+4+24 > len(data) {
		return -1
	}
	return int(binary.BigEndian.Uint16(data[i-4+24:]))
}

// TestAnnouncedChannelsFollowTheStream: a Matroska track whose container says
// 2 channels while its stream is 5.1 (the AAC AudioSpecificConfig, the AC-3
// frame header) is announced as 6 channels - the init's AudioSampleEntry, the
// HLS CHANNELS attribute, the DASH AudioChannelConfiguration - by to-mp4,
// to-hls, the on-demand plan and the ABR packaging alike: what the decoder
// configuration in the same init says.
func TestAnnouncedChannelsFollowTheStream(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	two := uint8(2)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
	ac3 := make([]byte, 768) // 192 kbit/s at 48 kHz
	copy(ac3, makeAC3(0, 20, 8, 0, 7, 1))
	cases := []struct {
		name, typ string
		audio     mkv.Track
		sample    func(int) []byte
	}{
		{"aac", "mp4a", mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0xB0}, SampleRate: &sr, Channels: &two}, cencAudioSample},
		{"ac3", "ac-3", mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "ac3", SampleRate: &sr, Channels: &two}, func(int) []byte { return ac3 }},
	}
	for _, c := range cases {
		var blocks []genBlock
		for i := range 60 {
			blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
		}
		for i := range 75 {
			blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 32, key: true, data: c.sample(i)})
		}
		sortGenBlocks(blocks)
		src := buildMKV(t, []mkv.Track{video, c.audio}, blocks)
		dir := t.TempDir()

		out := filepath.Join(dir, "out.mp4")
		if err := RemuxToMP4(ctx, src, out); err != nil {
			t.Fatalf("%s: to-mp4: %v", c.name, err)
		}
		if got := entryChannels(readBytes(t, out), c.typ); got != 6 {
			t.Errorf("%s: to-mp4 %s channelcount %d, want 6", c.name, c.typ, got)
		}

		hls := filepath.Join(dir, "hls")
		if err := RemuxToHLS(ctx, src, hls, Options{SegmentMs: 1000}); err != nil {
			t.Fatalf("%s: to-hls: %v", c.name, err)
		}
		var inits []byte
		entries, _ := os.ReadDir(hls)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".mp4") {
				inits = append(inits, readBytes(t, filepath.Join(hls, e.Name()))...)
			}
		}
		if got := entryChannels(inits, c.typ); got != 6 {
			t.Errorf("%s: to-hls %s channelcount %d, want 6", c.name, c.typ, got)
		}
		if m := readTextFile(t, filepath.Join(hls, "master.m3u8")); strings.Contains(m, "CHANNELS=") && !strings.Contains(m, `CHANNELS="6"`) {
			t.Errorf("%s: to-hls master announces other than 6 channels:\n%s", c.name, m)
		}

		plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
		if err != nil {
			t.Fatalf("%s: plan: %v", c.name, err)
		}
		if m := string(plan.MasterPlaylist()); !strings.Contains(m, `CHANNELS="6"`) {
			t.Errorf("%s: plan master does not announce 6 channels:\n%s", c.name, m)
		}
		inits = nil
		for _, name := range plan.Resources() {
			if strings.HasSuffix(name, ".mp4") {
				b, _, err := plan.Resource(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				inits = append(inits, b...)
			}
		}
		if got := entryChannels(inits, c.typ); got != 6 {
			t.Errorf("%s: plan %s channelcount %d, want 6", c.name, c.typ, got)
		}

		abr := filepath.Join(dir, "abr")
		if err := RemuxToABR(ctx, []string{src, src}, abr, Options{SegmentMs: 1000}); err != nil {
			t.Fatalf("%s: abr: %v", c.name, err)
		}
		if m := readTextFile(t, filepath.Join(abr, "master.m3u8")); !strings.Contains(m, `CHANNELS="6"`) {
			t.Errorf("%s: abr master does not announce 6 channels:\n%s", c.name, m)
		}
		if mpd := readTextFile(t, filepath.Join(abr, "manifest.mpd")); !strings.Contains(mpd, `value="6"`) && !strings.Contains(mpd, `value="F801"`) {
			t.Errorf("%s: abr manifest does not announce 6 channels:\n%s", c.name, mpd)
		}
	}
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAnnouncedChannelsImplicitPS: an AAC config that says LC mono without
// signalling PS can be HE-AACv2 with implicit PS - the decoder outputs stereo -
// so a container stating 2 channels keeps its 2; an explicit AOT 29 (PS) is
// 2 from the config itself.
func TestAnnouncedChannelsImplicitPS(t *testing.T) {
	sr := 48000.0
	one, two := uint8(1), uint8(2)
	cases := []struct {
		name      string
		asc       []byte
		container *uint8
		want      int
	}{
		{"LC mono, container 2", []byte{0x11, 0x88}, &two, 2},
		{"LC mono, container 1", []byte{0x11, 0x88}, &one, 1},
		{"LC mono, container silent", []byte{0x11, 0x88}, nil, 1},
		{"AOT 29 (PS), container 2", []byte{0xEB, 0x09, 0x88}, &two, 2},
		{"AOT 29 (PS), container 1", []byte{0xEB, 0x09, 0x88}, &one, 2},
	}
	for _, c := range cases {
		tr := &mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: c.asc, SampleRate: &sr, Channels: c.container}
		entry, err := aacEntry(tr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := entryChannels(entry, "mp4a"); got != c.want {
			t.Errorf("%s: AudioSampleEntry channelcount %d, want %d", c.name, got, c.want)
		}
		read := *tr // as the reader leaves it under WithInBandChannels: the config's count in StreamChannels
		if ch := aacChannels(c.asc); ch > 0 {
			read.StreamChannels = &ch
		}
		if got, want := hlsChannelsAttr(&read), fmt.Sprintf(`,CHANNELS="%d"`, c.want); got != want {
			t.Errorf("%s: HLS attribute %q, want %q", c.name, got, want)
		}
	}
}

// TestAnnouncedChannelsLateAudioTrack: an audio track whose first block comes after more than the in-band read's
// wait of other tracks' blocks is still announced in the master playlist with the channels its init states.
func TestAnnouncedChannelsLateAudioTrack(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	two := uint8(2)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "ac3", SampleRate: &sr, Channels: &two}
	ac3 := make([]byte, 768)
	copy(ac3, makeAC3(0, 20, 8, 0, 7, 1))
	var blocks []genBlock
	for i := range 400 {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := range 100 {
		blocks = append(blocks, genBlock{track: 2, pts: 8000 + int64(i)*32, key: true, data: ac3})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{video, audio}, blocks)

	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var inits []byte
	for _, name := range plan.Resources() {
		if strings.HasSuffix(name, ".mp4") {
			b, _, err := plan.Resource(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			inits = append(inits, b...)
		}
	}
	init := entryChannels(inits, "ac-3")
	if m := string(plan.MasterPlaylist()); !strings.Contains(m, fmt.Sprintf(`CHANNELS="%d"`, init)) {
		t.Errorf("plan: the init says %d channels, the master:\n%s", init, m)
	}
	hls := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(ctx, src, hls, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if m := readTextFile(t, filepath.Join(hls, "master.m3u8")); strings.Contains(m, "CHANNELS=") && !strings.Contains(m, fmt.Sprintf(`CHANNELS="%d"`, init)) {
		t.Errorf("to-hls: the init says %d channels, the master:\n%s", init, m)
	}
}
