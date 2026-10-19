package mkv

// RawElement is one child of a TrackEntry, Video or Audio element kept as read: Parent names the element it sits in,
// Data is its body.
type RawElement struct {
	Parent, ID uint32
	Data       []byte
}
