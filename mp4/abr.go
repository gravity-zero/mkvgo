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
// switches just realign on the next keyframe. The combined presentation is
// HLS-only — DASH multi-Representation requires aligned segment timelines,
// which independently encoded sources do not guarantee; each variant
// directory still carries its own manifest.mpd.

import (
	"context"
	"fmt"
	"path/filepath"
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

	return fs.DoWriteFile(filepath.Join(outputDir, "master.m3u8"), buildABRMaster(o, results), 0o644)
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
