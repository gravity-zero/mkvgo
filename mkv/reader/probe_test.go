package reader

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// --- minimal EBML fixture builders (hermetic: no ffmpeg/mkvmerge needed) ------

func uintElem(id uint32, val uint64, n int) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(n))
	ebml.WriteUint(&b, val, n)
	return b.Bytes()
}

func strElem(id uint32, s string) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(len(s)))
	ebml.WriteString(&b, s)
	return b.Bytes()
}

func masterElem(id uint32, children ...[]byte) []byte {
	var inner bytes.Buffer
	for _, c := range children {
		inner.Write(c)
	}
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(inner.Len()))
	b.Write(inner.Bytes())
	return b.Bytes()
}

func trackEntry(elems ...[]byte) []byte { return masterElem(mkv.IDTrackEntry, elems...) }

// buildMKV wraps the given TrackEntry blobs into a complete, minimal seekable
// MKV (EBML header + Segment{Info, Tracks{...}}).
func buildMKV(entries ...[]byte) []byte {
	tracks := masterElem(mkv.IDTracks, entries...)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0) // empty Info
	seg.Write(tracks)

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

func readFirstTrack(t *testing.T, data []byte) mkv.Track {
	t.Helper()
	c, err := Read(context.Background(), bytes.NewReader(data), "probe.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks parsed")
	}
	return c.Tracks[0]
}

// --- (a) no language / default / forced elements ------------------------------

func TestProbeNoLanguageNoFlags(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeSubtitle, 1),
		strElem(mkv.IDCodecID, "S_TEXT/UTF8"),
	)))

	if tr.Language != "" {
		t.Errorf("Language = %q, want \"\" (no longer synthesized to eng)", tr.Language)
	}
	if tr.LanguageBCP47 != "" {
		t.Errorf("LanguageBCP47 = %q, want \"\"", tr.LanguageBCP47)
	}
	if tr.LanguagePresent {
		t.Error("LanguagePresent = true, want false")
	}
	if tr.ResolvedLanguage() != "" {
		t.Errorf("ResolvedLanguage = %q, want \"\"", tr.ResolvedLanguage())
	}
	// FlagDefault absent → spec default true, but DefaultPresent must say "absent".
	if !tr.IsDefault {
		t.Error("IsDefault = false, want true (Matroska spec default when absent)")
	}
	if tr.DefaultPresent {
		t.Error("DefaultPresent = true, want false")
	}
	if tr.IsForced || tr.ForcedPresent {
		t.Errorf("forced: IsForced=%v ForcedPresent=%v, want false/false", tr.IsForced, tr.ForcedPresent)
	}
	if tr.Codec != "srt" {
		t.Errorf("Codec = %q, want srt", tr.Codec)
	}
}

// --- (b) legacy Language only -------------------------------------------------

func TestProbeLegacyLanguageOnly(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AC3"),
		strElem(mkv.IDLanguage, "fre"),
	)))

	if tr.Language != "fre" {
		t.Errorf("Language = %q, want fre", tr.Language)
	}
	if tr.LanguageBCP47 != "" {
		t.Errorf("LanguageBCP47 = %q, want \"\"", tr.LanguageBCP47)
	}
	if !tr.LanguagePresent {
		t.Error("LanguagePresent = false, want true")
	}
	if tr.ResolvedLanguage() != "fre" {
		t.Errorf("ResolvedLanguage = %q, want fre", tr.ResolvedLanguage())
	}
}

// --- (c) LanguageBCP47 only ---------------------------------------------------

func TestProbeBCP47Only(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_OPUS"),
		strElem(mkv.IDLanguageBCP47, "fr"),
	)))

	if tr.Language != "" {
		t.Errorf("Language = %q, want \"\" (only BCP47 present)", tr.Language)
	}
	if tr.LanguageBCP47 != "fr" {
		t.Errorf("LanguageBCP47 = %q, want fr", tr.LanguageBCP47)
	}
	if !tr.LanguagePresent {
		t.Error("LanguagePresent = false, want true")
	}
	if tr.ResolvedLanguage() != "fr" {
		t.Errorf("ResolvedLanguage = %q, want fr", tr.ResolvedLanguage())
	}
}

// --- (c2) both present → BCP47 precedence ------------------------------------

func TestProbeBCP47Precedence(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_OPUS"),
		strElem(mkv.IDLanguage, "fre"),
		strElem(mkv.IDLanguageBCP47, "fr-CA"),
	)))

	if tr.Language != "fre" {
		t.Errorf("Language = %q, want fre", tr.Language)
	}
	if tr.LanguageBCP47 != "fr-CA" {
		t.Errorf("LanguageBCP47 = %q, want fr-CA", tr.LanguageBCP47)
	}
	if tr.ResolvedLanguage() != "fr-CA" {
		t.Errorf("ResolvedLanguage = %q, want fr-CA (BCP47 wins)", tr.ResolvedLanguage())
	}
}

// --- (d) multi-audio FR(default)+EN, explicit FlagDefault --------------------

func TestProbeMultiAudioExplicitDefault(t *testing.T) {
	data := buildMKV(
		trackEntry(
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
			strElem(mkv.IDCodecID, "A_AC3"),
			strElem(mkv.IDLanguage, "fre"),
			uintElem(mkv.IDFlagDefault, 1, 1),
		),
		trackEntry(
			uintElem(mkv.IDTrackNumber, 2, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
			strElem(mkv.IDCodecID, "A_AC3"),
			strElem(mkv.IDLanguage, "eng"),
			uintElem(mkv.IDFlagDefault, 0, 1),
		),
	)
	c, err := Read(context.Background(), bytes.NewReader(data), "multi.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(c.Tracks))
	}
	fr, en := c.Tracks[0], c.Tracks[1]
	if fr.Language != "fre" || !fr.IsDefault || !fr.DefaultPresent {
		t.Errorf("FR track: lang=%q default=%v present=%v, want fre/true/true", fr.Language, fr.IsDefault, fr.DefaultPresent)
	}
	if en.Language != "eng" || en.IsDefault || !en.DefaultPresent {
		t.Errorf("EN track: lang=%q default=%v present=%v, want eng/false/true", en.Language, en.IsDefault, en.DefaultPresent)
	}
}

// --- (e) forced subtitle ------------------------------------------------------

func TestProbeForcedSubtitle(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeSubtitle, 1),
		strElem(mkv.IDCodecID, "S_TEXT/UTF8"),
		strElem(mkv.IDLanguage, "eng"),
		uintElem(mkv.IDFlagForced, 1, 1),
	)))

	if !tr.IsForced || !tr.ForcedPresent {
		t.Errorf("forced: IsForced=%v ForcedPresent=%v, want true/true", tr.IsForced, tr.ForcedPresent)
	}
}

// --- (f) 10-bit HEVC HDR video (BT.2020 / PQ) + frame rate -------------------

// hevcHDRTrackEntry builds a video TrackEntry signalling 10-bit BT.2020/PQ HDR
// at ~23.976 fps (DefaultDuration 41_708_333 ns).
func hevcHDRTrackEntry() []byte {
	colour := masterElem(mkv.IDColour,
		uintElem(mkv.IDColourMatrix, 9, 1),    // BT.2020 non-constant luminance
		uintElem(mkv.IDColourTransfer, 16, 1), // SMPTE ST 2084 (PQ)
		uintElem(mkv.IDColourPrimaries, 9, 1), // BT.2020
		uintElem(mkv.IDColourRange, 1, 1),     // broadcast / limited
		uintElem(mkv.IDColourBitsPerChannel, 10, 1),
	)
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDPixelWidth, 3840, 2),
		uintElem(mkv.IDPixelHeight, 2160, 2),
		colour,
	)
	return trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		uintElem(mkv.IDDefaultDuration, 41_708_333, 4),
		video,
	)
}

func TestProbeVideoColourHDR(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(hevcHDRTrackEntry()))

	if tr.Width == nil || *tr.Width != 3840 || tr.Height == nil || *tr.Height != 2160 {
		t.Errorf("dims = %v x %v, want 3840x2160", tr.Width, tr.Height)
	}
	if tr.VideoBitDepth == nil || *tr.VideoBitDepth != 10 {
		t.Errorf("VideoBitDepth = %v, want 10", tr.VideoBitDepth)
	}
	if tr.ColorSpace == nil || *tr.ColorSpace != 9 || tr.ColorSpaceName() != "bt2020nc" {
		t.Errorf("ColorSpace = %v / %q, want 9 / bt2020nc", tr.ColorSpace, tr.ColorSpaceName())
	}
	if tr.ColorTransfer == nil || *tr.ColorTransfer != 16 || tr.ColorTransferName() != "smpte2084" {
		t.Errorf("ColorTransfer = %v / %q, want 16 / smpte2084", tr.ColorTransfer, tr.ColorTransferName())
	}
	if tr.ColorPrimaries == nil || *tr.ColorPrimaries != 9 || tr.ColorPrimariesName() != "bt2020" {
		t.Errorf("ColorPrimaries = %v / %q, want 9 / bt2020", tr.ColorPrimaries, tr.ColorPrimariesName())
	}
	if tr.ColorRange == nil || tr.ColorRangeName() != "tv" {
		t.Errorf("ColorRange = %v / %q, want 1 / tv", tr.ColorRange, tr.ColorRangeName())
	}
	if !tr.IsHDR() {
		t.Error("IsHDR = false, want true (BT.2020 + PQ)")
	}
	if tr.FrameRate == nil || math.Abs(*tr.FrameRate-23.976) > 0.01 {
		t.Errorf("FrameRate = %v, want ~23.976", tr.FrameRate)
	}
}

// --- stream-reader parity: the same fixture, read via ReadStream -------------

func TestProbeStreamParity(t *testing.T) {
	// A track that exercises BCP47, explicit default and colour, then a tiny
	// cluster so the streaming reader has a terminating element to stop on.
	entry := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		strElem(mkv.IDLanguageBCP47, "fr"),
		uintElem(mkv.IDFlagDefault, 0, 1),
		uintElem(mkv.IDDefaultDuration, 41_708_333, 4),
		masterElem(mkv.IDVideo,
			uintElem(mkv.IDPixelWidth, 1920, 2),
			uintElem(mkv.IDPixelHeight, 1080, 2),
			masterElem(mkv.IDColour,
				uintElem(mkv.IDColourPrimaries, 9, 1),
				uintElem(mkv.IDColourTransfer, 18, 1), // HLG
				uintElem(mkv.IDColourBitsPerChannel, 10, 1),
			),
		),
	)

	seekData := buildMKV(entry)
	seekTrack := readFirstTrack(t, seekData)

	// Read the identical bytes through the streaming parser.
	c, _, err := ReadStream(context.Background(), bytes.NewReader(seekData))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("stream: no tracks")
	}
	st := c.Tracks[0]

	if st.LanguageBCP47 != seekTrack.LanguageBCP47 || st.LanguageBCP47 != "fr" {
		t.Errorf("BCP47 parity: stream=%q seek=%q", st.LanguageBCP47, seekTrack.LanguageBCP47)
	}
	if st.IsDefault != seekTrack.IsDefault || st.DefaultPresent != seekTrack.DefaultPresent {
		t.Errorf("default parity: stream=(%v,%v) seek=(%v,%v)", st.IsDefault, st.DefaultPresent, seekTrack.IsDefault, seekTrack.DefaultPresent)
	}
	if (st.ColorPrimaries == nil) != (seekTrack.ColorPrimaries == nil) || (st.ColorPrimaries != nil && *st.ColorPrimaries != *seekTrack.ColorPrimaries) {
		t.Errorf("primaries parity: stream=%v seek=%v", st.ColorPrimaries, seekTrack.ColorPrimaries)
	}
	if !st.IsHDR() || !seekTrack.IsHDR() {
		t.Errorf("HDR parity: stream=%v seek=%v (HLG)", st.IsHDR(), seekTrack.IsHDR())
	}
	if st.FrameRate == nil || seekTrack.FrameRate == nil || math.Abs(*st.FrameRate-*seekTrack.FrameRate) > 1e-9 {
		t.Errorf("fps parity: stream=%v seek=%v", st.FrameRate, seekTrack.FrameRate)
	}
}
