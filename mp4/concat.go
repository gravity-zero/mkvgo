package mp4

// concat.go - RemuxConcatToHLS: file-granularity HLS concatenation. Several
// sources (e.g. consecutive episodes) become ONE continuous HLS session: a
// single master.m3u8/playlist.m3u8/audioN.m3u8/subN.m3u8, so a player never
// reloads and never sees a session boundary. Each source keeps packaging into
// its own "part" (p0/, p1/, ...) exactly as RemuxToHLS would on its own, with
// no re-timestamping and no copy, and the top-level playlists stitch the
// parts together with EXT-X-DISCONTINUITY, the HLS-native "new timeline
// starts here" signal. This is the concatenation twin of RemuxToABR/PlanABR
// (abr.go): a composition of per-source HLSPlans instead of quality variants.
//
// Compatibility: every part must share the same video codec family and the
// same kept-audio layout (count, codec, language, in order); otherwise a
// single variant playlist could not describe every part, and packaging
// refuses up front (a cheap track-metadata probe, before anything is
// written). Subtitles are softer: they ride along only when every part
// exposes the same rendition layout (count/language/name/forced); otherwise
// they are dropped from the concatenated presentation (Options.OnDrop),
// leaving the video/audio concatenation intact.
//
// Subtitle cue times are the one place concat DOES rewrite content: WebVTT
// cues carry absolute presentation time and, unlike the CMAF fragments, are
// not reset by EXT-X-DISCONTINUITY, so part k's cues are shifted by the
// cumulative duration of parts 0..k-1 before they are served. Video/audio
// segments are never rewritten: they stay byte-identical to each part's own
// standalone packaging, which is what lets a part's segments be cached and
// served identically whether played standalone or as part of a concatenated
// session.
//
// v1 (this slice) does not support Options.Encrypt or Options.SingleFile, and
// emits no combined DASH manifest (DASH shares one SegmentTimeline per
// AdaptationSet; independent per-part timelines have nothing to share it
// over, exactly the ABR non-aligned rationale) and no I-frame playlist for
// the concatenation as a whole (each part's own trick-play, if any, stays
// under its own p{k}/ prefix, unlisted).

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// validateConcatOptions rejects the Options this slice does not support, so
// the caller finds out before anything is opened or written.
func validateConcatOptions(o *Options) error {
	if o.Encrypt != nil {
		return errf("concat presentations do not support Encrypt (v1): AES-128 key rotation across parts is not yet defined")
	}
	if o.SingleFile {
		return errf("concat presentations do not support SingleFile (v1): byte-range playlists reference one file per rendition, which a per-part concatenation does not have")
	}
	return nil
}

// audioSig is the compatibility-relevant signature of one audio rendition:
// its codec and language, compared in order across parts.
type audioSig struct{ codec, lang string }

// concatProbe is one source's compatibility facts, gathered from its track
// metadata alone (no sample data read, nothing written) so an incompatible
// set of sources is rejected before any packaging work happens.
type concatProbe struct {
	videoCodec string
	audio      []audioSig
}

// probeConcatSource opens src just far enough to plan its tracks (the same
// selection RemuxToHLS/PlanHLS apply: KeepTracks, one video, subtitle
// eligibility) and reports its video codec and its audio layout, without
// reading any sample.
func probeConcatSource(ctx context.Context, src string, o *Options) (concatProbe, error) {
	ps, err := openPackagingSource(ctx, src, o.FS)
	if err != nil {
		return concatProbe{}, err
	}
	defer ps.Close()
	planned, _, err := planTracks(ps.c, *o)
	if err != nil {
		return concatProbe{}, err
	}
	keep := keepTrackSet(o)
	var p concatProbe
	videoSeen := false
	for _, t := range planned {
		if t.isChapter || t.spec.text {
			continue
		}
		if keep != nil && !keep[t.mkv.ID] {
			continue
		}
		if t.spec.video {
			if videoSeen {
				continue // secondary video, dropped like RemuxToHLS/PlanHLS do
			}
			videoSeen = true
			p.videoCodec = t.mkv.Codec
			continue
		}
		p.audio = append(p.audio, audioSig{codec: t.mkv.Codec, lang: t.mkv.Language})
	}
	if !videoSeen {
		return concatProbe{}, errf("%s: no video track (concat requires one in every part)", src)
	}
	return p, nil
}

// validateConcatCompat refuses an incompatible set of sources with a precise
// error listing every mismatch, comparing every part against part 1.
func validateConcatCompat(probes []concatProbe) error {
	var problems []string
	ref := probes[0]
	for k := 1; k < len(probes); k++ {
		p := probes[k]
		if p.videoCodec != ref.videoCodec {
			problems = append(problems, fmt.Sprintf("part %d video codec %q differs from part 1's %q", k+1, p.videoCodec, ref.videoCodec))
			continue
		}
		if len(p.audio) != len(ref.audio) {
			problems = append(problems, fmt.Sprintf("part %d has %d audio track(s), part 1 has %d", k+1, len(p.audio), len(ref.audio)))
			continue
		}
		for i := range p.audio {
			if p.audio[i] != ref.audio[i] {
				problems = append(problems, fmt.Sprintf("part %d audio %d is %s/%s, part 1's is %s/%s",
					k+1, i+1, p.audio[i].codec, p.audio[i].lang, ref.audio[i].codec, ref.audio[i].lang))
			}
		}
	}
	if len(problems) > 0 {
		return errf("concat sources are not compatible: %s", strings.Join(problems, "; "))
	}
	return nil
}

// audioFts returns the indices, in fts order, of the non-primary (audio)
// renditions: the same order renditionInit/renditionSegment/renditionPlaylist
// already key their naming on.
func audioFts(fts []*fragTrack) []int {
	p := primaryIndex(fts)
	var out []int
	for i, ft := range fts {
		if i != p && !ft.outTrack.spec.video {
			out = append(out, i)
		}
	}
	return out
}

// subSig is the compatibility-relevant signature of one subtitle rendition.
type subSig struct {
	lang, name string
	forced     bool
}

func subLayout(subs []hlsSubTrack) []subSig {
	out := make([]subSig, len(subs))
	for i := range subs {
		t := &subs[i].track
		out[i] = subSig{lang: t.Language, name: t.Name, forced: t.IsForced}
	}
	return out
}

// subsAligned reports whether every part exposes the same subtitle rendition
// layout (count, language, name, forced flag, in order): the condition for
// emitting concatenated subtitle renditions at all.
func subsAligned(results []*hlsResult) bool {
	ref := subLayout(results[0].subs)
	for _, r := range results[1:] {
		rl := subLayout(r.subs)
		if len(rl) != len(ref) {
			return false
		}
		for i := range ref {
			if rl[i] != ref[i] {
				return false
			}
		}
	}
	return true
}

// avgBandwidth is peakBandwidth's average-bitrate counterpart, over the same
// per-segment durations/sizes.
func avgBandwidth(segs []segInfo) int64 {
	var totalBits, totalSec float64
	for _, s := range segs {
		totalBits += float64(s.bytes) * 8
		totalSec += s.durSec
	}
	if totalSec <= 0 {
		return peakBandwidth(segs)
	}
	return int64(totalBits / totalSec)
}

// cumulativeDurationsMs returns, for each part, the total presentation
// duration (ms) of every part before it: the shift a part's subtitle cues
// need to land on the concatenated timeline.
func cumulativeDurationsMs(results []*hlsResult) []int64 {
	cum := make([]int64, len(results))
	for k := 1; k < len(results); k++ {
		cum[k] = cum[k-1] + pickVideoFrag(results[k-1].fts).presentMs
	}
	return cum
}

// shiftCues returns a copy of cues with every timestamp shifted by shiftMs.
func shiftCues(cues []subtitle.Cue, shiftMs int64) []subtitle.Cue {
	out := make([]subtitle.Cue, len(cues))
	for i, c := range cues {
		out[i] = c
		if shiftMs != 0 {
			out[i].StartMs += shiftMs
			out[i].EndMs += shiftMs
		}
	}
	return out
}

// shiftWindowVTT filters cues (already end-resolved, part-local times) to the
// part-local window [segStart, segEnd), shifts the surviving ones by shiftMs
// and renders them as WebVTT.
func shiftWindowVTT(cues []subtitle.Cue, segStart, segEnd, shiftMs int64) ([]byte, error) {
	var window []subtitle.Cue
	for _, c := range cues {
		if c.EndMs > segStart && c.StartMs < segEnd {
			window = append(window, c)
		}
	}
	window = shiftCues(window, shiftMs)
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, window); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// buildConcatPlaylist renders one concatenated HLS media playlist: part 0's
// segments preceded by EXT-X-MAP (when mapURIs[0] != ""), each following part
// preceded by EXT-X-DISCONTINUITY and its own EXT-X-MAP: the HLS-native
// timeline reset, so no fragment needs re-timestamping. Version 6: a media
// playlist that carries EXT-X-MAP needs at least that.
func buildConcatPlaylist(o *Options, durs [][]float64, mapURIs []string, segName func(part, i int) string) []byte {
	rw := urlRewriter(o)
	var max float64
	for _, pd := range durs {
		for _, d := range pd {
			if d > max {
				max = d
			}
		}
	}
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:6\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int64(max+0.999))...)
	b = append(b, "#EXT-X-PLAYLIST-TYPE:VOD\n"...)
	for k, pd := range durs {
		if k > 0 {
			b = append(b, "#EXT-X-DISCONTINUITY\n"...)
		}
		if mapURIs[k] != "" {
			b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q\n", rw(mapURIs[k]))...)
		}
		for i, d := range pd {
			b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n%s\n", d, rw(segName(k, i)))...)
		}
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return b
}

// buildConcatVideoPlaylist renders the concatenated playlist.m3u8, the video
// rendition: part k's video segments live under p{k}/, reusing that part's
// own init/segment names.
func buildConcatVideoPlaylist(o *Options, results []*hlsResult) []byte {
	durs := make([][]float64, len(results))
	mapURIs := make([]string, len(results))
	for k, res := range results {
		durs[k] = res.durs
		vi := primaryIndex(res.fts)
		mapURIs[k] = fmt.Sprintf("p%d/%s", k, renditionInit(res.fts, vi))
	}
	segName := func(k, i int) string {
		vi := primaryIndex(results[k].fts)
		return fmt.Sprintf("p%d/%s", k, renditionSegment(results[k].fts, vi, i))
	}
	return buildConcatPlaylist(o, durs, mapURIs, segName)
}

// buildConcatAudioPlaylist renders the concatenated audio{j+1}.m3u8 (0-based
// j into the shared, validated audio layout).
func buildConcatAudioPlaylist(o *Options, results []*hlsResult, j int) []byte {
	durs := make([][]float64, len(results))
	mapURIs := make([]string, len(results))
	for k, res := range results {
		durs[k] = res.durs
		ai := audioFts(res.fts)[j]
		mapURIs[k] = fmt.Sprintf("p%d/%s", k, renditionInit(res.fts, ai))
	}
	segName := func(k, i int) string {
		ai := audioFts(results[k].fts)[j]
		return fmt.Sprintf("p%d/%s", k, renditionSegment(results[k].fts, ai, i))
	}
	return buildConcatPlaylist(o, durs, mapURIs, segName)
}

// buildConcatSubPlaylist renders the concatenated sub{j+1}.m3u8: like a
// single-source subtitle playlist, no EXT-X-MAP (WebVTT renditions carry
// none), segments named after the shifted p{k}/sub{j+1}_%05d.vtt files.
func buildConcatSubPlaylist(o *Options, results []*hlsResult, j int) []byte {
	durs := make([][]float64, len(results))
	mapURIs := make([]string, len(results))
	for k, res := range results {
		durs[k] = res.durs
	}
	segName := func(k, i int) string {
		return fmt.Sprintf("p%d/sub%d_%05d.vtt", k, j+1, i+1)
	}
	return buildConcatPlaylist(o, durs, mapURIs, segName)
}

// buildConcatMaster renders the single-variant master.m3u8: BANDWIDTH/
// AVERAGE-BANDWIDTH the max across parts, RESOLUTION the max area, CODECS
// from part 0 (already validated compatible with every other part), audio and
// subtitle groups pointing at the top-level concatenated playlists.
func buildConcatMaster(o *Options, results []*hlsResult) []byte {
	rw := urlRewriter(o)
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n"...)

	ref := results[0]
	audioIdx := audioFts(ref.fts)
	hasDefaultAudio := false
	for _, i := range audioIdx {
		hasDefaultAudio = hasDefaultAudio || ref.fts[i].outTrack.mkv.IsDefault
	}
	for j, i := range audioIdx {
		t := &ref.fts[i].outTrack.mkv
		name := t.Name
		if name == "" && t.Language != "" {
			name = t.Language
		}
		if name == "" {
			name = fmt.Sprintf("Audio %d", j+1)
		}
		attrs := fmt.Sprintf("TYPE=AUDIO,GROUP-ID=\"aud\",NAME=%q,AUTOSELECT=YES", name)
		if t.Language != "" {
			attrs += fmt.Sprintf(",LANGUAGE=%q", t.Language)
		}
		if t.IsDefault || (!hasDefaultAudio && j == 0) {
			attrs += ",DEFAULT=YES"
		}
		b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw(fmt.Sprintf("audio%d.m3u8", j+1)))...)
	}

	subsOK := subsAligned(results)
	if subsOK {
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
			b = append(b, fmt.Sprintf("#EXT-X-MEDIA:%s,URI=%q\n", attrs, rw(fmt.Sprintf("sub%d.m3u8", i+1)))...)
		}
	}

	var peak, avg int64
	var maxArea int64
	var w, h uint32
	for _, res := range results {
		if p := peakBandwidth(res.segs); p > peak {
			peak = p
		}
		if a := avgBandwidth(res.segs); a > avg {
			avg = a
		}
		if v := pickVideoFrag(res.fts); v != nil {
			t := &v.outTrack.mkv
			if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
				if area := int64(*t.Width) * int64(*t.Height); area > maxArea {
					maxArea, w, h = area, *t.Width, *t.Height
				}
			}
		}
	}
	inf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d", peak, avg)
	if maxArea > 0 {
		inf += fmt.Sprintf(",RESOLUTION=%dx%d", w, h)
	}
	if v := pickVideoFrag(ref.fts); v != nil {
		t := &v.outTrack.mkv
		if t.FrameRate != nil && *t.FrameRate > 0 {
			inf += fmt.Sprintf(",FRAME-RATE=%.3f", *t.FrameRate)
		}
	}
	if codecs := hlsCodecsAttr(ref.fts); codecs != "" {
		inf += fmt.Sprintf(",CODECS=%q", codecs)
	}
	if len(audioIdx) > 0 {
		inf += ",AUDIO=\"aud\""
	}
	if subsOK && len(ref.subs) > 0 {
		inf += ",SUBTITLES=\"subs\""
	}
	b = append(b, (inf + "\n" + rw("playlist.m3u8") + "\n")...)
	return b
}

// writeConcatSubtitles writes the concatenated subtitle renditions (full
// pass): for every subtitle index, each part's already-collected, end-
// resolved cues (hlsResult.subs[j].cues) are windowed on that part's own
// bounds, shifted onto the concatenated timeline, and written as
// p{k}/sub{j+1}_%05d.vtt; the whole shifted, concatenated cue list also
// becomes the top-level sub{j+1}.vtt. When the parts' subtitle layouts do not
// match, subtitles are dropped (Options.OnDrop) and nothing subtitle-related
// is written.
func writeConcatSubtitles(o *Options, fs *mkv.FS, outDir string, results []*hlsResult, cumMs []int64) error {
	if !subsAligned(results) {
		for i := range results[0].subs {
			t := &results[0].subs[i].track
			o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
				Reason: "subtitle rendition layout differs across the concatenated sources (count/language/name/forced must match); subtitles dropped from the concatenated presentation"})
		}
		return nil
	}
	for j := range results[0].subs {
		var whole []subtitle.Cue
		for k, res := range results {
			bounds := res.bounds
			cuesLocal := res.subs[j].cues
			for n := range res.durs {
				segStart := bounds[n]
				var segEnd int64 = math.MaxInt64
				if n+1 < len(bounds) {
					segEnd = bounds[n+1]
				}
				data, err := shiftWindowVTT(cuesLocal, segStart, segEnd, cumMs[k])
				if err != nil {
					return err
				}
				name := filepath.Join(outDir, fmt.Sprintf("p%d", k), fmt.Sprintf("sub%d_%05d.vtt", j+1, n+1))
				if err := fs.DoWriteFile(name, data, 0o644); err != nil {
					return err
				}
			}
			whole = append(whole, shiftCues(cuesLocal, cumMs[k])...)
		}
		var buf strings.Builder
		if err := subtitle.WriteWebVTT(&buf, whole); err != nil {
			return err
		}
		if err := fs.DoWriteFile(filepath.Join(outDir, fmt.Sprintf("sub%d.vtt", j+1)), []byte(buf.String()), 0o644); err != nil {
			return err
		}
		pl := buildConcatSubPlaylist(o, results, j)
		if err := fs.DoWriteFile(filepath.Join(outDir, fmt.Sprintf("sub%d.m3u8", j+1)), pl, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RemuxConcatToHLS packages sources - several files played as ONE continuous
// HLS session (e.g. consecutive episodes) - into outputDir: a single
// master.m3u8/playlist.m3u8/audioN.m3u8[/subN.m3u8] spanning every part, with
// no player reload and no re-timestamping. Each source packages into its own
// p{k}/ (p0/, p1/, ...) exactly as RemuxToHLS would on its own; the
// concatenated playlists reference those parts' segments directly, with
// EXT-X-DISCONTINUITY marking each part boundary. Options apply to every
// source uniformly. See the package doc above for the compatibility contract
// and the v1 limits (no Encrypt, no SingleFile, no combined DASH manifest, no
// combined I-frame playlist).
func RemuxConcatToHLS(ctx context.Context, sources []string, outputDir string, opts ...Options) error {
	if len(sources) < 2 {
		return errf("concat packaging needs at least two sources (got %d); use RemuxToHLS for one", len(sources))
	}
	o := optionsFrom(opts)
	if err := validateConcatOptions(&o); err != nil {
		return err
	}
	probes := make([]concatProbe, len(sources))
	for k, src := range sources {
		p, err := probeConcatSource(ctx, src, &o)
		if err != nil {
			return errf("part %d (%s): %w", k+1, src, err)
		}
		probes[k] = p
	}
	if err := validateConcatCompat(probes); err != nil {
		return err
	}

	fs := o.FS
	if err := fs.DoMkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	results := make([]*hlsResult, len(sources))
	for k, src := range sources {
		sub := filepath.Join(outputDir, fmt.Sprintf("p%d", k))
		res, err := remuxToHLSInto(ctx, src, sub, &o)
		if err != nil {
			return errf("part %d (%s): %w", k+1, src, err)
		}
		results[k] = res
	}

	if err := fs.DoWriteFile(filepath.Join(outputDir, "master.m3u8"), buildConcatMaster(&o, results), 0o644); err != nil {
		return err
	}
	if err := fs.DoWriteFile(filepath.Join(outputDir, "playlist.m3u8"), buildConcatVideoPlaylist(&o, results), 0o644); err != nil {
		return err
	}
	for j := range audioFts(results[0].fts) {
		name := fmt.Sprintf("audio%d.m3u8", j+1)
		if err := fs.DoWriteFile(filepath.Join(outputDir, name), buildConcatAudioPlaylist(&o, results, j), 0o644); err != nil {
			return err
		}
	}
	return writeConcatSubtitles(&o, fs, outputDir, results, cumulativeDurationsMs(results))
}

// parsePartPath splits "p{k}/<rest>" into k (0-based) and the remainder, like
// abrplan.go's parseVariantPath but 0-based (p0, p1, ...) to match the
// concat's part indexing.
func parsePartPath(name string) (k int, rest string, ok bool) {
	if len(name) < 2 || name[0] != 'p' {
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
