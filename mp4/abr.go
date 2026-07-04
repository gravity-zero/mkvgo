package mp4

// abr.go — RemuxToABR: package several pre-encoded quality variants of the
// same content (e.g. a 1080p and a 720p file) into one multi-variant HLS
// presentation, without transcoding — "ABR light": mkvgo does the packaging,
// producing the encodes remains a transcoder's job. The first source is the
// reference: its audio tracks and subtitles serve every variant; the other
// sources contribute only their video rendition.
//
// Layout: each source is packaged into v1/, v2/, … (v1 complete, the others
// video-only) and the top-level master.m3u8 declares one variant per source
// with its real BANDWIDTH/RESOLUTION/CODECS, all sharing v1's audio and
// subtitle groups. For seamless quality switching the sources should share
// the keyframe cadence (same GOP length); mismatched cadences still play,
// switches just realign on the next keyframe. The combined HLS master is always
// emitted. A combined DASH manifest.mpd (one AdaptationSet, a Representation per
// variant) is emitted too WHEN the variants are segment-aligned — DASH shares
// one SegmentTimeline across a switch set, so it is only valid then; otherwise
// only each variant's own manifest.mpd is written.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// RemuxToABR packages the sources — quality variants of the same content,
// best first — into outputDir as one multi-variant HLS presentation. See the
// package layout above. Options apply to every variant (same SegmentMs,
// Encrypt, RewriteURL…).
func RemuxToABR(ctx context.Context, sources []string, outputDir string, opts ...Options) error {
	if len(sources) < 2 {
		return errf("ABR packaging needs at least two sources (got %d) — use RemuxToHLS for one", len(sources))
	}
	o := optionsFrom(opts)
	fs := o.FS
	if err := fs.DoMkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	results := make([]*hlsResult, len(sources))
	for i, src := range sources {
		vo := o
		vo.VideoOnly = i > 0
		sub := filepath.Join(outputDir, fmt.Sprintf("v%d", i+1))
		res, err := remuxToHLSInto(ctx, src, sub, &vo)
		if err != nil {
			return errf("variant %d (%s): %w", i+1, src, err)
		}
		results[i] = res
	}

	if err := fs.DoWriteFile(filepath.Join(outputDir, "master.m3u8"), buildABRMaster(o, results), 0o644); err != nil {
		return err
	}
	// Combined DASH only when the variants line up (see combinedDASH).
	if mpd := combinedDASH(o, results); mpd != nil {
		if err := fs.DoWriteFile(filepath.Join(outputDir, "manifest.mpd"), mpd, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// buildABRMaster assembles the multi-variant master.m3u8 from the packaging
// facts of each quality variant (best first). The reference variant (v1)
// supplies the shared audio and subtitle groups; every variant contributes one
// EXT-X-STREAM-INF with its real BANDWIDTH/RESOLUTION/CODECS pointing at
// v{i}/playlist.m3u8. Identical for the full pass (RemuxToABR) and the
// on-demand plan (PlanABR), so their masters are byte-for-byte the same.
func buildABRMaster(o Options, results []*hlsResult) []byte {
	rw := urlRewriter(&o)
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n"...)

	// Audio and subtitle groups from the reference source, under v1/.
	ref := results[0]
	nAudio := 0
	hasDefaultAudio := false
	for _, ft := range ref.fts {
		hasDefaultAudio = hasDefaultAudio || (!ft.outTrack.spec.video && ft.outTrack.mkv.IsDefault)
	}
	for i, ft := range ref.fts {
		if ft.outTrack.spec.video {
			continue
		}
		nAudio++
		t := &ft.outTrack.mkv
		name := t.Name
		if name == "" && t.Language != "" {
			name = t.Language
		}
		if name == "" {
			name = fmt.Sprintf("Audio %d", nAudio)
		}
		attrs := fmt.Sprintf("TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%q,AUTOSELECT=YES", name)
		if t.Language != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%q", t.Language)
		}
		if t.IsDefault || (!hasDefaultAudio && nAudio == 1) {
			attrs += ",DEFAULT=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw("v1/"+renditionPlaylist(ref.fts, i)))...)
	}
	for i := range ref.subs {
		t := &ref.subs[i].track
		name := t.Name
		if name == "" && t.Language != "" {
			name = t.Language
		}
		if name == "" {
			name = fmt.Sprintf("Subtitles %d", i+1)
		}
		attrs := fmt.Sprintf("TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=%q,AUTOSELECT=YES", name)
		if t.Language != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%q", t.Language)
		}
		if t.IsDefault {
			attrs += ",DEFAULT=YES"
		}
		if t.IsForced {
			attrs += ",FORCED=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw(fmt.Sprintf("v1/sub%d.m3u8", i+1)))...)
	}

	// The audio codec strings ride on every variant (the group is shared).
	var audioCodecs string
	for _, ft := range ref.fts {
		if ft.outTrack.spec.video {
			continue
		}
		if cs := rfc6381Codec(ft.outTrack); cs != "" {
			audioCodecs += "," + cs
		} else {
			audioCodecs = ""
			break
		}
	}

	for vi, res := range results {
		inf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d", peakBandwidth(res.segs))
		if v := pickVideoFrag(res.fts); v != nil {
			t := &v.outTrack.mkv
			if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
				inf += fmt.Sprintf(",RESOLUTION=%dx%d", *t.Width, *t.Height)
			}
			if t.FrameRate != nil && *t.FrameRate > 0 {
				inf += fmt.Sprintf(",FRAME-RATE=%.3f", *t.FrameRate)
			}
			if vcs := rfc6381Codec(v.outTrack); vcs != "" && (audioCodecs != "" || nAudio == 0) {
				inf += fmt.Sprintf(",CODECS=%q", vcs+audioCodecs)
			}
		}
		if nAudio > 0 {
			inf += ",AUDIO=\"aud\""
		}
		if len(ref.subs) > 0 {
			inf += ",SUBTITLES=\"subs\""
		}
		b = append(b, (inf + "\n" + rw(fmt.Sprintf("v%d/playlist.m3u8", vi+1)) + "\n")...)
	}

	// Trick-play: one I-frame stream per variant that exposes an I-frame
	// playlist (progressive/CMAF MP4 sources, unencrypted). Players use these
	// for fast scrubbing; a variant without one (Matroska plan, encryption) is
	// simply omitted.
	for vi, res := range results {
		if len(res.iframes) == 0 {
			continue
		}
		uri := fmt.Sprintf("v%d/iframe.m3u8", vi+1)
		b = append(b, iframeStreamInfURI(&o, res.fts, res.durs, res.iframes, uri)...)
	}

	return b
}

// abrVariantsAligned reports whether every variant shares the same segment
// timeline — identical segment count and per-segment millisecond durations.
// Only then is a combined DASH multi-Representation manifest valid: DASH puts
// every Representation of an AdaptationSet on ONE shared SegmentTimeline, so a
// player switching quality fetches segment N of the new variant expecting it at
// the timeline's time N; misaligned variants would hand it the wrong-timed
// segment. HLS carries a separate playlist per variant and has no such
// constraint, so the combined HLS master is always emitted; the combined MPD
// only when the encodes line up (fixed GOP, keyframes at the same times).
func abrVariantsAligned(results []*hlsResult) bool {
	if len(results) < 2 || len(results[0].durs) == 0 {
		return false
	}
	ref := results[0].durs
	msDur := func(d float64) int64 { return int64(d*1000 + 0.5) }
	for _, r := range results[1:] {
		if len(r.durs) != len(ref) {
			return false
		}
		for i := range ref {
			if msDur(r.durs[i]) != msDur(ref[i]) {
				return false
			}
		}
	}
	return true
}

// combinedDASH builds one DASH manifest for segment-aligned ABR variants: a
// single video AdaptationSet with a Representation per variant (adaptive
// switching in one manifest), plus the reference variant's audio and subtitles,
// all on the shared SegmentTimeline. Returns nil for an encrypted presentation
// (DASH is not emitted) or when the variants are not aligned. Byte-identical for
// the full pass (RemuxToABR) and the on-demand plan (PlanABR).
func combinedDASH(o Options, results []*hlsResult) []byte {
	if o.Encrypt != nil || !abrVariantsAligned(results) {
		return nil
	}
	rw := urlRewriter(&o)
	durs := results[0].durs
	var totalSec, maxDur float64
	for _, d := range durs {
		totalSec += d
		if d > maxDur {
			maxDur = d
		}
	}
	timeline := dashTimeline(durs)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-live:2011" mediaPresentationDuration="%s" minBufferTime="%s">`+"\n",
		dashDuration(totalSec), dashDuration(maxDur))
	b.WriteString("  <Period>\n")

	// Video: one AdaptationSet, one Representation per variant (the switch set).
	b.WriteString(`    <AdaptationSet mimeType="video/mp4" contentType="video">` + "\n")
	for vi, res := range results {
		v := pickVideoFrag(res.fts)
		if v == nil {
			continue
		}
		vidx := 0
		for i, ft := range res.fts {
			if ft == v {
				vidx = i
				break
			}
		}
		t := &v.outTrack.mkv
		rep := fmt.Sprintf(`id="v%d" bandwidth="%d"`, vi+1, peakBandwidth(res.segs))
		if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
			rep += fmt.Sprintf(` width="%d" height="%d"`, *t.Width, *t.Height)
		}
		if t.FrameRate != nil && *t.FrameRate > 0 {
			rep += fmt.Sprintf(` frameRate="%s"`, dashFrameRate(*t.FrameRate))
		}
		if codecs := rfc6381Codec(v.outTrack); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		prefix := fmt.Sprintf("v%d/", vi+1)
		media := prefix + strings.Replace(renditionSegment(res.fts, vidx, 0), "00001", "$Number%05d$", 1)
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
		fmt.Fprintf(&b, `        <SegmentTemplate initialization="%s" media="%s" startNumber="1" timescale="1000">`+"\n",
			rw(prefix+renditionInit(res.fts, vidx)), rw(media))
		b.WriteString(timeline)
		b.WriteString("        </SegmentTemplate>\n")
		b.WriteString("      </Representation>\n")
	}
	b.WriteString("    </AdaptationSet>\n")

	// Audio and subtitles ride on the reference variant (v1/), shared by all.
	ref := results[0]
	for i, ft := range ref.fts {
		if ft.outTrack.spec.video {
			continue
		}
		t := &ft.outTrack.mkv
		as := `mimeType="audio/mp4" contentType="audio"`
		if t.Language != "" {
			as += fmt.Sprintf(" lang=%q", t.Language)
		}
		rep := fmt.Sprintf(`id="a%d" bandwidth="0"`, audioIndex(ref.fts, i))
		if t.SampleRate != nil && *t.SampleRate > 0 {
			rep += fmt.Sprintf(` audioSamplingRate="%d"`, int64(*t.SampleRate))
		}
		if codecs := rfc6381Codec(ft.outTrack); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		media := "v1/" + strings.Replace(renditionSegment(ref.fts, i, 0), "00001", "$Number%05d$", 1)
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
		fmt.Fprintf(&b, `        <SegmentTemplate initialization="%s" media="%s" startNumber="1" timescale="1000">`+"\n",
			rw("v1/"+renditionInit(ref.fts, i)), rw(media))
		b.WriteString(timeline)
		b.WriteString("        </SegmentTemplate>\n")
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}
	for i := range ref.subs {
		t := &ref.subs[i].track
		as := `mimeType="text/vtt" contentType="text"`
		if t.Language != "" {
			as += fmt.Sprintf(" lang=%q", t.Language)
		}
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		fmt.Fprintf(&b, `      <Representation id="sub%d" bandwidth="0">`+"\n", i+1)
		fmt.Fprintf(&b, "        <BaseURL>%s</BaseURL>\n", rw(fmt.Sprintf("v1/sub%d.vtt", i+1)))
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}

	b.WriteString("  </Period>\n</MPD>\n")
	return []byte(b.String())
}
