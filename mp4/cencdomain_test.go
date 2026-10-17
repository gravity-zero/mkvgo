package mp4

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestCENCTrackDomainsDistinct: two audio tracks whose IDs differ by 255 share no IV under one key: the
// presentation is refused rather than give both the same keystream.
func TestCENCTrackDomainsDistinct(t *testing.T) {
	w, h := uint32(64), uint32(48)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: cencAudioSample(i)})
		blocks = append(blocks, genBlock{track: 257, pts: int64(i) * 20, key: true, data: cencAudioSample(i + 1)})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"},
		{ID: 257, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
	}, blocks)
	o := Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "k"}}
	plan, err := PlanHLS(context.Background(), src, o)
	if err != nil {
		if !strings.Contains(err.Error(), "257") {
			t.Errorf("refused without naming the tracks: %v", err)
		}
		return
	}
	seen := map[string]string{}
	for _, name := range plan.Resources() {
		if !strings.HasSuffix(name, ".m4s") {
			continue
		}
		seg, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Fatal(name, err)
		}
		for i, s := range parseCENCSegmentFile(t, seg, 8).samples {
			k := fmt.Sprintf("%x", s.iv)
			if prev, ok := seen[k]; ok && !strings.HasPrefix(prev, name) {
				t.Fatalf("IV %s used by %s and %s#%d", k, prev, name, i)
			}
			seen[k] = fmt.Sprintf("%s#%d", name, i)
		}
	}
}
