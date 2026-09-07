package ops

import "github.com/gravity-zero/mkvgo/mkv"

// cueTimeRoundTripMs returns what a cue time of ms milliseconds reads back as
// after a rewrite: WriteCues truncates milliseconds into the file's timecode
// units, and the reader truncates them back. Under the 1 ms default the two
// truncations cancel and the value is unchanged; under any other timebase the
// round trip loses up to one unit plus one millisecond.
//
// This is the difference between the two ways a cue time can legitimately be
// written for the same keyframe, and code that compares cue times against block
// timecodes has to accept both:
//
//   - a muxer that stores the keyframe's own timecode units reads back as the
//     block timecode exactly;
//   - a rewrite that goes through the millisecond API (ours) reads back as this
//     round trip of it.
//
// Treating the second as a mismatch reported a freshly rebuilt index as stale.
func cueTimeRoundTripMs(ms, scale int64) int64 {
	if scale <= 0 {
		scale = mkv.DefaultTimecodeScale
	}
	if scale == mkv.DefaultTimecodeScale || ms < 0 {
		return ms
	}
	return ms * mkv.DefaultTimecodeScale / scale * scale / mkv.DefaultTimecodeScale
}
