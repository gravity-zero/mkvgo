// Package mp4 remuxes between Matroska/WebM and the ISO base media file format
// (MP4, ISO/IEC 14496-12).
//
// It is deliberately isolated from mkvgo's EBML core: MP4 is a box format with
// no bytes in common with EBML, so this package shares no low-level code with
// ebml/ or the Matroska reader/writer internals. It composes with them only
// through the public mkv types and the streaming block reader — reading
// Matroska blocks and emitting MP4 boxes (RemuxToMP4), and the reverse
// (RemuxFromMP4).
//
// Remux, not transcode: compressed samples are copied verbatim. Supported
// codecs are H.264, HEVC and AV1 (video) and AAC, Opus, AC-3, E-AC-3, FLAC, MP3
// and DTS (audio); the AC-3/E-AC-3 configuration boxes are derived from the
// elementary bitstream, which Matroska carries no CodecPrivate for, and DTS
// (incl. DTS-HD) is carried as mp4a/esds the way ffmpeg's mov muxer does it.
// TrueHD has no portable MP4 mapping and is rejected.
//
// QuickTime .mov files are read as well, including the raw-camera layout
// (mdat before a trailing moov, wide preamble, brand "qt  ") and QuickTime
// version 1/2 sound descriptions with a wave-wrapped esds.
//
// This package is experimental and its API may change between minor versions.
package mp4
