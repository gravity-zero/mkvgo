package mp4

import "github.com/gravity-zero/mkvgo/mkv"

// options.go — the option type shared by RemuxToMP4 and RemuxFromMP4.

// DroppedTrack describes a source track that the remux did not carry into the
// output, with a human-readable reason. It is reported through Options.OnDrop.
type DroppedTrack struct {
	ID     uint64
	Type   mkv.TrackType
	Codec  string
	Reason string
}

// Options configures a remux. The zero value is valid: the real OS filesystem,
// no progress reporting, and a strict policy that fails on any track whose codec
// cannot be carried (so the output never silently omits content).
type Options struct {
	// FS, when non-nil, replaces direct OS filesystem access.
	FS *mkv.FS
	// Progress, when non-nil, receives processed/total byte counts.
	Progress mkv.ProgressFunc
	// SkipUnsupported drops audio/video tracks whose codec cannot be carried in
	// the output instead of failing the whole remux. The remux still fails if no
	// supported track remains. Every dropped track is reported via OnDrop.
	SkipUnsupported bool
	// OnDrop, when non-nil, is called once per dropped track (unsupported codecs,
	// and subtitle/other tracks the format cannot carry). It never receives a
	// track that was successfully included.
	OnDrop func(DroppedTrack)
	// FastStart writes the moov box before the mdat box ("fast start"), so a
	// player can begin without first reading to the end of the file — useful for
	// progressive HTTP streaming. It costs one extra pass over the media (written
	// to a temporary file first). Only RemuxToMP4 honours this.
	FastStart bool
}

func optionsFrom(opts []Options) Options {
	if len(opts) > 0 {
		return opts[0]
	}
	return Options{}
}

// report invokes OnDrop if set.
func (o Options) report(d DroppedTrack) {
	if o.OnDrop != nil {
		o.OnDrop(d)
	}
}
