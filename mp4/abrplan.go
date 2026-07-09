package mp4

// abrplan.go — PlanABR: the on-demand counterpart of RemuxToABR. Given several
// pre-encoded quality variants of one title (best first), it plans each with
// PlanHLS (the reference complete, the rest video-only) and serves the whole
// multi-variant presentation resource by resource, nothing pre-generated.
//
// Resource names are the ABR layout: "master.m3u8" for the top manifest, and
// "v{k}/<name>" for any resource of variant k (playlist.m3u8, init.mp4,
// seg00007.m4s, audioN.m3u8, subN.vtt, …) — exactly the URIs the master and
// RemuxToABR's directories use, so a serving handler is one call to Resource. Each
// v{k}/<name> is byte-identical to the file RemuxToABR would have written.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ABRPlan serves a multi-variant HLS presentation on demand. It is immutable
// after PlanABR returns; Resource calls are safe to run concurrently (each
// variant plan opens its own reader per segment).
type ABRPlan struct {
	variants []*HLSPlan
	master   []byte
	mpd      []byte // combined DASH manifest, when the variants are segment-aligned
}

// PlanABR plans the sources — quality variants of the same content, best first
// — as one multi-variant HLS presentation served on demand. The first source is
// the reference (its audio and subtitles serve every variant); the rest
// contribute only their video rendition, exactly like RemuxToABR. Options apply
// to every variant. Each source must carry a Cues index (MKV) or be a
// progressive MP4/MOV, as PlanHLS requires.
func PlanABR(ctx context.Context, sources []string, opts ...Options) (*ABRPlan, error) {
	if len(sources) < 2 {
		return nil, errf("ABR planning needs at least two sources (got %d) — use PlanHLS for one", len(sources))
	}
	o := optionsFrom(opts)
	variants := make([]*HLSPlan, len(sources))
	results := make([]*hlsResult, len(sources))
	for i, src := range sources {
		vo := o
		vo.VideoOnly = i > 0
		pl, err := PlanHLS(ctx, src, vo)
		if err != nil {
			return nil, errf("variant %d (%s): %w", i+1, src, err)
		}
		variants[i] = pl
		results[i] = pl.hlsResult()
	}
	return &ABRPlan{variants: variants, master: buildABRMaster(o, results), mpd: combinedDASH(o, results)}, nil
}

// MasterPlaylist returns the multi-variant master.m3u8.
func (a *ABRPlan) MasterPlaylist() []byte { return a.master }

// NumVariants returns how many quality variants the presentation carries.
func (a *ABRPlan) NumVariants() int { return len(a.variants) }

// Variant returns variant k's underlying single-source plan (0-based), for
// callers that want its NumSegments/Segment directly.
func (a *ABRPlan) Variant(k int) *HLSPlan { return a.variants[k] }

// Resource resolves any resource of the presentation and returns its bytes and
// Content-Type. name is "master.m3u8" for the top manifest, or "v{k}/<name>"
// (k is 1-based, matching the master's URIs) for a resource of variant k.
func (a *ABRPlan) Resource(ctx context.Context, name string) ([]byte, string, error) {
	if name == "master.m3u8" {
		return a.master, "application/vnd.apple.mpegurl", nil
	}
	if name == "manifest.mpd" {
		if a.mpd == nil {
			return nil, "", errf("no combined DASH manifest (the variants are not segment-aligned, or the presentation is encrypted); use master.m3u8, or each v{k}/manifest.mpd")
		}
		return a.mpd, "application/dash+xml", nil
	}
	k, rest, ok := parseVariantPath(name)
	if !ok || k < 1 || k > len(a.variants) {
		return nil, "", errf("unknown ABR resource %q", name)
	}
	return a.variants[k-1].Resource(ctx, rest)
}

// Resources lists every resource name the presentation serves: the master, then
// each variant's resources under its v{k}/ prefix. A variant's own master.m3u8
// and manifest.mpd are superseded by the ABR master and omitted (combined DASH
// is not emitted — independently encoded variants have unaligned timelines).
func (a *ABRPlan) Resources() []string {
	names := []string{"master.m3u8"}
	if a.mpd != nil {
		names = append(names, "manifest.mpd")
	}
	for i, v := range a.variants {
		for _, r := range v.Resources() {
			if r == "master.m3u8" || r == "manifest.mpd" {
				continue
			}
			names = append(names, fmt.Sprintf("v%d/%s", i+1, r))
		}
	}
	return names
}

// parseVariantPath splits "v{k}/<rest>" into k (1-based) and the remainder.
func parseVariantPath(name string) (k int, rest string, ok bool) {
	if len(name) < 2 || name[0] != 'v' {
		return 0, "", false
	}
	slash := strings.IndexByte(name, '/')
	if slash < 2 {
		return 0, "", false
	}
	k, err := strconv.Atoi(name[1:slash])
	if err != nil {
		return 0, "", false
	}
	return k, name[slash+1:], true
}
