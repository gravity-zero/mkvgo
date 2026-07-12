package reader

import (
	"math"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// promoteTrackBitrate fills a track's Bitrate from the Matroska "BPS" tag (bits per
// second) that mainstream muxers write per track. This is the value probers expose as
// TAG:BPS - its own per-stream bit_rate field stays N/A for Matroska, so surfacing it
// as the typed Bitrate gives MORE than external probers here (for MP4 the Bitrate comes from
// btrt/esds and does equal the conventional bit_rate). The tag targets the track by its UID;
// a missing or non-numeric value is ignored, and a Bitrate already set from another
// source is kept. The full Read always reaches this; the metadata path does so only
// under WithBitrate, which follows the head SeekHead to the Tags element.
func promoteTrackBitrate(c *mkv.Container) {
	for _, tag := range c.Tags {
		if tag.TargetID == 0 {
			continue // a global tag, not a per-track bitrate
		}
		bps := bpsTagValue(tag.SimpleTags)
		if bps <= 0 {
			continue
		}
		for i := range c.Tracks {
			if t := &c.Tracks[i]; t.UID == tag.TargetID && t.Bitrate == nil {
				b := uint32(bps)
				t.Bitrate = &b
			}
		}
	}
}

// bpsTagValue returns the value of a "BPS" SimpleTag (bits per second), or 0 when
// absent or not a positive integer within uint32 range.
func bpsTagValue(tags []mkv.SimpleTag) int64 {
	for _, st := range tags {
		if !strings.EqualFold(st.Name, "BPS") {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(st.Value), 10, 64); err == nil && n > 0 && n <= math.MaxUint32 {
			return n
		}
	}
	return 0
}
