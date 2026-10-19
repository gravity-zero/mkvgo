package mp4

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/internal/lang"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Join concatenates MP4/MOV files with the same streams (tracks, timescales, sample descriptions), each from the previous one's end.
func Join(ctx context.Context, sources []string, dstPath string, opts ...Options) error {
	o := optionsFrom(opts)
	if len(sources) < 2 {
		return errf("join: at least two files are needed")
	}
	if err := distinctOutput(o.FS, dstPath, sources...); err != nil {
		return err
	}
	var vs vsource
	budget := int64(maxSamples)
	var held int64
	models := make([]*cutModel, len(sources))
	for i, p := range sources {
		f, err := o.FS.DoOpen(p)
		if err != nil {
			return err
		}
		defer f.Close()
		size, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		var base int64
		vs, base = vs.withFile(f, size)
		if models[i], err = loadCutModel(f, p, size, base, &budget, o); err != nil {
			return errf("join: %s: %w", shown.Path(p), err)
		}
		if held += models[i].held; held > o.moovLimit() {
			return errf("join: the files' movie headers total more than %d bytes: join fewer files at once, or raise Options.MaxMoovBytes", o.moovLimit())
		}
		if i > 0 {
			if err := sameStreams(models[0], models[i]); err != nil {
				return errf("join: %s: %w", shown.Path(p), err)
			}
		}
	}
	first := models[0]
	offsets := make([]int64, len(models)) // where each file starts on the output timeline (ns)
	for i := 1; i < len(models); i++ {
		offsets[i] = offsets[i-1] + models[i-1].movieNs
		if models[i].link.continues(models[i-1].link) {
			offsets[i] = offsets[i-1] + models[i-1].cutNs()
		}
	}
	totalNs := offsets[len(models)-1] + models[len(models)-1].movieNs

	outTS := first.outTS
	traks := make([][]byte, 0, len(first.tracks))
	var movieDur int64
	for ti := range first.tracks {
		tt, lead, err := joinTables(models, offsets, ti, &vs)
		if err != nil {
			return errf("join: track %d: %w", ti+1, err)
		}
		c := &first.tracks[ti]
		e := c.edit
		e.empty = rescale(e.empty, first.movieTS, outTS) + rescale(lead, c.ts, outTS)
		segDur := rescale(max(tt.maxEnd()-e.mediaTime, 0), c.ts, outTS)
		last := &models[len(models)-1].tracks[ti]
		endNs := offsets[len(models)-1] + last.presentEndNs(models[len(models)-1].movieTS)
		if lim := rescale(min(endNs, totalNs), nsPerSec, outTS) - e.empty; segDur > lim {
			segDur = max(lim, 0)
		}
		var edts []byte
		if len(tt.samples) > 0 {
			edts = buildEdit(e.empty, e.mediaTime, segDur)
		}
		t := c.trak
		if !sameSgpds(models, ti) {
			tt.groups, tt.gparam = nil, nil
			t, err = dropSgpds(t)
			if err != nil {
				return err
			}
		}
		if t, err = rebuildTrak(t, tt, edts, e.empty+segDur); err != nil {
			return errf("join: track %d: %w", ti+1, err)
		}
		traks = append(traks, t)
		movieDur = max(movieDur, e.empty+segDur)
	}
	payload, err := first.assemble(traks, movieDur, outTS)
	if err != nil {
		return err
	}
	var chs []mkv.Chapter
	for i, m := range models {
		for j, ch := range flattenChapters(m.meta.Chapters) {
			ch.StartMs += roundMs(offsets[i])
			if ch.EndMs > 0 {
				ch.EndMs += roundMs(offsets[i])
			}
			// A chapter a split cut in two comes back whole: the next part of that split, same title, running into the seam (or to the previous file's end as read, which truncates where the seam rounds).
			if n := len(chs); n > 0 && j == 0 && m.link.continues(models[i-1].link) && ch.StartMs == roundMs(offsets[i]) && chs[n-1].Title == ch.Title &&
				(chs[n-1].EndMs == 0 || chs[n-1].EndMs == ch.StartMs || chs[n-1].EndMs == roundMs(offsets[i-1])+models[i-1].meta.DurationMs) {
				chs[n-1].EndMs = ch.EndMs
				continue
			}
			ch.ID = uint64(len(chs) + 1)
			chs = append(chs, ch)
		}
	}
	r0 := vs[0].r
	if payload, vs, err = withChapters(r0, payload, vs, tidyChapters(chs, 0)); err != nil {
		return errf("join: %w", err)
	}
	// The output is no part of a split: the first part's link would chain it to files it does not follow.
	if payload, err = withSplitLink(payload, splitLink{}); err != nil {
		return errf("join: %w", err)
	}
	ftyp, err := sourceFtyp(r0, vs[0].size)
	if err != nil {
		return err
	}
	top, err := sourceTopBoxes(r0, vs[0].size)
	if err != nil {
		return err
	}
	return writeRelocated(ctx, vs, payload, nil, outLayout{ftyp: ftyp, top: top}, dstPath, o)
}

// sameStreams refuses a file whose tracks a join could not continue from the first file's.
func sameStreams(a, b *cutModel) error {
	if len(a.tracks) != len(b.tracks) {
		return fmt.Errorf("it has %d tracks, the first file %d: a join needs the same tracks in every file", len(b.tracks), len(a.tracks))
	}
	for i := range a.tracks {
		x, y := &a.tracks[i], &b.tracks[i]
		if x.handler != y.handler {
			return fmt.Errorf("track %d is %q here and %q in the first file: put the tracks in the same order first (mux)", i+1, y.handler, x.handler)
		}
		if x.ts != y.ts {
			return fmt.Errorf("track %d runs on a %d Hz timescale here and %d Hz in the first file, which a sample copy cannot reconcile: join them as MKV (from-mp4, join, to-mp4)", i+1, y.ts, x.ts)
		}
		sx, err := boxAt(x.trak, "mdia", "minf", "stbl", "stsd")
		if err != nil {
			return err
		}
		sy, err := boxAt(y.trak, "mdia", "minf", "stbl", "stsd")
		if err != nil {
			return err
		}
		if !bytes.Equal(decodingDescription(sx, x.handler), decodingDescription(sy, y.handler)) {
			return fmt.Errorf("track %d is encoded differently from the first file's (its sample description differs): a join copies samples, so every file needs the same encoding settings", i+1)
		}
		if lx, ly := trakLanguage(x.trak), trakLanguage(y.trak); lx != "und" && ly != "und" && lx != ly {
			return fmt.Errorf("track %d is in %s here and %s in the first file: the join pairs tracks by position, so put them in the same order first (mux), or correct a wrong language (edit-track -lang)", i+1, ly, lx)
		}
		if x.handler == "vide" && !bytes.Equal(tkhdDisplay(x.trak), tkhdDisplay(y.trak)) {
			return fmt.Errorf("track %d is turned or sized for display differently from the first file's (its tkhd matrix or size differs), which the join could state only one way: join files that display the same way", i+1)
		}
	}
	return nil
}

// trakLanguage is the language a trak's mdhd states, as its ISO 639-2 bibliographic code ("und" when it states none).
func trakLanguage(trak []byte) string {
	p, err := boxAt(trak, "mdia", "mdhd")
	if err != nil {
		return "und"
	}
	_, l := parseMdhd(p)
	return lang.Primary(l)
}

// tkhdDisplay is a trak's display matrix and size as its tkhd states them (nil when it has none).
func tkhdDisplay(trak []byte) []byte {
	p, err := boxAt(trak, "tkhd")
	if err != nil || len(p) < 1 {
		return nil
	}
	off := 40
	if p[0] == 1 {
		off = 52
	}
	if len(p) < off+44 {
		return nil
	}
	return p[off : off+44]
}

// maxAudioGapMs is the longest audio gap a join hides by stretching the last sample; players drift past it.
const maxAudioGapMs = 500

// joinTables lays track ti of every file on the output timeline; lead is how late it starts when the first files lack it.
func joinTables(models []*cutModel, offsets []int64, ti int, vs *vsource) (out *trakTable, lead int64, err error) {
	first := &models[0].tracks[ti]
	out = &trakTable{}
	withSdtp := true
	for _, m := range models {
		withSdtp = withSdtp && m.tracks[ti].tt.sdtp != nil
	}
	groupTypes := map[string]bool{}
	for typ := range first.tt.groups {
		groupTypes[typ] = true
	}
	for _, m := range models[1:] {
		for typ := range groupTypes {
			if _, ok := m.tracks[ti].tt.groups[typ]; !ok {
				delete(groupTypes, typ)
			}
		}
	}
	if len(groupTypes) > 0 {
		out.groups, out.gparam = map[string][]uint32{}, map[string]*uint32{}
		for typ := range groupTypes {
			out.gparam[typ] = first.tt.gparam[typ]
		}
	}
	filler := textFiller(first)
	fillerAt := int64(-1)
	var end int64
	for k, m := range models {
		c := &m.tracks[ti]
		tt := c.tt
		from, to := 0, len(tt.samples)
		allSync := true
		for _, x := range tt.samples {
			allSync = allSync && x.sync
		}
		// Pictures presented ahead of the first keyframe (open-GOP leading pictures) may be hidden; frames after it may not.
		if key := firstSyncCts(tt); k > 0 && !allSync && c.edit.mediaTime > key {
			return nil, 0, fmt.Errorf("file %d hides the first %d ms of this track with its edit list, frames a join would have to show or orphan: remux the files to MKV (from-mp4) and join there", k+1, rescale(c.edit.mediaTime-key, c.ts, 1000))
		}
		if k > 0 && allSync {
			// Samples wholly ahead of the file's media start (encoder priming) would play twice over the seam.
			for from < to && tt.samples[from].sync && tt.samples[from].dts+int64(tt.samples[from].cto)+int64(tt.samples[from].dur) <= c.edit.mediaTime {
				from++
			}
		}
		if k < len(models)-1 && !allSync && c.edit.segDur >= 0 && len(tt.samples) > 0 {
			last := tt.samples[len(tt.samples)-1]
			mediaEnd := rescale(c.edit.empty, m.movieTS, nsPerSec) + rescale(tt.maxEnd()-c.edit.mediaTime, c.ts, nsPerSec)
			if hidden := mediaEnd - c.presentEndNs(m.movieTS); hidden > rescale(int64(last.dur), c.ts, nsPerSec) {
				return nil, 0, fmt.Errorf("file %d's edit list hides the last %d ms of this track, frames a join would have to show: remux the files to MKV (from-mp4) and join there", k+1, hidden/1_000_000)
			}
		}
		if k < len(models)-1 && allSync && c.edit.segDur >= 0 {
			// Samples past the file's presentation end (encoder padding its edit list hides) would play over the seam.
			endNs := c.presentEndNs(m.movieTS)
			for to > from && c.ptNs(to-1, m.movieTS) >= endNs {
				to--
			}
		}
		shift := int64(0)
		if k > 0 {
			shift = rescale(offsets[k]+rescale(c.edit.empty, m.movieTS, nsPerSec), nsPerSec, first.ts) -
				rescale(first.edit.empty, models[0].movieTS, first.ts) + first.edit.mediaTime - c.edit.mediaTime
		}
		if n := len(out.samples); n > 0 && from < to && c.isText() && m.link.continues(models[k-1].link) {
			// The cue a split carried over the cut opens this part again: it is the previous part's last sample, kept once.
			if last, s := out.samples[n-1], tt.samples[from]; shift+s.dts == last.dts && s.size == last.size {
				from++
			}
		}
		if n := len(out.samples); n > 0 && from < to {
			last := &out.samples[n-1]
			firstDts := tt.samples[from].dts
			switch gap := shift + firstDts - end; {
			case gap > 0 && filler != nil:
				if fillerAt < 0 {
					fillerAt = vs.limit()
					*vs = vs.withMem(filler)
				}
				if gap > math.MaxUint32 {
					return nil, 0, fmt.Errorf("file %d starts %d ticks after the previous one ends, a gap one sample cannot span: join files that follow each other", k+1, gap)
				}
				out.samples = append(out.samples, rwSample{src: fillerAt, dts: end, size: uint32(len(filler)), dur: uint32(gap), sdi: last.sdi, sync: true})
				if withSdtp {
					out.sdtp = append(out.sdtp, 0)
				}
				for typ := range groupTypes {
					out.groups[typ] = append(out.groups[typ], 0)
				}
			case gap > 0 && first.handler == "soun" && rescale(gap, first.ts, 1000) > maxAudioGapMs:
				return nil, 0, fmt.Errorf("file %d's audio starts %d ms after the previous file's ends, more than the %d ms a join fills by holding the last sample: remux the files to MKV (from-mp4) and join there", k+1, rescale(gap, first.ts, 1000), maxAudioGapMs)
			case gap > 0:
				if int64(last.dur)+gap > math.MaxUint32 {
					return nil, 0, fmt.Errorf("file %d starts %d ticks after the previous one ends, a gap one sample cannot span: join files that follow each other", k+1, gap)
				}
				last.dur += uint32(gap)
			case gap < 0 && int64(last.dur)+gap > 0:
				// The previous file's last sample runs past this file's start: it ends there, the timeline does not drift.
				last.dur = uint32(int64(last.dur) + gap)
			case gap < 0 && !allSync:
				// This file's pictures are reordered deeper than the previous one's (an open GOP's leading pictures): its decoding would start inside it.
				return nil, 0, fmt.Errorf("file %d starts decoding %d ms before file %d ends, its first pictures reordered further ahead than that file's: remux the files to MKV (from-mp4) and join there", k+1, rescale(-gap, c.ts, 1000), k)
			case gap < 0:
				// This file's first sample starts before the previous one's last: the samples it covers give way, its own times stay.
				start := shift + firstDts
				for n > 0 && out.samples[n-1].dts >= start {
					n--
				}
				out.samples = out.samples[:n]
				if withSdtp {
					out.sdtp = out.sdtp[:n]
				}
				for typ := range groupTypes {
					out.groups[typ] = out.groups[typ][:n]
				}
				if n > 0 && out.samples[n-1].dts+int64(out.samples[n-1].dur) > start {
					out.samples[n-1].dur = uint32(start - out.samples[n-1].dts)
				}
			}
		}
		// The leading pictures a file's edit list hides reference pictures the file before it never held: they go, their time to the picture before.
		dropLeading, keyed := k > 0 && !allSync && !m.link.continues(models[k-1].link), false
		for i := from; i < to; i++ {
			s := tt.samples[i]
			if dropLeading && keyed && !s.sync && s.dts+int64(s.cto) < c.edit.mediaTime {
				out.samples[len(out.samples)-1].dur += s.dur
				continue
			}
			keyed = keyed || s.sync
			s.dts += shift
			out.samples = append(out.samples, s)
			if withSdtp {
				out.sdtp = append(out.sdtp, tt.sdtp[i])
			}
			for typ := range groupTypes {
				out.groups[typ] = append(out.groups[typ], tt.groups[typ][i])
			}
		}
		if n := len(out.samples); n > 0 {
			end = out.samples[n-1].dts + int64(out.samples[n-1].dur)
		}
	}
	if len(out.samples) > 0 {
		lead = out.samples[0].dts
		for i := range out.samples {
			out.samples[i].dts -= lead
		}
	}
	return out, lead, nil
}

// decodingDescription is an stsd as a decoder reads it: the bitrates muxers declare per file (btrt, the esds rates) left out.
func decodingDescription(stsd []byte, handler string) []byte {
	if len(stsd) < 8 {
		return stsd
	}
	entries, err := iterBoxes(stsd[8:])
	if err != nil {
		return stsd
	}
	var w bw
	w.bytes(stsd[:8])
	for _, e := range entries {
		hdr := len(e.payload)
		switch handler {
		case "vide":
			hdr = 78
		case "soun":
			hdr = audioExtOffset(e.payload)
		}
		kids, err := iterBoxes(e.payload[min(hdr, len(e.payload)):])
		if hdr >= len(e.payload) || err != nil {
			w.bytes(box(e.typ, e.payload))
			continue
		}
		w.bytes([]byte(e.typ))
		w.bytes(e.payload[:hdr])
		decodingKids(&w, kids, true)
	}
	return w.b
}

// decodingKids writes a sample entry's child boxes without their declared bitrates, inside its QuickTime wave too (one level, as QuickTime writes it).
func decodingKids(w *bw, kids []memBox, walkWave bool) {
	for _, k := range kids {
		switch k.typ {
		case "btrt":
		case "esds":
			if obj, asc, _, err := parseESDS(k.payload); err == nil {
				w.bytes([]byte{obj})
				w.bytes(asc)
				continue
			}
			w.bytes(box(k.typ, k.payload))
		case "wave":
			if inner, err := iterBoxes(k.payload); err == nil && walkWave {
				w.bytes([]byte(k.typ))
				decodingKids(w, inner, false)
				continue
			}
			w.bytes(box(k.typ, k.payload))
		default:
			w.bytes(box(k.typ, k.payload))
		}
	}
}

// firstSyncCts is the composition time of a table's first sync sample in decode order.
func firstSyncCts(tt *trakTable) int64 {
	for _, s := range tt.samples {
		if s.sync {
			return s.dts + int64(s.cto)
		}
	}
	return 0
}

// textFiller is the empty sample that keeps a subtitle track blank across a gap (nil: the track is not timed text).
func textFiller(c *cutTrack) []byte {
	stsd, err := boxAt(c.trak, "mdia", "minf", "stbl", "stsd")
	if err != nil || len(stsd) < 16 {
		return nil
	}
	switch string(stsd[12:16]) {
	case "tx3g", "text":
		return []byte{0, 0}
	case "wvtt":
		return box("vtte", nil)
	}
	return nil
}

// sameSgpds reports whether every file carries byte-identical sample group descriptions on track ti.
func sameSgpds(models []*cutModel, ti int) bool {
	ref, err := sgpdBytes(models[0].tracks[ti].trak)
	if err != nil {
		return false
	}
	for _, m := range models[1:] {
		b, err := sgpdBytes(m.tracks[ti].trak)
		if err != nil || !bytes.Equal(ref, b) {
			return false
		}
	}
	return true
}

func sgpdBytes(trak []byte) ([]byte, error) {
	stbl, err := boxAt(trak, "mdia", "minf", "stbl")
	if err != nil {
		return nil, err
	}
	sb, err := iterBoxes(stbl)
	if err != nil {
		return nil, err
	}
	var w bw
	for _, b := range sb {
		if b.typ == "sgpd" {
			w.bytes(box("sgpd", b.payload))
		}
	}
	return w.b, nil
}

// dropSgpds removes a trak's sample group descriptions.
func dropSgpds(trak []byte) ([]byte, error) {
	return rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
		var out [][]byte
		for _, b := range sb {
			if b.typ != "sgpd" {
				out = append(out, box(b.typ, b.payload))
			}
		}
		return out, nil
	})
}

// cutNs is where a split part's cut falls: as its link states it, else as its tracks show it.
func (m *cutModel) cutNs() int64 {
	if m.link.cutNs > 0 {
		return m.link.cutNs
	}
	var end int64
	other := false
	for i := range m.tracks {
		if c := &m.tracks[i]; c.otherVideo(i, m.ref) {
			other = true
		} else {
			end = max(end, c.presentEndNs(m.movieTS))
		}
	}
	if !other || end == 0 {
		return m.movieNs
	}
	return end
}
