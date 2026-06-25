package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestExtendedDispositions covers the extended Matroska disposition flags
// (commentary / hearing-impaired / visual-impaired / original / descriptions),
// mapping to the ffprobe stream dispositions of the same name.
func TestExtendedDispositions(t *testing.T) {
	track := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
		uintElem(mkv.IDFlagCommentary, 1, 1),
		uintElem(mkv.IDFlagHearingImpaired, 1, 1),
		uintElem(mkv.IDFlagOriginal, 1, 1),
	)
	file := segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tr := c.Tracks[0]
	if !tr.Commentary || !tr.HearingImpaired || !tr.Original {
		t.Errorf("set flags: commentary=%v hearing=%v original=%v, want all true", tr.Commentary, tr.HearingImpaired, tr.Original)
	}
	if tr.VisualImpaired || tr.TextDescriptions {
		t.Errorf("unset flags must be false: visual=%v descriptions=%v", tr.VisualImpaired, tr.TextDescriptions)
	}
}
