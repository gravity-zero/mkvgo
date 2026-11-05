package mp4

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestHLSFromCMAFBandwidthCountsTheAudio: a variant's BANDWIDTH is the peak of what it plays - its video and the
// heaviest audio of its group (RFC 8216 4.3.4.2), as every other master mkvgo writes says; the CMAF one gave the
// video's alone.
func TestHLSFromCMAFBandwidthCountsTheAudio(t *testing.T) {
	dir := cmafLadder(t)
	p := cmafPresentation(t, dir)
	o := optionsFrom(nil)
	video, audio, err := readCMAFPresentation(context.Background(), &o, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := HLSFromCMAF(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	heaviest := int64(0)
	for _, a := range audio {
		heaviest = max(heaviest, a.bandwidth)
	}
	for _, v := range video {
		if want := fmt.Sprintf("BANDWIDTH=%d,", v.bandwidth+heaviest); !strings.Contains(string(got["master.m3u8"]), want) {
			t.Errorf("no variant of %s says %s (video %d + audio %d):\n%s", v.id, want, v.bandwidth, heaviest, got["master.m3u8"])
		}
	}
}
