package mp4

// cmafmpd.go - a DASH manifest over CMAF fragments packaged elsewhere.
// RemuxToHLS, RemuxToABR and the on-demand plans emit the MPD for the
// fragments they cut themselves; DASHFromCMAF emits the same manifest for an
// initialisation segment and media segments an external encoder already
// produced (one set per quality rung, optionally audio renditions). Nothing is
// re-packaged or copied: the init's moov gives each track's codec, dimensions,
// frame rate, sample rate, language and timescale; each media segment's moof
// gives its exact tick span (tfdt + trun durations); the manifest references
// the files where they are. The rungs of the video switch set are compared
// segment by segment before anything is emitted - a DASH player switching
// quality fetches segment N of the new rung expecting it to cover the same time
// as segment N of the old one, so misaligned rungs would play the wrong time.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// CMAFRepresentation is one rendition packaged by an external encoder: an
// initialisation segment and its media segments, read through Options.FS from
// Dir. Everything the manifest says about it comes from the files; the only
// declared facts are the ones the files cannot carry.
type CMAFRepresentation struct {
	// Dir holds the files; Init and Segments are names inside it.
	Dir string
	// Init is the initialisation segment: ftyp + moov with an mvex box.
	Init string
	// Segments are the media segments (moof + mdat, usually behind a styp),
	// in playback order.
	Segments []string
	// URLPrefix precedes Init and every segment name in the manifest: "" when
	// the manifest sits in Dir, "v1/" when it sits one level above. The
	// result goes through Options.RewriteURL like every other URI.
	URLPrefix string
	// ID is the Representation@id; empty picks "v{n}" (video) or "a{n}"
	// (audio) from the position.
	ID string
	// Bandwidth, in bits/s, overrides the value derived from the segments
	// (the peak segment bitrate, as the packager's own manifests report). 0
	// derives it.
	Bandwidth int64
}

// CMAFPresentation groups external representations for one manifest. Video
// is the switch set: one AdaptationSet with a Representation per rung, best
// first, validated segment-aligned. Each Audio entry gets an AdaptationSet of
// its own (one per language or codec; audio is selected, not switched). A
// video representation whose init also carries audio (muxed segments) is
// described as such: its codecs attribute lists both.
type CMAFPresentation struct {
	Video []CMAFRepresentation
	Audio []CMAFRepresentation
}

// DASHFromCMAF returns the static DASH manifest (manifest.mpd) describing the
// presentation, or reports what stops it from being valid: a missing or
// non-CMAF file, a segment out of order, rungs whose segments do not cover
// the same time spans. Segment names that count up by one ("seg00001.m4s",
// "seg00002.m4s", …) are addressed with a SegmentTemplate; any other naming
// with a SegmentList. Every timeline keeps its track's native timescale.
// Encryption is not described (Options.CENC and Options.Encrypt are ignored).
func DASHFromCMAF(ctx context.Context, p CMAFPresentation, opts ...Options) ([]byte, error) {
	o := optionsFrom(opts)
	video, audio, err := readCMAFPresentation(ctx, &o, p)
	if err != nil {
		return nil, err
	}
	if err := validateCMAFSwitchSet(video); err != nil {
		return nil, err
	}
	return renderCMAFManifest(&o, video, audio), nil
}

// readCMAFPresentation reads every representation's files and checks each
// one on its own (ids unique, segments in order, one video track for Video,
// none for Audio). What holds between rungs is the manifest's business:
// DASH validates the switch set, HLS carries one playlist per rung.
func readCMAFPresentation(ctx context.Context, o *Options, p CMAFPresentation) (video, audio []*cmafRep, err error) {
	if len(p.Video) == 0 {
		return nil, nil, errf("a CMAF presentation needs at least one video representation")
	}
	video = make([]*cmafRep, len(p.Video))
	audio = make([]*cmafRep, len(p.Audio))
	seen := map[string]bool{}
	read := func(dst []*cmafRep, src []CMAFRepresentation, prefix string, isVideo bool) error {
		for i := range src {
			if err := ctx.Err(); err != nil {
				return err
			}
			id := src[i].ID
			if id == "" {
				id = fmt.Sprintf("%s%d", prefix, i+1)
			}
			if seen[id] {
				return errf("representation id %q is used twice - give each representation its own ID", id)
			}
			seen[id] = true
			r, err := readCMAFRepresentation(o.FS, &src[i], id, isVideo)
			if err != nil {
				return err
			}
			dst[i] = r
		}
		return nil
	}
	if err := read(video, p.Video, "v", true); err != nil {
		return nil, nil, err
	}
	if err := read(audio, p.Audio, "a", false); err != nil {
		return nil, nil, err
	}
	return video, audio, nil
}

// cmafRep is a representation as read from its files.
type cmafRep struct {
	id        string
	tracks    []mkv.Track // the init's tracks, in moov order
	primary   int         // the track the timeline follows: the video track, else the first
	timescale uint32      // the primary track's
	segs      []cmafSeg
	initURL   string
	segURLs   []string
	bandwidth int64
}

// cmafSeg is one media segment on the primary track's timeline.
type cmafSeg struct {
	start, dur int64 // ticks
	bytes      int64
	sync       bool // the primary track's first sample is a sync sample
}

func (s cmafSeg) end() int64 { return s.start + s.dur }

// readCMAFRepresentation reads the init and walks every segment's moof.
func readCMAFRepresentation(fs *mkv.FS, r *CMAFRepresentation, id string, isVideo bool) (*cmafRep, error) {
	if r.Init == "" {
		return nil, errf("representation %q: no initialisation segment given (Init)", id)
	}
	if len(r.Segments) == 0 {
		return nil, errf("representation %q: no media segments given (Segments)", id)
	}
	mv, trex, err := readCMAFInit(fs, filepath.Join(r.Dir, r.Init))
	if err != nil {
		return nil, errf("representation %q: %w", id, err)
	}
	rep := &cmafRep{id: id, tracks: buildMKVTracks(mv, false)}
	hasVideo := false
	for i := range mv.tracks {
		if mv.tracks[i].trackType == mkv.VideoTrack {
			rep.primary, hasVideo = i, true
			break
		}
	}
	switch {
	case isVideo && !hasVideo:
		return nil, errf("representation %q: %s carries no video track - list audio-only renditions under Audio", id, r.Init)
	case !isVideo && hasVideo:
		return nil, errf("representation %q: %s carries a video track - list it under Video", id, r.Init)
	}
	primary := &mv.tracks[rep.primary]
	if primary.timescale == 0 {
		return nil, errf("representation %q: %s declares no timescale for track %d", id, r.Init, primary.trackID)
	}
	rep.timescale = primary.timescale

	rep.initURL = r.URLPrefix + r.Init
	rep.segURLs = make([]string, len(r.Segments))
	rep.segs = make([]cmafSeg, len(r.Segments))
	for k, name := range r.Segments {
		rep.segURLs[k] = r.URLPrefix + name
		seg, err := readCMAFSegment(fs, filepath.Join(r.Dir, name), primary.trackID, trex)
		if err != nil {
			return nil, errf("representation %q: segment %d (%s): %w", id, k+1, name, err)
		}
		if k > 0 && seg.start < rep.segs[k-1].end() {
			return nil, errf("representation %q: segment %d (%s) starts at %s, before segment %d ends (%s) - the segments must be listed in playback order and must not overlap",
				id, k+1, name, cmafSeconds(seg.start, rep.timescale), k, cmafSeconds(rep.segs[k-1].end(), rep.timescale))
		}
		rep.segs[k] = seg
	}

	rep.bandwidth = r.Bandwidth
	if rep.bandwidth == 0 {
		rep.bandwidth = cmafPeakBandwidth(rep.segs, rep.timescale)
	}
	return rep, nil
}

// readCMAFInit parses an initialisation segment's moov and its trex defaults.
func readCMAFInit(fs *mkv.FS, path string) (*movie, map[uint32]trexDefault, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := fs.DoStat(path)
	if err != nil {
		return nil, nil, err
	}
	moov, err := readMoov(f, st.Size())
	if err != nil {
		return nil, nil, errf("%s: %w", path, err)
	}
	mv, err := parseMoov(moov, st.Size(), sampleNone)
	if err != nil {
		return nil, nil, errf("%s: %w", path, err)
	}
	if !mv.fragmented {
		return nil, nil, errf("%s is not a CMAF initialisation segment (no mvex box) - pass the init segment the encoder wrote alongside the media segments", path)
	}
	boxes, err := iterBoxes(moov)
	if err != nil {
		return nil, nil, errf("%s: %w", path, err)
	}
	return mv, parseTrexDefaults(boxes), nil
}

// readCMAFSegment walks a media segment's top-level boxes and returns the span
// its moof fragments give trackID. Media bytes (mdat) are never read.
func readCMAFSegment(fs *mkv.FS, path string, trackID uint32, trex map[uint32]trexDefault) (cmafSeg, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return cmafSeg{}, err
	}
	defer f.Close()
	st, err := fs.DoStat(path)
	if err != nil {
		return cmafSeg{}, err
	}
	size := st.Size()
	seg := cmafSeg{bytes: size}
	var (
		hdr   [16]byte
		found bool
		end   int64
	)
	for off := int64(0); off+8 <= size; {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return cmafSeg{}, err
		}
		if _, err := io.ReadFull(f, hdr[:8]); err != nil {
			return cmafSeg{}, errf("read box header at offset %d: %w", off, err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(f, hdr[8:16]); err != nil {
				return cmafSeg{}, errf("read largesize at offset %d: %w", off, err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			return cmafSeg{}, errf("box %q at offset %d declares %d bytes past the end of the file (%d bytes) - the segment is truncated or is not an MP4 fragment", typ, off, boxSize, size)
		}
		if typ == "moof" {
			if boxSize-headerLen > maxFragMoofBytes {
				return cmafSeg{}, errf("moof at offset %d is %d bytes (exceeds limit)", off, boxSize-headerLen)
			}
			payload, err := readExact(f, boxSize-headerLen)
			if err != nil {
				return cmafSeg{}, err
			}
			span, ok, err := moofSpan(payload, trackID, trex)
			if err != nil {
				return cmafSeg{}, errf("moof at offset %d: %w", off, err)
			}
			switch {
			case !ok:
			case !found:
				seg.start, seg.sync, end, found = span.start, span.sync, span.end(), true
			case span.start != end:
				return cmafSeg{}, errf("the moof at offset %d starts at tick %d while the previous fragment ended at tick %d - one segment file must hold one continuous span", off, span.start, end)
			default:
				end = span.end()
			}
		}
		off += boxSize
	}
	if !found {
		return cmafSeg{}, errf("no moof box for track %d - not a media segment of this representation (an initialisation segment, or a segment of another track?)", trackID)
	}
	seg.dur = end - seg.start
	return seg, nil
}

// moofSpan returns the span the trafs of one moof give trackID. ok is false
// when the moof carries nothing for that track.
func moofSpan(moof []byte, trackID uint32, trex map[uint32]trexDefault) (span cmafSeg, ok bool, err error) {
	boxes, err := iterBoxes(moof)
	if err != nil {
		return span, false, err
	}
	for _, b := range boxes {
		if b.typ != "traf" {
			continue
		}
		s, matched, err := trafSpan(b.payload, trackID, trex)
		if err != nil {
			return span, false, err
		}
		if !matched {
			continue
		}
		if ok {
			return span, false, errf("track %d has two trafs in one moof", trackID)
		}
		span, ok = s, true
	}
	return span, ok, nil
}

// trafSpan decodes a traf's tfhd/tfdt/trun into the span it covers for
// trackID, without touching sample offsets or sizes.
func trafSpan(traf []byte, trackID uint32, trex map[uint32]trexDefault) (span cmafSeg, matched bool, err error) {
	boxes, err := iterBoxes(traf)
	if err != nil {
		return span, false, err
	}
	var (
		h        tfhd
		haveTfhd bool
	)
	for _, b := range boxes {
		if b.typ == "tfhd" {
			if h, err = parseTfhd(b.payload); err != nil {
				return span, false, err
			}
			haveTfhd = true
		}
	}
	if !haveTfhd || h.trackID != trackID {
		return span, false, nil
	}
	td := trex[trackID]
	if !h.haveDur {
		h.defDur = td.duration
	}
	if !h.haveFlags {
		h.defFlags = td.flags
	}
	var (
		dur     int64
		samples uint32
	)
	for _, b := range boxes {
		switch b.typ {
		case "tfdt":
			span.start = parseTfdt(b.payload)
		case "trun":
			d, n, firstSync, err := trunSpan(b.payload, h.defDur, h.defFlags)
			if err != nil {
				return span, false, err
			}
			if samples == 0 && n > 0 {
				span.sync = firstSync
			}
			dur += d
			samples += n
		}
	}
	if samples == 0 {
		return span, false, errf("track %d: the traf carries no samples", trackID)
	}
	if dur <= 0 {
		return span, false, errf("track %d: the fragment's samples have no duration (no trun sample_duration and no tfhd/trex default) - the manifest cannot time it", trackID)
	}
	span.dur = dur
	return span, true, nil
}

// trunSpan returns the total duration and sample count of one trun and
// whether its first sample is a sync sample.
func trunSpan(p []byte, defDur, defFlags uint32) (dur int64, count uint32, firstSync bool, err error) {
	if len(p) < 8 {
		return 0, 0, false, errf("trun too short")
	}
	flags := binary.BigEndian.Uint32(p[0:4]) & 0xFFFFFF
	count = binary.BigEndian.Uint32(p[4:8])
	off := 8
	if flags&trunDataOffset != 0 {
		off += 4
	}
	firstFlags, haveFirstFlags := uint32(0), flags&trunFirstSampleFlag != 0
	if haveFirstFlags {
		if off+4 > len(p) {
			return 0, 0, false, errf("trun truncated first_sample_flags")
		}
		firstFlags = binary.BigEndian.Uint32(p[off : off+4])
		off += 4
	}
	entry := 0
	for _, f := range []uint32{trunSampleDuration, trunSampleSize, trunSampleFlags, trunSampleCTS} {
		if flags&f != 0 {
			entry += 4
		}
	}
	if int64(off)+int64(count)*int64(entry) > int64(len(p)) {
		return 0, 0, false, errf("trun declares %d samples but holds fewer", count)
	}
	sflags := defFlags
	if flags&trunSampleFlags != 0 && count > 0 {
		at := off
		if flags&trunSampleDuration != 0 {
			at += 4
		}
		if flags&trunSampleSize != 0 {
			at += 4
		}
		sflags = binary.BigEndian.Uint32(p[at : at+4])
	}
	if haveFirstFlags {
		sflags = firstFlags
	}
	firstSync = sflags&sampleNonSync == 0
	if flags&trunSampleDuration == 0 {
		return int64(count) * int64(defDur), count, firstSync, nil
	}
	for i := uint32(0); i < count; i++ {
		at := off + int(i)*entry
		dur += int64(binary.BigEndian.Uint32(p[at : at+4]))
	}
	return dur, count, firstSync, nil
}

// validateCMAFSwitchSet checks that every rung covers the same time spans,
// segment for segment, and carries the same kinds of tracks - what a DASH
// player assumes when it switches between Representations of one
// AdaptationSet.
func validateCMAFSwitchSet(reps []*cmafRep) error {
	ref := reps[0]
	const remedy = "the rungs were not segmented on the same boundaries; encode them with the same fixed GOP and forced keyframe times, or publish each rung as its own manifest"
	for _, r := range reps[1:] {
		if a, b := trackLayout(r.tracks), trackLayout(ref.tracks); a != b {
			return errf("representation %q carries %s where %q carries %s - every rung of a switch set must hold the same kinds of tracks", r.id, a, ref.id, b)
		}
		if len(r.segs) != len(ref.segs) {
			return errf("representation %q has %d segments, %q has %d - %s", r.id, len(r.segs), ref.id, len(ref.segs), remedy)
		}
		for k := range r.segs {
			s, t := r.segs[k], ref.segs[k]
			if !sameTicks(s.start, r.timescale, t.start, ref.timescale) || !sameTicks(s.dur, r.timescale, t.dur, ref.timescale) {
				return errf("representation %q segment %d (%s) covers %s to %s but %q covers %s to %s - %s",
					r.id, k+1, r.segURLs[k], cmafSeconds(s.start, r.timescale), cmafSeconds(s.end(), r.timescale),
					ref.id, cmafSeconds(t.start, ref.timescale), cmafSeconds(t.end(), ref.timescale), remedy)
			}
		}
	}
	return nil
}

// sameTicks reports whether a/tsA and b/tsB are the same instant, exactly
// (cross-multiplied in 128 bits, no rounding).
func sameTicks(a int64, tsA uint32, b int64, tsB uint32) bool {
	if a < 0 || b < 0 {
		return a == b && tsA == tsB
	}
	h1, l1 := bits.Mul64(uint64(a), uint64(tsB))
	h2, l2 := bits.Mul64(uint64(b), uint64(tsA))
	return h1 == h2 && l1 == l2
}

// trackLayout names the kinds of tracks an init carries ("1 video + 1 audio").
func trackLayout(tracks []mkv.Track) string {
	var v, a, other int
	for i := range tracks {
		switch tracks[i].Type {
		case mkv.VideoTrack:
			v++
		case mkv.AudioTrack:
			a++
		default:
			other++
		}
	}
	parts := []string{}
	if v > 0 {
		parts = append(parts, fmt.Sprintf("%d video", v))
	}
	if a > 0 {
		parts = append(parts, fmt.Sprintf("%d audio", a))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("%d other", other))
	}
	if len(parts) == 0 {
		return "no tracks"
	}
	return strings.Join(parts, " + ")
}

// cmafSeconds formats a tick count as seconds for an error message.
func cmafSeconds(ticks int64, ts uint32) string {
	return fmt.Sprintf("%.3f s", float64(ticks)/float64(ts))
}

// cmafPeakBandwidth is peakBandwidth over tick-timed segments.
func cmafPeakBandwidth(segs []cmafSeg, ts uint32) int64 {
	var peak float64
	for _, s := range segs {
		if s.dur > 0 {
			if bps := float64(s.bytes) * 8 * float64(ts) / float64(s.dur); bps > peak {
				peak = bps
			}
		}
	}
	return int64(peak)
}

// cmafCodecs is the codecs attribute for every track of a representation,
// or "" when any track's string cannot be produced (a partial list is worse
// than none, as for the playlists).
func cmafCodecs(tracks []mkv.Track) string {
	parts := make([]string, 0, len(tracks))
	for i := range tracks {
		cs := rfc6381Codec(&outTrack{mkv: tracks[i]})
		if cs == "" {
			return ""
		}
		parts = append(parts, cs)
	}
	return strings.Join(parts, ",")
}

// numberTemplate recognises segment names that differ only by one run of
// digits counting up by one - an encoder's "seg%05d.m4s" - and returns the
// SegmentTemplate media pattern with its startNumber. ok is false for any
// other naming, and for a single segment (nothing tells which digits count),
// and the manifest then lists the segments one by one.
func numberTemplate(names []string) (media string, startNumber int64, ok bool) {
	if len(names) == 0 {
		return "", 0, false
	}
	prefix, suffix := names[0], names[0]
	for _, n := range names[1:] {
		prefix = commonPrefix(prefix, n)
		suffix = commonSuffix(suffix, n)
	}
	// The shared prefix/suffix must not eat digits that belong to the number
	// ("seg0000" of seg00001/seg00002), and must leave room for it.
	for len(prefix) > 0 && isDigit(prefix[len(prefix)-1]) {
		prefix = prefix[:len(prefix)-1]
	}
	for len(suffix) > 0 && isDigit(suffix[0]) {
		suffix = suffix[1:]
	}
	if len(prefix)+len(suffix) >= len(names[0]) {
		return "", 0, false
	}
	width, padded := 0, false
	for i, n := range names {
		if len(n) <= len(prefix)+len(suffix) {
			return "", 0, false
		}
		mid := n[len(prefix) : len(n)-len(suffix)]
		for j := 0; j < len(mid); j++ {
			if !isDigit(mid[j]) {
				return "", 0, false
			}
		}
		v, err := strconv.ParseInt(mid, 10, 64)
		if err != nil {
			return "", 0, false
		}
		if i == 0 {
			startNumber = v
			width = len(mid)
		} else if v != startNumber+int64(i) {
			return "", 0, false
		}
		if len(mid) != width {
			width = -1 // mixed widths: only an unpadded count fits $Number$
		}
		if mid[0] == '0' && len(mid) > 1 {
			padded = true
		}
	}
	esc := func(s string) string { return strings.ReplaceAll(s, "$", "$$") }
	switch {
	case width > 0:
		if !padded && width == 1 {
			return esc(prefix) + "$Number$" + esc(suffix), startNumber, true
		}
		return esc(prefix) + fmt.Sprintf("$Number%%0%dd$", width) + esc(suffix), startNumber, true
	case !padded:
		return esc(prefix) + "$Number$" + esc(suffix), startNumber, true
	}
	return "", 0, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

func commonSuffix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return a[len(a)-n:]
}

// renderCMAFManifest writes the MPD: the video switch set, then one
// AdaptationSet per audio representation, each Representation addressed by
// a SegmentTemplate when its names count up, else a SegmentList.
func renderCMAFManifest(o *Options, video, audio []*cmafRep) []byte {
	rw := urlRewriter(o)
	all := append(append([]*cmafRep{}, video...), audio...)
	var totalSec, maxSegSec float64
	templated := true
	for _, r := range all {
		ts := float64(r.timescale)
		if span := float64(r.segs[len(r.segs)-1].end()-r.segs[0].start) / ts; span > totalSec {
			totalSec = span
		}
		for _, s := range r.segs {
			if d := float64(s.dur) / ts; d > maxSegSec {
				maxSegSec = d
			}
		}
		if _, _, ok := numberTemplate(r.segURLs); !ok {
			templated = false
		}
	}
	profile := "urn:mpeg:dash:profile:isoff-live:2011"
	if !templated {
		profile = "urn:mpeg:dash:profile:isoff-main:2011"
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="%s" mediaPresentationDuration="%s" minBufferTime="%s">`+"\n",
		profile, dashDuration(totalSec), dashDuration(maxSegSec))
	b.WriteString("  <Period>\n")

	// Video: one AdaptationSet, the validated switch set.
	as := `mimeType="video/mp4" contentType="video" segmentAlignment="true"`
	if cmafAllSync(video) {
		as += ` startWithSAP="1"`
	}
	fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
	for _, r := range video {
		t := &r.tracks[r.primary]
		rep := fmt.Sprintf(`id=%q bandwidth="%d"`, r.id, r.bandwidth)
		if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
			rep += fmt.Sprintf(` width="%d" height="%d"`, *t.Width, *t.Height)
		}
		if t.FrameRate != nil && *t.FrameRate > 0 {
			rep += fmt.Sprintf(` frameRate="%s"`, dashFrameRate(*t.FrameRate))
		}
		if codecs := cmafCodecs(r.tracks); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
		writeCMAFAddressing(&b, rw, r)
		b.WriteString("      </Representation>\n")
	}
	b.WriteString("    </AdaptationSet>\n")

	// Audio: one AdaptationSet per representation.
	for _, r := range audio {
		t := &r.tracks[r.primary]
		as := `mimeType="audio/mp4" contentType="audio"` + dashLangAttr(t)
		rep := fmt.Sprintf(`id=%q bandwidth="%d"`, r.id, r.bandwidth)
		if t.SampleRate != nil && *t.SampleRate > 0 {
			rep += fmt.Sprintf(` audioSamplingRate="%d"`, int64(*t.SampleRate))
		}
		if codecs := cmafCodecs(r.tracks); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
		b.WriteString(dashAudioChannelConfiguration(t, "        "))
		writeCMAFAddressing(&b, rw, r)
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}

	b.WriteString("  </Period>\n</MPD>\n")
	return []byte(b.String())
}

// cmafAllSync reports whether every segment of every rung opens on a sync
// sample (startWithSAP="1").
func cmafAllSync(reps []*cmafRep) bool {
	for _, r := range reps {
		for _, s := range r.segs {
			if !s.sync {
				return false
			}
		}
	}
	return true
}

// writeCMAFAddressing writes the Representation's segment addressing and
// timeline, in its track's native timescale.
func writeCMAFAddressing(b *strings.Builder, rw func(string) string, r *cmafRep) {
	starts := make([]int64, len(r.segs))
	durs := make([]int64, len(r.segs))
	for i, s := range r.segs {
		starts[i], durs[i] = s.start, s.dur
	}
	timeline := dashTimelineSpans(starts, durs)
	if media, start, ok := numberTemplate(r.segURLs); ok {
		fmt.Fprintf(b, `        <SegmentTemplate initialization="%s" media="%s" startNumber="%d" timescale="%d">`+"\n",
			rw(r.initURL), rw(media), start, r.timescale)
		b.WriteString(timeline)
		b.WriteString("        </SegmentTemplate>\n")
		return
	}
	fmt.Fprintf(b, `        <SegmentList timescale="%d">`+"\n", r.timescale)
	fmt.Fprintf(b, `          <Initialization sourceURL="%s"/>`+"\n", rw(r.initURL))
	b.WriteString(timeline)
	for _, u := range r.segURLs {
		fmt.Fprintf(b, `          <SegmentURL media="%s"/>`+"\n", rw(u))
	}
	b.WriteString("        </SegmentList>\n")
}
