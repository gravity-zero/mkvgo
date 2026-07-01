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
	// Progress, when non-nil, receives processed/total byte counts. Only the
	// remux entry points report progress; OpenMeta/ReadMeta ignore it (the
	// metadata probe is a single bounded read).
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
	// FlattenStyledSubs carries styled text subtitles that have no plain-text-safe
	// default — ASS/SSA — as tx3g timed text instead of dropping them. Flattening
	// is lossy: all styling, positioning and karaoke is discarded, only the text
	// remains. (SRT and WebVTT are already carried as tx3g by default.) Only
	// RemuxToMP4 honours this.
	FlattenStyledSubs bool
	// Keyframes, on the metadata probe (OpenMeta/ReadMeta), builds the MP4 sample
	// table so Container.Keyframes is populated. It is off by default because
	// expanding the sample table is the dominant cost of parsing a long movie's
	// moov; leave it off when only stream metadata is needed. Ignored by the remux
	// entry points, which always build the sample table.
	Keyframes bool
	// NativeWebVTT carries WebVTT tracks as native wvtt (ISO/IEC 14496-30) instead
	// of the default tx3g. wvtt is lossless (cue settings and inline markup are
	// preserved) and is read by Apple/Safari/CMAF, but ffmpeg's MP4 demuxer does
	// not recognise it. Leave it off for the widest playback compatibility. Only
	// RemuxToMP4 honours this.
	NativeWebVTT bool
	// InBandColour, on the metadata probe (OpenMeta/ReadMeta), recovers a video
	// track's colour from the first sample's in-band SPS (and Alternative Transfer
	// Characteristics SEI) when it is absent from both the colr box and a bare
	// hvcC — the MP4 counterpart of reader.WithInBandColourFallback. Off by
	// default; only a track that needs it reads one bounded sample.
	InBandColour bool
	// MP3ContainerDelay carries an MP3 track's encoder delay through the container
	// (the MP4 edit list <-> Matroska CodecDelay), like AAC. Off by default because
	// MP3's delay is already in its in-band Xing/LAME header, which ffmpeg applies on
	// decode; a derived edit list then over-trims and desyncs native MKV/WebM MP3.
	// Opt in only to round-trip an MP3 that originated in an MP4 (rare) back to MP4.
	// Both RemuxFromMP4 and RemuxToMP4 must enable it for a full round trip.
	MP3ContainerDelay bool
}

// wantsEditList reports whether a codec's container delay should be carried as an
// MP4 edit list / Matroska CodecDelay: always for hasContainerPriming codecs, and
// for MP3 only under the MP3ContainerDelay opt-in.
func wantsEditList(codec string, mp3Delay bool) bool {
	return hasContainerPriming(codec) || (mp3Delay && codec == "A_MPEG/L3")
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
