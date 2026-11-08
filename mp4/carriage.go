package mp4

import (
	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Carriage says whether a Matroska track goes into an MP4 (RemuxToMP4) and into an HLS/CMAF presentation
// (RemuxToHLS, PlanHLS, the growing plan), and why not, in the words Options.OnDrop reports: "" when the target
// carries it.
type Carriage struct {
	MP4 string `json:"mp4,omitempty"`
	HLS string `json:"hls,omitempty"`
}

// TracksCarriage is, per track of c in order, what RemuxToMP4 and an HLS presentation would do with it under
// opts (FlattenStyledSubs, NativeWebVTT, KeepTracks and the like change the answer): carried, or dropped for the
// reason given. It runs the operations' own track planning, head-only, and refuses nothing itself.
func TracksCarriage(c *mkv.Container, opts ...Options) ([]Carriage, error) {
	if err := oneopt.Check(opts); err != nil {
		return nil, err
	}
	o := optionsFrom(opts)
	o.SkipUnsupported, o.Strict = true, false
	index := make(map[uint64]int, len(c.Tracks))
	for i, t := range c.Tracks {
		index[t.ID] = i
	}
	out := make([]Carriage, len(c.Tracks))
	o.OnDrop = func(d DroppedTrack) {
		if i, ok := index[d.ID]; ok {
			out[i].MP4 = d.Reason
		}
	}
	_, _, _ = planTracks(c, o) // a plan that fails as a whole (no track left) has already said why per track
	o.OnDrop = func(d DroppedTrack) {
		if i, ok := index[d.ID]; ok {
			out[i].HLS = d.Reason
		}
	}
	if planned, _, err := planTracksFor(c, o, false); err == nil {
		_, _, _ = selectMedia(planned, &o, keepTrackSet(&o))
	}
	if !o.VideoOnly {
		_, _ = planSubTracks(c, o)
	}
	return out, nil
}
