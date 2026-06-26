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

// TestReadMetaBitrate covers the head-only WithBitrate path: a SeekHead at the head
// references the Tags element at the tail (the ffmpeg layout). Default ReadMeta
// leaves Bitrate nil; WithBitrate follows the SeekHead to that one element and fills
// Track.Bitrate from the BPS tag while keeping Tags nil.
func TestReadMetaBitrate(t *testing.T) {
	info := infoElem()
	tracks := masterElem(mkv.IDTracks, trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackUID, 42, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
	))
	cluster := clusterElem()
	tags := masterElem(mkv.IDTags, masterElem(mkv.IDTag,
		masterElem(mkv.IDTargets, uintElem(mkv.IDTagTrackUID, 42, 1)),
		masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "BPS"), strElem(mkv.IDTagString, "128000")),
	))

	// SeekHead (fixed-width 8-byte positions) referencing the tail Tags.
	l := uint64(len(seekHeadElem(seekEntry(mkv.IDInfo, 0), seekEntry(mkv.IDTracks, 0), seekEntry(mkv.IDTags, 0))))
	tagsPos := l + uint64(len(info)) + uint64(len(tracks)) + uint64(len(cluster))
	sh := seekHeadElem(seekEntry(mkv.IDInfo, l), seekEntry(mkv.IDTracks, l+uint64(len(info))), seekEntry(mkv.IDTags, tagsPos))
	if uint64(len(sh)) != l {
		t.Fatalf("SeekHead width changed: %d vs %d", len(sh), l)
	}
	file := segmentMKV(sh, info, tracks, cluster, tags)

	c, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if c.Tracks[0].Bitrate != nil {
		t.Errorf("default ReadMeta should leave Bitrate nil, got %d", *c.Tracks[0].Bitrate)
	}

	c, err = ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv", WithBitrate())
	if err != nil {
		t.Fatalf("ReadMeta WithBitrate: %v", err)
	}
	if c.Tracks[0].Bitrate == nil || *c.Tracks[0].Bitrate != 128000 {
		t.Errorf("WithBitrate Bitrate = %v, want 128000 from the BPS tag", c.Tracks[0].Bitrate)
	}
	if c.Tags != nil {
		t.Error("WithBitrate must keep Tags nil (metadata-path contract)")
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
