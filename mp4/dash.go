package mp4

// dash.go - the DASH side of the CMAF packaging. The segments RemuxToHLS and
// HLSPlan emit are CMAF fragments (cmfc/cmfs brands), which is the point of
// CMAF: one segment set, two manifests. buildDASHManifest writes the static
// VOD MPD referencing the same init.mp4 + segNNNNN.m4s (SegmentTemplate with
// an explicit SegmentTimeline, millisecond timescale) plus one text/vtt
// AdaptationSet per subtitle rendition (whole-file BaseURL subN.vtt).

import (
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildDASHManifest renders manifest.mpd for the presentation: one
// AdaptationSet per demuxed rendition (video, each audio with its language,
// each subtitle as a whole-file WebVTT) over the same CMAF segments the HLS
// playlists reference. durs are the segment durations in seconds; bandwidth
// is the peak aggregate segment bitrate in bits/s. chapters, when non-nil
// (Options.ChapterMarkers), adds a Period-level EventStream - one Event per
// chapter (see buildChapterEventStream); nil leaves the manifest unchanged
// from before the option existed.
func buildDASHManifest(o *Options, fts []*fragTrack, subs []hlsSubTrack, durs []float64, bandwidth int64, chapters []mkv.Chapter) []byte {
	rw := urlRewriter(o)
	var totalSec float64
	for _, d := range durs {
		totalSec += d
	}
	var maxDur float64
	for _, d := range durs {
		if d > maxDur {
			maxDur = d
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	mpdAttrs := `xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011"`
	if o.CENC != nil {
		mpdAttrs = `xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011"`
	}
	fmt.Fprintf(&b, `<MPD %s mediaPresentationDuration="%s" minBufferTime="%s">`+"\n",
		mpdAttrs, dashDuration(totalSec), dashDuration(maxDur))
	b.WriteString("  <Period>\n")
	if len(chapters) > 0 {
		b.WriteString(buildChapterEventStream(chapters))
	}

	// One AdaptationSet per demuxed rendition, all sharing the same timeline.
	timeline := dashTimeline(durs)
	for i, ft := range fts {
		t := &ft.outTrack.mkv
		var as, rep string
		if ft.outTrack.spec.video {
			as = `mimeType="video/mp4" contentType="video"`
			rep = fmt.Sprintf(`id="v" bandwidth="%d"`, bandwidth)
			if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
				rep += fmt.Sprintf(` width="%d" height="%d"`, *t.Width, *t.Height)
			}
			if t.FrameRate != nil && *t.FrameRate > 0 {
				rep += fmt.Sprintf(` frameRate="%s"`, dashFrameRate(*t.FrameRate))
			}
		} else {
			as = `mimeType="audio/mp4" contentType="audio"` + dashLangAttr(t)
			rep = fmt.Sprintf(`id="a%d" bandwidth="%d"`, audioIndex(fts, i), dashAudioBandwidth(ft))
			if t.SampleRate != nil && *t.SampleRate > 0 {
				rep += fmt.Sprintf(` audioSamplingRate="%d"`, int64(*t.SampleRate))
			}
		}
		if codecs := rfc6381Codec(ft.outTrack); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		if o.CENC != nil {
			b.WriteString(cencContentProtection(o.CENC))
		}
		if !ft.outTrack.spec.video {
			b.WriteString(dashLabel(t, "      ")) // after ContentProtection: the schema's order
		}
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
		if !ft.outTrack.spec.video {
			b.WriteString(dashAudioChannelConfiguration(t, "        "))
		}
		media := strings.Replace(renditionSegment(fts, i, 0), "00001", "$Number%05d$", 1)
		fmt.Fprintf(&b, `        <SegmentTemplate initialization="%s" media="%s" startNumber="1" timescale="1000">`+"\n",
			rw(renditionInit(fts, i)), rw(media))
		b.WriteString(timeline)
		b.WriteString("        </SegmentTemplate>\n")
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}

	// Subtitle renditions: one whole-presentation WebVTT file each.
	for i := range subs {
		t := &subs[i].track
		as := `mimeType="text/vtt" contentType="text"` + dashLangAttr(t)
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		b.WriteString(dashLabel(t, "      "))
		fmt.Fprintf(&b, `      <Representation id="sub%d" bandwidth="0">`+"\n", i+1)
		fmt.Fprintf(&b, "        <BaseURL>%s</BaseURL>\n", rw(fmt.Sprintf("sub%d.vtt", i+1)))
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}

	b.WriteString("  </Period>\n</MPD>\n")
	return []byte(b.String())
}

// dashTimeline renders the shared SegmentTimeline (exact millisecond
// durations, run-length encoded).
func dashTimeline(durs []float64) string {
	starts := make([]int64, len(durs))
	ticks := make([]int64, len(durs))
	var t int64
	for i, d := range durs {
		ticks[i] = int64(d*1000 + 0.5)
		starts[i] = t
		t += ticks[i]
	}
	return dashTimelineSpans(starts, ticks)
}

// dashTimelineSpans renders a SegmentTimeline from segment start times and
// durations in the enclosing timescale's ticks. Consecutive segments of equal
// duration that follow each other without a gap collapse into one <S> with a
// repeat count; a gap (a start past the previous end) opens a new <S> with an
// explicit t.
func dashTimelineSpans(starts, durs []int64) string {
	var b strings.Builder
	b.WriteString("          <SegmentTimeline>\n")
	for i := 0; i < len(durs); {
		t, d := starts[i], durs[i]
		j := i + 1
		for j < len(durs) && durs[j] == d && starts[j] == starts[j-1]+d {
			j++
		}
		if r := j - i - 1; r > 0 {
			fmt.Fprintf(&b, `            <S t="%d" d="%d" r="%d"/>`+"\n", t, d, r)
		} else {
			fmt.Fprintf(&b, `            <S t="%d" d="%d"/>`+"\n", t, d)
		}
		i = j
	}
	b.WriteString("          </SegmentTimeline>\n")
	return b.String()
}

// dashLangAttr is the AdaptationSet lang attribute (with its leading space)
// for a track, or "" when the track has no language. The BCP-47 tag wins over
// the legacy three-letter code (Track.ResolvedLanguage), which is what the
// attribute is defined as (RFC 5646).
func dashLangAttr(t *mkv.Track) string {
	if l := t.ResolvedLanguage(); l != "" {
		return fmt.Sprintf(" lang=%q", l)
	}
	return ""
}

// dashAudioBandwidth is an audio Representation's bandwidth attribute: the
// track's own bit rate when it is known - from its samples when the plan
// holds them, else the container's figure (the Matroska BPS tag, the MP4
// btrt/esds average) - and 0 only when neither says. 0 is what every audio
// Representation declared before, known or not.
func dashAudioBandwidth(ft *fragTrack) int64 {
	if n := len(ft.samples); n > 1 {
		if span := ft.samples[n-1].ptsMs - ft.samples[0].ptsMs; span > 0 {
			var bytes int64
			for _, s := range ft.samples {
				bytes += int64(s.size)
			}
			return bytes * 8 * 1000 / span
		}
	}
	if b := ft.outTrack.mkv.Bitrate; b != nil && *b > 0 {
		return int64(*b)
	}
	return 0
}

// dashLabel is the AdaptationSet's Label element (one line, indented) for a
// track with a name - the title the HLS master already shows as NAME - or ""
// when it has none. It goes where the schema puts it: after ContentProtection,
// before the Representations.
func dashLabel(t *mkv.Track, indent string) string {
	if t.Name == "" {
		return ""
	}
	return fmt.Sprintf("%s<Label>%s</Label>\n", indent, xmlEscapeText(t.Name))
}

// hlsChannelsAttr is the EXT-X-MEDIA CHANNELS attribute (with its leading
// comma) for an audio track - the count of its channels, which players and
// the HLS authoring rules expect on every audio rendition - or "" when the
// count is unknown. The DASH side of the same fact is
// dashAudioChannelConfiguration.
func hlsChannelsAttr(t *mkv.Track) string {
	if t.Channels == nil || *t.Channels == 0 {
		return ""
	}
	return fmt.Sprintf(",CHANNELS=\"%d\"", *t.Channels)
}

// dashAudioChannelConfiguration is the Representation's
// AudioChannelConfiguration element (one line, indented) for an audio track,
// or "" when the channel count is unknown. The scheme follows the codec, as
// players expect: AC-3 and E-AC-3 carry the Dolby channel mask, everything
// else the MPEG channel count.
func dashAudioChannelConfiguration(t *mkv.Track, indent string) string {
	if t.Channels == nil || *t.Channels == 0 {
		return ""
	}
	scheme, value := "urn:mpeg:dash:23003:3:audio_channel_configuration:2011", fmt.Sprintf("%d", *t.Channels)
	if t.Codec == "ac3" || t.Codec == "eac3" {
		if mask, ok := dolbyChannelMask(*t.Channels); ok {
			scheme, value = "tag:dolby.com,2014:dash:audio_channel_configuration:2011", mask
		}
	}
	return fmt.Sprintf(`%s<AudioChannelConfiguration schemeIdUri="%s" value="%s"/>`+"\n", indent, scheme, value)
}

// dolbyChannelMask is the Dolby DASH channel-configuration value for the
// conventional layout of a channel count (ETSI TS 102 366 speaker bits, L
// 0x8000, C 0x4000, R 0x2000, Ls 0x1000, Rs 0x0800, Lrs/Rrs 0x0200, LFE
// 0x0001): 2.0, 5.1, 7.1 and the smaller layouts. A count with no single
// conventional layout (7 channels: 6.1 or 7.0) reports !ok and the caller
// falls back to the count.
func dolbyChannelMask(channels uint8) (string, bool) {
	switch channels {
	case 1:
		return "4000", true // C
	case 2:
		return "A000", true // L R
	case 3:
		return "E000", true // L C R
	case 4:
		return "B800", true // L R Ls Rs
	case 5:
		return "F800", true // L C R Ls Rs
	case 6:
		return "F801", true // 5.1
	case 8:
		return "FA01", true // 7.1
	}
	return "", false
}

// dashDuration formats seconds as an ISO 8601 duration (PT#S).
func dashDuration(sec float64) string {
	return fmt.Sprintf("PT%.3fS", sec)
}

// dashFrameRate renders a frame rate as the MPD's rational form, using the
// common NTSC rationals where they match.
func dashFrameRate(fps float64) string {
	for _, r := range []struct {
		num, den int
		fps      float64
	}{
		{24000, 1001, 23.976}, {30000, 1001, 29.97}, {60000, 1001, 59.94},
	} {
		if fps > r.fps-0.005 && fps < r.fps+0.005 {
			return fmt.Sprintf("%d/%d", r.num, r.den)
		}
	}
	if fps == float64(int(fps)) {
		return fmt.Sprintf("%d", int(fps))
	}
	return fmt.Sprintf("%d/1000", int(fps*1000+0.5))
}
