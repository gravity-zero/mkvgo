package ops

// playability.go - a pure metadata decision layer that lets a media server
// decide, WITHOUT probing an external tool and WITHOUT decoding, whether a
// file can direct-play on a target, needs only a container remux, or needs a
// transcode - and if a remux is needed, which container is the cheapest one
// that would work. Every decision is made from the head-only track metadata
// mkvgo already reads (codec, profile, level, pixel format, bit depth,
// resolution, colour/HDR/Dolby Vision, audio channels/sample rate): no block
// walk, no sample decode.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

// Target describes what a playback target can consume directly: which
// container(s), which video/audio codecs, resolution and level ceilings, and
// whether it accepts HDR10/Dolby Vision and 10-bit video. See TargetByName
// for the built-in, reviewable capability table this package ships with.
//
// Container/VideoCodecs/AudioCodecs entries are mkvgo's own short names (the
// same strings as Track.Codec: "h264", "hevc", "av1", "vp9", "vp8", "aac",
// "opus", "mp3", "ac3", "eac3", "flac", ...); Container is listed cheapest
// first, since it doubles as the remux preference order.
//
// MaxLevelH264/MaxLevelHEVC use the same encoding as Track.Level: H.264
// level_idc (10x the level, e.g. 41 = 4.1), HEVC general_level_idc (30x the
// level, e.g. 120 = 4.0). 0 means "no limit stated".
type Target struct {
	Name        string   // "safari", "chrome", "firefox", "chromecast-gen3", "mse-generic", or "custom"
	Container   []string // playable container formats, cheapest first: "mp4", "webm", "hls", "dash", "mkv" (rare)
	VideoCodecs []string
	AudioCodecs []string
	MaxWidth    int
	MaxHeight   int
	// MaxLevelH264/MaxLevelHEVC cap the codec level (0 = no limit).
	MaxLevelH264 int
	MaxLevelHEVC int
	// HDR/DolbyVision gate HDR10 static metadata / Dolby Vision configuration
	// records regardless of which video codec carries them.
	HDR         bool
	DolbyVision bool
	// HEVCMain10 gates 10-bit HEVC (profile "Main 10"); VP9Profile2 gates
	// 10-bit VP9. Both are read from the track's decoded bit depth, since
	// mkvgo does not resolve a VP9 profile number from the bitstream.
	HEVCMain10  bool
	VP9Profile2 bool
}

// TrackVerdict is one track's playability verdict.
type TrackVerdict struct {
	TrackID uint64
	Type    string   // "video", "audio", "subtitle"
	Verdict string   // "direct-play" | "remux" | "transcode"
	Reasons []string // why remux or transcode; empty for direct-play
}

// PlayabilityReport is the whole-file verdict: per track, plus the overall
// (worst of all tracks) and, when the overall is "remux", the cheapest
// container that would make every track direct-play.
type PlayabilityReport struct {
	Target         string
	OverallVerdict string
	RemuxContainer string
	Tracks         []TrackVerdict
}

const (
	verdictDirectPlay = "direct-play"
	verdictRemux      = "remux"
	verdictTranscode  = "transcode"
)

// verdictRank orders verdicts from best to worst so the overall verdict can be
// computed as a simple max.
func verdictRank(v string) int {
	switch v {
	case verdictRemux:
		return 1
	case verdictTranscode:
		return 2
	default:
		return 0
	}
}

// Playability evaluates every track of the file at path against target and
// returns the per-track and overall verdicts. It reads head-only metadata
// only (Matroska/WebM via reader.OpenMetaWithFS, MP4/MOV via mp4.OpenMeta) -
// no block walk, no decode.
func Playability(ctx context.Context, path string, target Target, opts ...mkv.Options) (*PlayabilityReport, error) {
	c, srcContainer, err := openAnyMeta(ctx, path, mkv.FSFrom(opts))
	if err != nil {
		return nil, err
	}
	return evaluatePlayability(c.Tracks, srcContainer, target), nil
}

// evaluatePlayability is the pure decision core of Playability, factored out
// so it can be exercised directly (in tests, or by a caller that already has
// the tracks and just wants the verdicts) without file I/O.
func evaluatePlayability(tracks []mkv.Track, srcContainer string, target Target) *PlayabilityReport {
	report := &PlayabilityReport{Target: target.Name}
	worst := 0
	for _, t := range tracks {
		v := evaluateTrack(t, srcContainer, target)
		if r := verdictRank(v.Verdict); r > worst {
			worst = r
		}
		report.Tracks = append(report.Tracks, v)
	}
	switch worst {
	case 2:
		report.OverallVerdict = verdictTranscode
	case 1:
		report.OverallVerdict = verdictRemux
		report.RemuxContainer = pickRemuxContainer(target, tracks, report.Tracks)
	default:
		report.OverallVerdict = verdictDirectPlay
	}
	return report
}

// evaluateTrack returns one track's verdict: video/audio codec and profile
// support first (a hard "transcode" failure short-circuits), then - only for
// a track whose codec is otherwise fine - whether the source container is
// itself accepted by the target (direct-play) or merely carries a codec the
// target could accept from a different container (remux).
func evaluateTrack(t mkv.Track, srcContainer string, target Target) TrackVerdict {
	v := TrackVerdict{TrackID: t.ID, Type: string(t.Type)}

	switch t.Type {
	case mkv.VideoTrack:
		evaluateVideoTrack(&v, t, target)
	case mkv.AudioTrack:
		evaluateAudioTrack(&v, t, target)
	default:
		// Subtitles are container/codec-agnostic to this decision: mkvgo (and
		// most players) side-load or burn them independently of the video/
		// audio remux path, so they never force a transcode.
		v.Verdict = verdictDirectPlay
		return v
	}
	if v.Verdict == verdictTranscode {
		return v
	}

	codec := canonicalCodec(t.Codec)
	if containsStr(target.Container, srcContainer) {
		v.Verdict = verdictDirectPlay
		return v
	}
	if remux := firstContainerSupporting(target.Container, codec); remux != "" {
		v.Verdict = verdictRemux
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"source container %q does not carry this codec on target %q; remux to %s carries it without a transcode",
			srcContainer, target.Name, remux))
		return v
	}
	v.Verdict = verdictTranscode
	v.Reasons = append(v.Reasons, fmt.Sprintf(
		"codec %q is not carried by any container target %q accepts; a transcode is required", codec, target.Name))
	return v
}

// evaluateVideoTrack sets v.Verdict to "transcode" (with reasons) when the
// codec, profile, level, resolution, bit depth or HDR/Dolby Vision signalling
// is not supported by target; otherwise it leaves v.Verdict at
// "direct-play" (the caller still checks the container).
func evaluateVideoTrack(v *TrackVerdict, t mkv.Track, target Target) {
	codec := canonicalCodec(t.Codec)
	if !containsStr(target.VideoCodecs, codec) {
		v.Verdict = verdictTranscode
		v.Reasons = append(v.Reasons, fmt.Sprintf("video codec %q is not supported on target %q", codec, target.Name))
		return
	}

	var reasons []string
	switch codec {
	case "h264":
		if target.MaxLevelH264 > 0 {
			if t.Level == nil {
				reasons = append(reasons, "H.264 level is not present in the source metadata; treating conservatively as exceeding the target's level limit")
			} else if int(*t.Level) > target.MaxLevelH264 {
				reasons = append(reasons, fmt.Sprintf("level %s exceeds target max %s",
					h264LevelString(*t.Level), h264LevelString(uint16(target.MaxLevelH264))))
			}
		}
	case "hevc":
		if target.MaxLevelHEVC > 0 {
			if t.Level == nil {
				reasons = append(reasons, "HEVC level is not present in the source metadata; treating conservatively as exceeding the target's level limit")
			} else if int(*t.Level) > target.MaxLevelHEVC {
				reasons = append(reasons, fmt.Sprintf("level %s exceeds target max %s",
					hevcLevelString(*t.Level), hevcLevelString(uint16(target.MaxLevelHEVC))))
			}
		}
		if isHEVCMain10(t) && !target.HEVCMain10 {
			reasons = append(reasons, "HEVC Main10 (10-bit) is unsupported on target")
		}
	case "vp9":
		if isTrack10Bit(t) && !target.VP9Profile2 {
			reasons = append(reasons, "VP9 Profile 2 (10-bit) is unsupported on target")
		}
	}
	if t.DolbyVision != nil && !target.DolbyVision {
		reasons = append(reasons, "Dolby Vision is unsupported on target")
	}
	if t.HDR != nil && !target.HDR {
		reasons = append(reasons, "HDR10 static metadata is unsupported on target")
	}
	if target.MaxWidth > 0 && t.Width != nil && int(*t.Width) > target.MaxWidth {
		reasons = append(reasons, fmt.Sprintf("width %d exceeds target max %d", *t.Width, target.MaxWidth))
	}
	if target.MaxHeight > 0 && t.Height != nil && int(*t.Height) > target.MaxHeight {
		reasons = append(reasons, fmt.Sprintf("height %d exceeds target max %d", *t.Height, target.MaxHeight))
	}

	if len(reasons) > 0 {
		v.Verdict = verdictTranscode
		v.Reasons = reasons
		return
	}
	v.Verdict = verdictDirectPlay
}

// evaluateAudioTrack sets v.Verdict to "transcode" when the codec is not in
// target.AudioCodecs, else leaves it at "direct-play" (container checked by
// the caller). Audio has no profile/level ceiling in this model.
func evaluateAudioTrack(v *TrackVerdict, t mkv.Track, target Target) {
	codec := canonicalCodec(t.Codec)
	if !containsStr(target.AudioCodecs, codec) {
		v.Verdict = verdictTranscode
		v.Reasons = append(v.Reasons, fmt.Sprintf("audio codec %q is not supported on target %q", codec, target.Name))
		return
	}
	v.Verdict = verdictDirectPlay
}

// isHEVCMain10 reports whether the track is 10-bit HEVC: either the SPS
// profile string ("Main 10") or, when the profile could not be resolved, the
// decoded video bit depth.
func isHEVCMain10(t mkv.Track) bool {
	return t.Profile == "Main 10" || isTrack10Bit(t)
}

// isTrack10Bit reports whether the track's decoded bit depth is above 8,
// mkvgo's only head-only signal for a 10-bit stream when the codec has no
// explicit profile string (VP9).
func isTrack10Bit(t mkv.Track) bool {
	return t.VideoBitDepth != nil && *t.VideoBitDepth > 8
}

// h264LevelString formats an H.264 level_idc (10x the level) the way ffprobe
// prints it, e.g. 41 -> "4.1".
func h264LevelString(levelIdc uint16) string {
	return fmt.Sprintf("%.1f", float64(levelIdc)/10)
}

// hevcLevelString formats an HEVC general_level_idc (30x the level), e.g.
// 120 -> "4.0".
func hevcLevelString(levelIdc uint16) string {
	return fmt.Sprintf("%.1f", float64(levelIdc)/30)
}

// canonicalCodec normalizes the one mkvgo short codec name that is not
// already the canonical form used across containers: MP3 stays the raw
// Matroska codec ID "A_MPEG/L3" in Track.Codec (see mkv/codec_names.go).
// Every other codec is already canonical (h264, hevc, av1, vp9, vp8, aac,
// opus, ac3, eac3, flac, vorbis, dts, truehd, ...).
func canonicalCodec(codec string) string {
	if codec == "A_MPEG/L3" {
		return "mp3"
	}
	return codec
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// containerCodecs is the static "does this container format carry this
// codec" table used to find the cheapest remux target. It intentionally
// mirrors mkvgo's own remux capabilities (to-mp4/to-webm/to-hls), not a
// container format's full theoretical codec list.
var containerCodecs = map[string]map[string]bool{
	"mp4": {
		"h264": true, "hevc": true, "av1": true, "vp9": true,
		"aac": true, "ac3": true, "eac3": true, "mp3": true, "flac": true, "opus": true,
	},
	"webm": {
		"vp8": true, "vp9": true, "av1": true,
		"opus": true, "vorbis": true, "flac": true,
	},
	"hls": {
		"h264": true, "hevc": true,
		"aac": true, "ac3": true, "eac3": true, "mp3": true,
	},
	"dash": {
		"h264": true, "hevc": true, "av1": true, "vp9": true,
		"aac": true, "ac3": true, "eac3": true, "opus": true,
	},
	"mkv": {
		"h264": true, "hevc": true, "av1": true, "vp9": true, "vp8": true,
		"aac": true, "ac3": true, "eac3": true, "mp3": true, "flac": true,
		"opus": true, "vorbis": true, "dts": true, "truehd": true,
	},
}

func containerSupportsCodec(container, codec string) bool {
	m, ok := containerCodecs[container]
	return ok && m[codec]
}

func firstContainerSupporting(containers []string, codec string) string {
	for _, c := range containers {
		if containerSupportsCodec(c, codec) {
			return c
		}
	}
	return ""
}

// pickRemuxContainer returns the cheapest container in target.Container
// (preference order as given) that carries every track's codec that will
// keep its current codec - i.e. every track whose own verdict was not
// already "transcode" (a track being transcoded ends up in whatever codec
// the transcode targets, so its current codec does not constrain the
// container choice). "" when no single container works for every such track.
func pickRemuxContainer(target Target, tracks []mkv.Track, verdicts []TrackVerdict) string {
	for _, cont := range target.Container {
		ok := true
		for i, t := range tracks {
			if t.Type == mkv.SubtitleTrack || verdicts[i].Verdict == verdictTranscode {
				continue
			}
			if !containerSupportsCodec(cont, canonicalCodec(t.Codec)) {
				ok = false
				break
			}
		}
		if ok {
			return cont
		}
	}
	return ""
}

// isMP4Ext reports whether path looks like an ISO-BMFF file by extension
// (query/fragment suffixes of a URL are ignored). Mirrors the CLI's
// extension dispatch (cmd/mkvgo/commands.isMP4Path).
func isMP4Ext(path string) bool {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".m4a", ".mov":
		return true
	}
	return false
}

// openAnyMeta reads head-only metadata from either a Matroska/WebM or an
// MP4/MOV source, dispatching by extension first and falling back to content
// sniffing (reader.ErrNotMatroska) for a mislabeled file. It returns the
// container's format name ("mkv" or "mp4") alongside the metadata, since
// Playability needs it to know whether the source container already plays on
// the target.
func openAnyMeta(ctx context.Context, path string, fs *mkv.FS) (*mkv.Container, string, error) {
	if isMP4Ext(path) {
		c, _, err := mp4.OpenMetaWithFS(ctx, path, fs)
		if err != nil {
			return nil, "", err
		}
		return c, "mp4", nil
	}
	c, err := reader.OpenMetaWithFS(ctx, path, fs)
	if err != nil {
		if errors.Is(err, reader.ErrNotMatroska) {
			c2, _, mp4Err := mp4.OpenMetaWithFS(ctx, path, fs)
			if mp4Err != nil {
				return nil, "", mp4Err
			}
			return c2, "mp4", nil
		}
		return nil, "", err
	}
	return c, "mkv", nil
}
