// Package trackchild knows which TrackEntry, Video and Audio children the Matroska writer emits from Track fields.
package trackchild

import "github.com/gravity-zero/mkvgo/mkv"

// written are the children of a TrackEntry, Video and Audio element the writer emits from Track fields;
// the reader keeps every other one as a RawElement, so a rewrite carries it over.
var written = map[uint32]map[uint32]bool{
	mkv.IDTrackEntry: {mkv.IDTrackNumber: true, mkv.IDTrackUID: true, mkv.IDTrackType: true, mkv.IDCodecID: true, mkv.IDCodecPrivate: true,
		mkv.IDDefaultDuration: true, mkv.IDLanguage: true, mkv.IDLanguageBCP47: true, mkv.IDName: true, mkv.IDCodecDelay: true,
		mkv.IDSeekPreRoll: true, mkv.IDFlagDefault: true, mkv.IDFlagForced: true, mkv.IDVideo: true, mkv.IDAudio: true,
		mkv.IDBlockAdditionMapping: true, mkv.IDContentEncodings: true},
	mkv.IDVideo: {mkv.IDPixelWidth: true, mkv.IDPixelHeight: true, mkv.IDDisplayWidth: true, mkv.IDDisplayHeight: true, mkv.IDColour: true,
		mkv.IDProjection: true},
	mkv.IDAudio: {mkv.IDSamplingFreq: true, mkv.IDOutputSamplingFreq: true, mkv.IDChannels: true, mkv.IDBitDepth: true},
}

// Written reports whether the writer emits child id of parent (a TrackEntry, Video or Audio ID) itself.
func Written(parent, id uint32) bool { return written[parent][id] }
