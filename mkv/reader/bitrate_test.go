package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestPromoteBPSBitrate covers surfacing the Matroska "BPS" per-track tag as the
// typed Track.Bitrate (matching ffprobe's bit_rate), keyed by the track UID.
func TestPromoteBPSBitrate(t *testing.T) {
	track := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackUID, 42, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
	)
	tags := masterElem(mkv.IDTags, masterElem(mkv.IDTag,
		masterElem(mkv.IDTargets, uintElem(mkv.IDTagTrackUID, 42, 1)),
		masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "BPS"), strElem(mkv.IDTagString, "128000")),
	))
	file := segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), tags, clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.Tracks[0].Bitrate == nil || *c.Tracks[0].Bitrate != 128000 {
		t.Errorf("Bitrate = %v, want 128000 from the BPS tag", c.Tracks[0].Bitrate)
	}
}

// TestPromoteBPSBitrateIgnoresJunk covers the guards: a global tag (no track UID)
// and a non-numeric BPS value leave Bitrate unset.
func TestPromoteBPSBitrateIgnoresJunk(t *testing.T) {
	track := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackUID, 7, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
	)
	tags := masterElem(mkv.IDTags,
		// Global tag (no Targets/UID): must be ignored.
		masterElem(mkv.IDTag, masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "BPS"), strElem(mkv.IDTagString, "999999"))),
		// Non-numeric per-track value: ignored.
		masterElem(mkv.IDTag,
			masterElem(mkv.IDTargets, uintElem(mkv.IDTagTrackUID, 7, 1)),
			masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "BPS"), strElem(mkv.IDTagString, "n/a")),
		),
	)
	file := segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), tags, clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.Tracks[0].Bitrate != nil {
		t.Errorf("Bitrate = %v, want nil (global + non-numeric tags ignored)", *c.Tracks[0].Bitrate)
	}
}
