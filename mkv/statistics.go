package mkv

import (
	"strconv"
	"strings"
)

// TrackStatistics is what a track's statistics tags state, once trusted.
type TrackStatistics struct {
	DurationMs int64
	Frames     int64 // NUMBER_OF_FRAMES, 0 when absent
}

// TrustedTrackStatistics returns, by track number, the statistics of every
// track whose tags describe THIS file: written by the file's writing
// application, on its date when both are stated, with a duration not past the
// declared one (a tag copied verbatim by a remux certifies frames the file no
// longer holds).
func TrustedTrackStatistics(c *Container) map[uint64]TrackStatistics {
	out := map[uint64]TrackStatistics{}
	for i := range c.Tracks {
		t := &c.Tracks[i]
		uid := t.UID
		if uid == 0 {
			uid = t.ID
		}
		for _, tag := range c.Tags {
			if tag.TargetID == 0 || tag.TargetID != uid || !statisticsDescribeFile(c, tag.SimpleTags) {
				continue
			}
			durMs, err := ParseClockTime(SimpleTagValue(tag.SimpleTags, "DURATION"))
			if err != nil || durMs <= 0 || (c.DurationMs > 0 && durMs > c.DurationMs+1000) {
				continue
			}
			st := TrackStatistics{DurationMs: durMs}
			if n, err := strconv.ParseInt(strings.TrimSpace(SimpleTagValue(tag.SimpleTags, "NUMBER_OF_FRAMES")), 10, 64); err == nil && n > 0 {
				st.Frames = n
			}
			out[t.ID] = st
		}
	}
	return out
}

// statisticsDescribeFile checks a statistics tag set was measured by the application that wrote the file, on the same date when both say.
func statisticsDescribeFile(c *Container, tags []SimpleTag) bool {
	app := SimpleTagValue(tags, "_STATISTICS_WRITING_APP")
	if app == "" || app != c.Info.WritingApp {
		return false
	}
	if date := SimpleTagValue(tags, "_STATISTICS_WRITING_DATE_UTC"); date != "" && c.Info.DateUTC != nil {
		return date == c.Info.DateUTC.UTC().Format("2006-01-02 15:04:05")
	}
	return true
}

// SimpleTagValue returns the value of the named SimpleTag (case-insensitive), or "" when absent.
func SimpleTagValue(tags []SimpleTag, name string) string {
	for _, st := range tags {
		if strings.EqualFold(st.Name, name) {
			return st.Value
		}
	}
	return ""
}
