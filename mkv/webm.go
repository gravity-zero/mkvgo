package mkv

import (
	"fmt"
	"strings"
)

// WebM is a constrained profile of Matroska: it declares the "webm" DocType and
// permits only a small set of codecs. webmCodecs is that allowlist (the WebVTT
// subtitle family is matched separately, by prefix, in IsWebMCodec).
var webmCodecs = map[string]bool{
	"V_VP8":         true, // video
	"V_VP9":         true,
	"V_AV1":         true,
	"A_VORBIS":      true, // audio
	"A_OPUS":        true,
	"S_TEXT/WEBVTT": true, // subtitles (Matroska-style WebVTT id)
}

// canonicalCodecID maps a codec string to its full Matroska codec ID. mkvgo's
// reader stores the short name (e.g. "vp9") in Track.Codec, while the WebM
// allowlist is keyed by full IDs (e.g. "V_VP9"); this accepts either form.
func canonicalCodecID(codec string) string {
	if webmCodecs[codec] {
		return codec // already a full ID
	}
	for full, short := range CodecShortName {
		if short == codec {
			return full
		}
	}
	return codec
}

// IsWebMCodec reports whether a codec is permitted in a WebM file. It accepts
// either the full Matroska codec ID ("V_VP9") or mkvgo's short name ("vp9").
func IsWebMCodec(codec string) bool {
	id := canonicalCodecID(codec)
	if webmCodecs[id] {
		return true
	}
	// WebM's own subtitle/caption ids: D_WEBVTT/SUBTITLES, /CAPTIONS, /DESCRIPTIONS, /METADATA.
	return strings.HasPrefix(id, "D_WEBVTT/")
}

// WebMDocTypeVersion returns the EBML DocTypeVersion needed to write c as WebM:
// 4 when an AV1 track is present (AV1-in-WebM is a DocTypeVersion-4 feature),
// otherwise 2 (the classic WebM baseline).
func WebMDocTypeVersion(c *Container) uint64 {
	for _, t := range c.Tracks {
		if canonicalCodecID(t.Codec) == "V_AV1" {
			return 4
		}
	}
	return 2
}

// ValidateWebM checks that c can be written as WebM: every track must use a
// codec from the WebM allowlist AND carry the codec initialisation data that
// strict players (browsers) require. mkvgo is a container tool - it cannot
// transcode or synthesise init data - so either is a hard error. It returns an
// error naming each offending track, or nil when c is WebM-compatible.
func ValidateWebM(c *Container) error {
	var bad []string
	for _, t := range c.Tracks {
		if !IsWebMCodec(t.Codec) {
			bad = append(bad, fmt.Sprintf("track %d (%s %q): codec outside the WebM subset", t.ID, t.Type, t.Codec))
			continue
		}
		if msg := webmCodecInitError(t); msg != "" {
			bad = append(bad, fmt.Sprintf("track %d (%s): %s", t.ID, t.Codec, msg))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("not WebM-compatible: %s (WebM allows only VP8/VP9/AV1 video, Vorbis/Opus audio, WebVTT subtitles)", strings.Join(bad, "; "))
	}
	return nil
}

// WebMNonSubsetElements lists the top-level metadata elements present in c that
// the WebM streaming output does NOT carry - RemuxToWebM / NewWebMStreamWriter
// emit only Info + Tracks + Clusters (+ Cues when seekable). Attachments are not
// part of WebM at all; Chapters and Tags are dropped because the streaming
// writer does not serialise them. An empty result means nothing will be lost.
// Callers can use this to detect data loss before remuxing.
func WebMNonSubsetElements(c *Container) []string {
	var out []string
	if len(c.Chapters) > 0 {
		out = append(out, "Chapters")
	}
	if len(c.Attachments) > 0 {
		out = append(out, "Attachments")
	}
	if len(c.Tags) > 0 {
		out = append(out, "Tags")
	}
	return out
}

// webmCodecInitError reports, for codecs whose CodecPrivate is mandatory in
// WebM, why a track's setup data is missing or malformed - or "" if acceptable.
// Browsers reject tracks lacking this initialisation data even when the codec
// itself is allowed.
func webmCodecInitError(t Track) string {
	switch canonicalCodecID(t.Codec) {
	case "A_OPUS":
		if len(t.CodecPrivate) < 8 || string(t.CodecPrivate[:8]) != "OpusHead" {
			return "Opus track is missing its OpusHead CodecPrivate"
		}
	case "A_VORBIS":
		if len(t.CodecPrivate) == 0 {
			return "Vorbis track is missing its setup-header CodecPrivate"
		}
		if t.CodecPrivate[0] != 0x02 {
			return "Vorbis CodecPrivate is malformed (expected 3 packed setup headers)"
		}
	case "V_AV1":
		if len(t.CodecPrivate) == 0 {
			return "AV1 track is missing its av1C CodecPrivate"
		}
	}
	return ""
}
