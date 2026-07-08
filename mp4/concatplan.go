package mp4

// concatplan.go - PlanConcat: the on-demand counterpart of RemuxConcatToHLS.
// Given several sources plannable by PlanHLS, it plans each one and serves
// the whole concatenated session resource by resource, nothing pre-generated:
// video/audio segments delegate straight to each part's own HLSPlan (byte-
// identical to that part's standalone plan and to the full pass), and the
// concatenated playlists/subtitles are the same builders concat.go's full
// pass uses, so both sides describe the presentation identically.
//
// Resource names mirror RemuxConcatToHLS's layout: "master.m3u8",
// "playlist.m3u8", "audio{j}.m3u8", "sub{j}.m3u8"/"sub{j}.vtt" at the top, and
// "p{k}/<name>" (k 0-based) for any resource of part k: exactly the URIs the
// top playlists and RemuxConcatToHLS's directories use.

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// ConcatPlan serves a concatenated multi-source HLS session on demand. It is
// immutable after PlanConcat returns; Resource calls are safe to run
// concurrently (each part plan opens its own reader per segment/subtitle
// scan).
type ConcatPlan struct {
	parts   []*HLSPlan
	results []*hlsResult
	cumMs   []int64 // cumulative duration (ms) of every part before k

	master   []byte
	playlist []byte
	audio    [][]byte // audio{j+1}.m3u8, 0-based j
	subsOK   bool
	subPl    [][]byte // sub{j+1}.m3u8, 0-based j (only when subsOK)

	opts Options
}

// PlanConcat plans sources - several files played as ONE continuous HLS
// session - and returns a plan that serves it resource by resource. Sources
// must satisfy PlanHLS's requirements (a Cues index for Matroska, or an
// MP4/MOV sample table) and must be compatible: same video codec family, same
// kept-audio layout (count, codec, language, in order), checked from track
// metadata alone before any source is planned, so an incompatible set fails
// fast. Options apply to every source uniformly. See concat.go for the v1
// limits (no Encrypt, no SingleFile, no combined DASH manifest, no combined
// I-frame playlist).
func PlanConcat(ctx context.Context, sources []string, opts ...Options) (*ConcatPlan, error) {
	if len(sources) < 2 {
		return nil, errf("concat planning needs at least two sources (got %d); use PlanHLS for one", len(sources))
	}
	o := optionsFrom(opts)
	if err := validateConcatOptions(&o); err != nil {
		return nil, err
	}
	probes := make([]concatProbe, len(sources))
	for k, src := range sources {
		p, err := probeConcatSource(ctx, src, &o)
		if err != nil {
			return nil, errf("part %d (%s): %w", k+1, src, err)
		}
		probes[k] = p
	}
	if err := validateConcatCompat(probes); err != nil {
		return nil, err
	}

	parts := make([]*HLSPlan, len(sources))
	results := make([]*hlsResult, len(sources))
	for k, src := range sources {
		pl, err := PlanHLS(ctx, src, o)
		if err != nil {
			return nil, errf("part %d (%s): %w", k+1, src, err)
		}
		parts[k] = pl
		results[k] = pl.hlsResult()
	}

	cp := &ConcatPlan{parts: parts, results: results, opts: o}
	cp.cumMs = cumulativeDurationsMs(results)
	cp.master = buildConcatMaster(&o, results)
	cp.playlist = buildConcatVideoPlaylist(&o, results)
	for j := range audioFts(results[0].fts) {
		cp.audio = append(cp.audio, buildConcatAudioPlaylist(&o, results, j))
	}
	cp.subsOK = subsAligned(results)
	if !cp.subsOK {
		for i := range results[0].subs {
			t := &results[0].subs[i].track
			o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
				Reason: "subtitle rendition layout differs across the concatenated sources (count/language/name/forced must match); subtitles dropped from the concatenated presentation"})
		}
	} else {
		for j := range results[0].subs {
			cp.subPl = append(cp.subPl, buildConcatSubPlaylist(&o, results, j))
		}
	}
	return cp, nil
}

// NumParts returns how many sources the concatenated session carries.
func (cp *ConcatPlan) NumParts() int { return len(cp.parts) }

// Part returns part k's underlying single-source plan (0-based), for callers
// that want its NumSegments/Segment directly.
func (cp *ConcatPlan) Part(k int) *HLSPlan { return cp.parts[k] }

// MasterPlaylist returns the concatenated master.m3u8.
func (cp *ConcatPlan) MasterPlaylist() []byte { return cp.master }

// isConcatSuperseded reports whether a part's own resource name is replaced
// by a top-level concatenated one (its own master/playlist/audio/subtitle
// playlists and manifest, plus the I-frame playlist this slice does not
// carry forward) and so is left out of the concatenated Resources() list.
// Windowed subtitle segments (sub{j}_%05d.vtt) are the one per-part name kept:
// reused, but re-served shifted onto the concatenated timeline.
func isConcatSuperseded(name string) bool {
	switch name {
	case "master.m3u8", "playlist.m3u8", "manifest.mpd", "iframe.m3u8":
		return true
	}
	var n int
	if _, err := fmt.Sscanf(name, "audio%d.m3u8", &n); err == nil && name == fmt.Sprintf("audio%d.m3u8", n) {
		return true
	}
	if _, err := fmt.Sscanf(name, "sub%d.m3u8", &n); err == nil && name == fmt.Sprintf("sub%d.m3u8", n) {
		return true
	}
	if _, err := fmt.Sscanf(name, "sub%d.vtt", &n); err == nil && name == fmt.Sprintf("sub%d.vtt", n) {
		return true
	}
	return false
}

// Resources returns every resource name the concatenated session serves: the
// top-level master, video and audio playlists, the subtitle playlists/whole
// VTTs (only when the parts' subtitle layouts align), then each part's own
// resources under its p{k}/ prefix.
func (cp *ConcatPlan) Resources() []string {
	names := []string{"master.m3u8", "playlist.m3u8"}
	for j := range cp.audio {
		names = append(names, fmt.Sprintf("audio%d.m3u8", j+1))
	}
	if cp.subsOK {
		for j := range cp.subPl {
			names = append(names, fmt.Sprintf("sub%d.m3u8", j+1), fmt.Sprintf("sub%d.vtt", j+1))
		}
	}
	for k, p := range cp.parts {
		for _, r := range p.Resources() {
			if isConcatSuperseded(r) {
				continue
			}
			names = append(names, fmt.Sprintf("p%d/%s", k, r))
		}
	}
	return names
}

// Resource builds the named resource and returns its bytes and Content-Type.
// name is exactly the URI the concatenated playlists reference: "master.m3u8",
// "playlist.m3u8", "audio1.m3u8", "sub1.vtt", or "p{k}/<name>" for a resource
// of part k (k 0-based): the same names RemuxConcatToHLS's directories use.
func (cp *ConcatPlan) Resource(ctx context.Context, name string) ([]byte, string, error) {
	const (
		mimeM3U8 = "application/vnd.apple.mpegurl"
		mimeVTT  = "text/vtt"
	)
	switch name {
	case "master.m3u8":
		return cp.master, mimeM3U8, nil
	case "playlist.m3u8":
		return cp.playlist, mimeM3U8, nil
	}
	var j int
	if _, err := fmt.Sscanf(name, "audio%d.m3u8", &j); err == nil && name == fmt.Sprintf("audio%d.m3u8", j) {
		if j < 1 || j > len(cp.audio) {
			return nil, "", errf("unknown concat resource %q", name)
		}
		return cp.audio[j-1], mimeM3U8, nil
	}
	if _, err := fmt.Sscanf(name, "sub%d.m3u8", &j); err == nil && name == fmt.Sprintf("sub%d.m3u8", j) {
		if !cp.subsOK || j < 1 || j > len(cp.subPl) {
			return nil, "", errf("no subtitle rendition %d in the concatenated presentation (the parts' subtitle layouts do not align)", j)
		}
		return cp.subPl[j-1], mimeM3U8, nil
	}
	if _, err := fmt.Sscanf(name, "sub%d.vtt", &j); err == nil && name == fmt.Sprintf("sub%d.vtt", j) {
		data, err := cp.wholeSubtitle(ctx, j-1)
		return data, mimeVTT, err
	}

	k, rest, ok := parsePartPath(name)
	if !ok || k < 0 || k >= len(cp.parts) {
		return nil, "", errf("unknown concat resource %q", name)
	}
	var pj, pn int
	if _, err := fmt.Sscanf(rest, "sub%d_%d.vtt", &pj, &pn); err == nil && rest == fmt.Sprintf("sub%d_%05d.vtt", pj, pn) {
		data, err := cp.windowedSubtitle(ctx, k, pj-1, pn-1)
		return data, mimeVTT, err
	}
	return cp.parts[k].Resource(ctx, rest)
}

// windowedSubtitle builds part k's n-th windowed subtitle segment (sub{j+1}_
// %05d.vtt), shifted onto the concatenated timeline: the on-demand
// counterpart of writeConcatSubtitles' per-segment file.
func (cp *ConcatPlan) windowedSubtitle(ctx context.Context, k, j, n int) ([]byte, error) {
	if !cp.subsOK {
		return nil, errf("no subtitle rendition %d in the concatenated presentation (the parts' subtitle layouts do not align)", j+1)
	}
	p := cp.parts[k]
	if j < 0 || j >= len(p.subs) {
		return nil, errf("subtitle rendition %d out of range (0..%d)", j, len(p.subs)-1)
	}
	if n < 0 || n >= p.segCount {
		return nil, errf("subtitle segment %d out of range for part %d (0..%d)", n, k, p.segCount-1)
	}
	segStart := p.bounds[n]
	var segEnd int64 = math.MaxInt64
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}
	cues, err := p.subCuesForWindow(ctx, j, segStart, segEnd)
	if err != nil {
		return nil, err
	}
	return shiftWindowVTT(cues, segStart, segEnd, cp.cumMs[k])
}

// wholeSubtitle builds the j-th subtitle rendition's whole-presentation
// WebVTT: every part's cues, shifted onto the concatenated timeline and
// concatenated in part order.
func (cp *ConcatPlan) wholeSubtitle(ctx context.Context, j int) ([]byte, error) {
	if !cp.subsOK || j < 0 || j >= len(cp.subPl) {
		return nil, errf("no subtitle rendition %d in the concatenated presentation (the parts' subtitle layouts do not align)", j+1)
	}
	var all []subtitle.Cue
	for k, p := range cp.parts {
		cues, err := p.subCuesThrough(ctx, j, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		all = append(all, shiftCues(cues, cp.cumMs[k])...)
	}
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, all); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
