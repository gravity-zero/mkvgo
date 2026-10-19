package mkv

// RawElement is one child of a TrackEntry, Video or Audio element kept as read: Parent names the element it sits in,
// Data is its body.
type RawElement struct {
	Parent, ID uint32
	Data       []byte
}

// writtenTrackChildren are the children of a TrackEntry, Video and Audio element the writer emits from Track fields;
// the reader keeps every other one as a RawElement, so a rewrite carries it over.
var writtenTrackChildren = map[uint32]map[uint32]bool{
	IDTrackEntry: {IDTrackNumber: true, IDTrackUID: true, IDTrackType: true, IDCodecID: true, IDCodecPrivate: true,
		IDDefaultDuration: true, IDLanguage: true, IDLanguageBCP47: true, IDName: true, IDCodecDelay: true,
		IDSeekPreRoll: true, IDFlagDefault: true, IDFlagForced: true, IDVideo: true, IDAudio: true,
		IDBlockAdditionMapping: true, IDContentEncodings: true},
	IDVideo: {IDPixelWidth: true, IDPixelHeight: true, IDDisplayWidth: true, IDDisplayHeight: true, IDColour: true,
		IDProjection: true},
	IDAudio: {IDSamplingFreq: true, IDOutputSamplingFreq: true, IDChannels: true, IDBitDepth: true},
}

// WrittenTrackChild reports whether the writer emits child id of parent (a TrackEntry, Video or Audio ID) itself.
func WrittenTrackChild(parent, id uint32) bool { return writtenTrackChildren[parent][id] }
