package mp4

// dash.go — the DASH side of the CMAF packaging. The segments RemuxToHLS and
// HLSPlan emit are CMAF fragments (cmfc/cmfs brands), which is the point of
// CMAF: one segment set, two manifests. buildDASHManifest writes the static
// VOD MPD referencing the same init.mp4 + segNNNNN.m4s (SegmentTemplate with
// an explicit SegmentTimeline, millisecond timescale) plus one text/vtt
// AdaptationSet per subtitle rendition (whole-file BaseURL subN.vtt).

import (
	"fmt"
	"strings"
)

// buildDASHManifest renders manifest.mpd for the presentation: one
// AdaptationSet per demuxed rendition (video, each audio with its language,
// each subtitle as a whole-file WebVTT) over the same CMAF segments the HLS
// playlists reference. durs are the segment durations in seconds; bandwidth
// is the peak aggregate segment bitrate in bits/s.
func buildDASHManifest(o *Options, fts []*fragTrack, subs []hlsSubTrack, durs []float64, bandwidth int64) []byte {
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
	fmt.Fprintf(&b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011" mediaPresentationDuration="%s" minBufferTime="%s">`+"\n",
		dashDuration(totalSec), dashDuration(maxDur))
	b.WriteString("  <Period>\n")

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
			as = `mimeType="audio/mp4" contentType="audio"`
			if t.Language != "" {
				as += fmt.Sprintf(" lang=%q", t.Language)
			}
			rep = fmt.Sprintf(`id="a%d" bandwidth="0"`, audioIndex(fts, i))
			if t.SampleRate != nil && *t.SampleRate > 0 {
				rep += fmt.Sprintf(` audioSamplingRate="%d"`, int64(*t.SampleRate))
			}
		}
		if codecs := rfc6381Codec(ft.outTrack); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
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
		as := `mimeType="text/vtt" contentType="text"`
		if t.Language != "" {
			as += fmt.Sprintf(" lang=%q", t.Language)
		}
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
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
	var b strings.Builder
	b.WriteString("          <SegmentTimeline>\n")
	var t int64
	for i := 0; i < len(durs); {
		d := int64(durs[i]*1000 + 0.5)
		j := i + 1
		for j < len(durs) && int64(durs[j]*1000+0.5) == d {
			j++
		}
		if r := j - i - 1; r > 0 {
			fmt.Fprintf(&b, `            <S t="%d" d="%d" r="%d"/>`+"\n", t, d, r)
		} else {
			fmt.Fprintf(&b, `            <S t="%d" d="%d"/>`+"\n", t, d)
		}
		t += d * int64(j-i)
		i = j
	}
	b.WriteString("          </SegmentTimeline>\n")
	return b.String()
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
