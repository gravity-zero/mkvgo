package mp4

import "github.com/gravity-zero/mkvgo/mkv"

// frameconvert.go - the optional per-frame audio codec seam.
//
// A FrameConverter re-encodes an audio track's elementary-stream frames from
// one codec to another while a presentation is packaged, so a source codec a
// browser cannot play (AC-3, E-AC-3) is served as one it can (FLAC) with no
// separate transcode pass and no change to the source file. It is entirely
// optional: with Options.FrameConverter nil - the default - every frame is
// carried verbatim and the packaged output is byte-for-byte the unconverted
// presentation. The seam adds no dependency to this module; the decoder and
// re-encoder live in the caller's implementation, injected as an interface.

// FrameConverter decides, per audio track, whether that track's frames are
// re-encoded, and supplies the per-track converter that does it.
type FrameConverter interface {
	// NewTrackConverter returns a converter for one audio track, or (nil, nil)
	// to carry that track verbatim. It is called once per audio track per
	// packaging pass, so a converter's per-track state - a decoder's
	// inter-frame overlap, a re-encoder's running sample number - is never
	// shared across tracks and never reused across passes (each on-demand
	// window and each full pass gets its own).
	//
	// The track is the source track (its Channels, SampleRate and ID carry
	// over to the converted output unchanged); only video tracks are never
	// offered - the seam is audio only.
	NewTrackConverter(track mkv.Track) (TrackConverter, error)
}

// TrackConverter converts the frames of one audio track, fed in decode order.
type TrackConverter interface {
	// Convert transforms one input frame into one output frame. The mapping is
	// one-to-one: one input syncframe yields one output frame carrying the same
	// samples, so per-sample timing and segment boundaries are unchanged and
	// only the bytes and their size differ. A converter that cannot keep one
	// frame in for one frame out (a different samples-per-frame than the
	// source) must not be used here - the media timescale and segment grid are
	// derived from the source's frame rate and would drift.
	Convert(frame []byte) ([]byte, error)

	// OutputCodec is the converted track's Matroska codec id and CodecPrivate,
	// as mp4 names them: for FLAC, codec "flac" and the CodecPrivate a native
	// FLAC track would carry (the "fLaC" marker followed by the metadata
	// blocks whose STREAMINFO the fMP4 dfLa box is built from). mkvgo builds
	// the stsd sample entry from these exactly as for a native track of that
	// codec. It is read once, before the first frame, so the header must be
	// known at construction (it always is: a FLAC STREAMINFO is fixed by the
	// channel count, sample rate and block size, not by the audio).
	OutputCodec() (codec string, codecPrivate []byte)
}

// applyFrameConverter rebinds an audio outTrack to its converter's output
// codec, so every downstream box-builder (the stsd sample entry, the handler,
// the codec dispatch) treats it as a native track of the converted codec and
// only the per-frame bytes still pass through Convert. It is a no-op - and the
// track is left exactly as it was - when there is no converter, when the
// converter declines the track, or when the track is not audio, which is what
// keeps the nil path byte-for-byte identical.
//
// The returned TrackConverter is nil when the track is carried verbatim; the
// caller stores it on the outTrack and runs each frame through it at the
// choke point.
func applyFrameConverter(fc FrameConverter, ot *outTrack) (TrackConverter, error) {
	if fc == nil || ot.spec.video || ot.spec.text {
		return nil, nil
	}
	tc, err := fc.NewTrackConverter(ot.mkv)
	if err != nil {
		return nil, err
	}
	if tc == nil {
		return nil, nil
	}
	codec, codecPrivate := tc.OutputCodec()
	spec, ok := lookupCodec(codec)
	if !ok {
		return nil, errf("track %d: frame converter output codec %q is not packageable", ot.mkv.ID, codec)
	}
	ot.mkv.Codec = codec
	ot.mkv.CodecPrivate = codecPrivate
	ot.spec = spec
	ot.sampleEntry = nil // rebuilt from the converted codec on the first frame
	return tc, nil
}

// convertFrame runs one frame through the track's converter, or returns it
// unchanged when the track is carried verbatim (conv nil). It is the single
// call every choke point makes, so the verbatim path stays a plain passthrough.
func (ot *outTrack) convertFrame(frame []byte) ([]byte, error) {
	if ot.conv == nil {
		return frame, nil
	}
	return ot.conv.Convert(frame)
}
