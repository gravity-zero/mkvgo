package mp4

// cmafhls.go - the HLS side of CMAFPresentation: the same external fragments
// DASHFromCMAF describes, as a multivariant master plus one media playlist per
// representation. Nothing is copied or re-packaged; the playlists reference
// the files where they are, through the same URLPrefix + Options.RewriteURL.
// HLS carries one playlist per rung and has no shared timeline, so rungs need
// not be segment-aligned here (a switch realigns on the next segment) - only
// each representation is checked on its own.

import (
	"context"
	"fmt"
	"strings"
)

// HLSFromCMAF returns the HLS playlists describing the presentation, keyed by
// file name: "master.m3u8" (the multivariant playlist) and "<id>.m3u8" for
// every representation (its media playlist, EXT-X-MAP on the init, one
// EXTINF per segment). All of them sit in the same directory as the DASH
// manifest would, so the same URLPrefix serves both. Audio representations
// form one EXT-X-MEDIA group; the first is DEFAULT=YES (an external init's
// track flags say "enabled", not "default"). Encryption is not described.
func HLSFromCMAF(ctx context.Context, p CMAFPresentation, opts ...Options) (map[string][]byte, error) {
	o := optionsFrom(opts)
	video, audio, err := readCMAFPresentation(ctx, &o, p)
	if err != nil {
		return nil, err
	}
	// buildMediaPlaylist emits EXT-X-KEY lines from these; external fragments
	// are described in the clear.
	o.Encrypt, o.CENC = nil, nil

	out := map[string][]byte{"master.m3u8": buildCMAFMaster(&o, video, audio)}
	for _, r := range append(append([]*cmafRep{}, video...), audio...) {
		durs := make([]float64, len(r.segs))
		for i, s := range r.segs {
			durs[i] = float64(s.dur) / float64(r.timescale)
		}
		urls := r.segURLs
		out[r.id+".m3u8"] = buildMediaPlaylist(&o, durs, r.initURL, func(i int) string { return urls[i] }, nil)
	}
	return out, nil
}

// buildCMAFMaster writes master.m3u8: the audio group, then one
// EXT-X-STREAM-INF per video representation with its real
// BANDWIDTH/RESOLUTION/FRAME-RATE/CODECS. Without video, every audio
// representation is its own audio-only variant (no group: there is nothing
// to attach it to).
func buildCMAFMaster(o *Options, video, audio []*cmafRep) []byte {
	rw := urlRewriter(o)
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	if len(video) == 0 {
		for _, r := range audio {
			inf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d", r.bandwidth)
			if cs := cmafCodecs(r.tracks); cs != "" {
				inf += fmt.Sprintf(",CODECS=%q", cs)
			}
			fmt.Fprintf(&b, "%s\n%s\n", inf, rw(r.id+".m3u8"))
		}
		return []byte(b.String())
	}

	var (
		audioCodecs  []string
		audioUnknown bool // one audio codec without a string: no CODECS anywhere
	)
	for i, r := range audio {
		t := &r.tracks[r.primary]
		name := t.Name
		if name == "" {
			name = t.ResolvedLanguage()
		}
		if name == "" {
			name = fmt.Sprintf("Audio %d", i+1)
		}
		attrs := fmt.Sprintf("TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%q,AUTOSELECT=YES", name)
		if l := t.ResolvedLanguage(); l != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%q", l)
		}
		attrs += hlsChannelsAttr(t)
		if i == 0 {
			attrs += ",DEFAULT=YES"
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw(r.id+".m3u8"))
		switch cs := cmafCodecs(r.tracks); {
		case cs == "":
			audioUnknown = true
		case !containsString(audioCodecs, cs):
			audioCodecs = append(audioCodecs, cs)
		}
	}

	for _, r := range video {
		t := &r.tracks[r.primary]
		inf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d", r.bandwidth)
		if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
			inf += fmt.Sprintf(",RESOLUTION=%dx%d", *t.Width, *t.Height)
		}
		if t.FrameRate != nil && *t.FrameRate > 0 {
			inf += fmt.Sprintf(",FRAME-RATE=%.3f", *t.FrameRate)
		}
		if vcs := cmafCodecs(r.tracks); vcs != "" && !audioUnknown {
			inf += fmt.Sprintf(",CODECS=%q", strings.Join(append([]string{vcs}, audioCodecs...), ","))
		}
		if len(audio) > 0 {
			inf += ",AUDIO=\"aud\""
		}
		fmt.Fprintf(&b, "%s\n%s\n", inf, rw(r.id+".m3u8"))
	}
	return []byte(b.String())
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
