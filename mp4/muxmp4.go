package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"

	"github.com/gravity-zero/mkvgo/internal/lang"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// muxInput is one output track: a probe track of an MP4 source, or a built trak whose samples sit in mem.
type muxInput struct {
	path  string
	id    uint64
	trak  []byte // built trak (movie timescale: milliseconds); nil for a source track
	mem   []byte
	apply func(t *mkv.Track) // per-track metadata override; nil keeps the source's
}

// muxSource is one opened MP4 source.
type muxSource struct {
	r       io.ReadSeeker
	size    int64
	base    int64
	payload []byte
	mv      *movie
	movieTS uint32
	traks   [][]byte
	qt      bool // a QuickTime movie (major brand "qt  ", or no ftyp)
	mdats   [][2]int64
}

// Mux builds an MP4 from tracks of MP4/MOV sources, samples copied untouched; opts set the metadata, as matroska.Mux.
func Mux(ctx context.Context, opts mkv.MuxOptions, extra ...Options) error {
	if len(opts.Attachments) > 0 {
		return errf("mux: an MP4 carries no attachments; mux to MKV to keep them")
	}
	if len(opts.Tracks) == 0 {
		return errf("mux: no tracks given")
	}
	var inputs []muxInput
	for _, ti := range opts.Tracks {
		inputs = append(inputs, muxInput{path: ti.SourcePath, id: ti.TrackID, apply: trackInputOverride(ti)})
	}
	return muxMP4(ctx, inputs, opts.OutputPath, func(c *mkv.Container) {
		c.Info.Title, c.Tags, c.Chapters = opts.Title, opts.Tags, opts.Chapters
	}, optionsFrom(extra))
}

func trackInputOverride(ti mkv.TrackInput) func(*mkv.Track) {
	return func(t *mkv.Track) {
		if ti.Language != "" {
			t.Language, t.LanguageBCP47 = LanguageFields(ti.Language)
		}
		if ti.Name != "" {
			t.Name = ti.Name
		}
		t.IsDefault = ti.IsDefault
	}
}

// readSRT parses an SRT file through fs, refusing one past maxSRTBytes rather than holding it.
func readSRT(fs *mkv.FS, path string) ([]subtitle.SRTEntry, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: maxSRTBytes + 1}
	entries, err := subtitle.ParseSRTReader(lr)
	if lr.N == 0 {
		return nil, fmt.Errorf("the SRT file passes the %d MiB a subtitle track takes: split it into several files", maxSRTBytes>>20)
	}
	if err != nil {
		return nil, fmt.Errorf("parse SRT: %w", err)
	}
	return entries, nil
}

// maxSRTBytes bounds the SRT file merge-subtitle reads whole.
const maxSRTBytes = 64 << 20

// LanguageFields splits a language as a user gives it into the mdhd's 3-letter code (ISO 639-2/T for a tag's 2-letter language) and, for a BCP-47 tag, the elng tag.
func LanguageFields(tag string) (language, bcp47 string) {
	return lang.Fields(tag, lang.Terminology)
}

// Merge combines the selected tracks of MP4/MOV inputs (all of an input's when it names none); title, tags and chapters come from the first input.
func Merge(ctx context.Context, opts mkv.MergeOptions, extra ...Options) error {
	if len(opts.Inputs) == 0 {
		return errf("merge: no inputs")
	}
	o := optionsFrom(extra)
	if o.Progress == nil {
		o.Progress = opts.Progress
	}
	var inputs []muxInput
	for _, in := range opts.Inputs {
		ids := in.TrackIDs
		if len(ids) == 0 {
			c, dropped, err := OpenMeta(ctx, in.SourcePath, o)
			if err != nil {
				return errf("merge: %s: %w", shown.Path(in.SourcePath), err)
			}
			reportDropped(o, dropped)
			for _, t := range c.Tracks {
				ids = append(ids, t.ID)
			}
		}
		for _, id := range ids {
			inputs = append(inputs, muxInput{path: in.SourcePath, id: id})
		}
	}
	return muxMP4(ctx, inputs, opts.OutputPath, nil, o)
}

// AddTrack writes srcPath to dstPath with one track of an MP4/MOV source appended (Language and Name override when set, IsDefault always).
func AddTrack(ctx context.Context, srcPath, dstPath string, input mkv.TrackInput, opts ...Options) error {
	o := optionsFrom(opts)
	inputs, err := allTracks(ctx, srcPath, o)
	if err != nil {
		return errf("add-track: %w", err)
	}
	inputs = append(inputs, muxInput{path: input.SourcePath, id: input.TrackID, apply: trackInputOverride(input)})
	return muxMP4(ctx, inputs, dstPath, nil, o)
}

// MergeSubtitle writes srcPath to dstPath with an SRT file added as a timed-text (tx3g) subtitle track.
func MergeSubtitle(ctx context.Context, srcPath, srtPath, dstPath string, lang, name string, opts ...Options) error {
	o := optionsFrom(opts)
	// The SRT reaches the mux as memory, out of sight of its own output check.
	if err := distinctOutput(o.FS, dstPath, srtPath); err != nil {
		return err
	}
	entries, err := readSRT(o.FS, srtPath)
	if err != nil {
		return errf("merge-subtitle: %w", err)
	}
	if len(entries) == 0 {
		return errf("merge-subtitle: the SRT file holds no cue")
	}
	if _, bcp := LanguageFields(lang); bcp != "" && !validBCP47(bcp) {
		return errf("merge-subtitle: %q is not a BCP-47 language tag (letters and digits in subtags of 1 to 8, joined by \"-\", e.g. \"pt-BR\")", lang)
	}
	trak, data, err := buildSRTTrak(entries, lang, name)
	if err != nil {
		return errf("merge-subtitle: %w", err)
	}
	inputs, err := allTracks(ctx, srcPath, o)
	if err != nil {
		return errf("merge-subtitle: %w", err)
	}
	inputs = append(inputs, muxInput{trak: trak, mem: data})
	return muxMP4(ctx, inputs, dstPath, nil, o)
}

// reportDropped tells OnDrop about the traks a mux leaves out (timecode, chapter images...).
func reportDropped(o Options, dropped []DroppedTrack) {
	for _, d := range dropped {
		d.Reason += " (not carried)"
		o.report(d)
	}
}

// allTracks lists every probe track of an MP4 source as mux inputs.
func allTracks(ctx context.Context, path string, o Options) ([]muxInput, error) {
	c, dropped, err := OpenMeta(ctx, path, o)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shown.Path(path), err)
	}
	reportDropped(o, dropped)
	var out []muxInput
	for _, t := range c.Tracks {
		out = append(out, muxInput{path: path, id: t.ID})
	}
	return out, nil
}

// buildSRTTrak builds a tx3g subtitle trak from SRT cues; its chunk offsets start at 0 in the returned data.
func buildSRTTrak(entries []subtitle.SRTEntry, lang, name string) ([]byte, []byte, error) {
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].StartMs < entries[b].StartMs })
	spec, _ := subtitleCarriage("srt", false, false)
	t := &outTrack{mkv: mkv.Track{ID: 1, Type: mkv.SubtitleTrack, Codec: "srt", Name: name}, spec: spec, mp4ID: 1}
	if lang != "" {
		t.mkv.Language, t.mkv.LanguageBCP47 = LanguageFields(lang)
	}
	entry, err := spec.sampleEntry(&t.mkv, nil)
	if err != nil {
		return nil, nil, err
	}
	t.sampleEntry = entry
	var buf bytes.Buffer
	cw := &countWriter{w: &buf}
	for _, e := range entries {
		var dur int64
		if e.EndMs > e.StartMs {
			dur = e.EndMs - e.StartMs
		}
		if err := t.addTextBlock(cw, mkv.Block{Timecode: e.StartMs, Duration: dur, Data: []byte(e.Text)}); err != nil {
			return nil, nil, err
		}
	}
	if t.hasPendingCue {
		if err := t.flushPendingCue(cw, -1); err != nil {
			return nil, nil, err
		}
	}
	if err := flushChunk(cw, t); err != nil {
		return nil, nil, err
	}
	trak, _ := buildTrak(t, 0, true)
	return trak[8:], buf.Bytes(), nil
}

// muxMP4 writes the inputs' tracks in order to one fast-start MP4; meta, when set, rewrites the first source's metadata.
func muxMP4(ctx context.Context, inputs []muxInput, dstPath string, meta func(c *mkv.Container), o Options) error {
	var vs vsource
	sources := map[string]*muxSource{}
	var first *muxSource
	var held int64
	for _, in := range inputs {
		if in.trak != nil || sources[in.path] != nil {
			continue
		}
		f, err := o.FS.DoOpen(in.path)
		if err != nil {
			return err
		}
		defer f.Close()
		s, err := openMuxSource(f, o)
		if err != nil {
			return errf("%s: %w", shown.Path(in.path), err)
		}
		if held += int64(len(s.payload)); held > o.moovLimit() {
			return errf("the inputs' movie headers total more than %d bytes: mux fewer files at once, or raise Options.MaxMoovBytes", o.moovLimit())
		}
		vs, s.base = vs.withFile(f, s.size)
		sources[in.path] = s
		if first == nil {
			first = s
		}
	}
	if first == nil {
		return errf("no MP4 source among the inputs")
	}
	if err := distinctOutput(o.FS, dstPath, slices.Collect(maps.Keys(sources))...); err != nil {
		return err
	}
	outTS := first.movieTS // every source's movie times stay exact on a common multiple, taken in input order
	for _, in := range inputs {
		if s := sources[in.path]; s != nil {
			outTS = commonTimescale(outTS, s.movieTS)
		}
	}
	var traks [][]byte
	var movieDur int64
	nextID := uint32(1)
	for i, in := range inputs {
		var trak []byte
		var err error
		if in.trak != nil {
			base := vs.limit()
			vs = vs.withMem(in.mem)
			trak, err = shiftChunkOffsets(in.trak, base)
			if err == nil {
				trak, err = rescaleMovieTime(trak, movieTimescale, outTS)
			}
			if err == nil && first.qt {
				trak, err = hdlrForm(trak, true)
			}
		} else {
			src := sources[in.path]
			trak, err = src.trak(in.id, outTS)
			if err == nil && src.qt != first.qt {
				trak, err = hdlrForm(trak, first.qt)
			}
			if err == nil && src.qt && !first.qt {
				trak, err = isoForm(trak)
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", shown.Path(in.path), err)
			}
		}
		if err != nil {
			return errf("track %d of the output: %w", i+1, err)
		}
		if trak, err = setTrakSlot(trak, nextID); err != nil {
			return err
		}
		nextID++
		if d, err := trakDuration(trak); err == nil {
			movieDur = max(movieDur, d)
		}
		traks = append(traks, trak)
	}
	payload, err := assembleMux(first, traks, movieDur, nextID, outTS)
	if err != nil {
		return err
	}
	limit := vs.limit()
	before, mv, err := moovContainer(first.r, payload, limit)
	if err != nil {
		return err
	}
	after, _, err := moovContainer(first.r, payload, limit)
	if err != nil {
		return err
	}
	for i, in := range inputs {
		if in.apply != nil && i < len(after.Tracks) {
			in.apply(&after.Tracks[i])
		}
	}
	if meta != nil {
		meta(after)
	} else if src, _, err := moovContainer(first.r, first.payload, first.size); err == nil {
		// Read from the first source's own moov (its chapter track is not carried), made storable rather than refused.
		after.Chapters = tidyChapters(src.Chapters, after.DurationMs)
		before.Chapters = after.Chapters
	}
	if err := checkEditable(before, after); err != nil {
		return errf("%w", err)
	}
	if len(after.Chapters) > 0 {
		before.Chapters = nil // the sources' chapter tracks are not carried: rebuild one for the output
	}
	moov, chap, err := patchMoovMeta(payload, mv, before, after, limit)
	if err != nil {
		return err
	}
	mb, err := iterBoxes(moov)
	if err != nil || len(mb) != 1 {
		return errf("rebuilt moov does not parse")
	}
	ftyp, err := sourceFtyp(first.r, first.size)
	if err != nil {
		return err
	}
	top, err := sourceTopBoxes(first.r, first.size)
	if err != nil {
		return err
	}
	return writeRelocated(ctx, vs.withMem(chap), mb[0].payload, nil, outLayout{ftyp: ftyp, top: top, interleave: true}, dstPath, o)
}

func openMuxSource(r io.ReadSeeker, o Options) (*muxSource, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	payload, err := readMoovLimit(r, size, o.moovLimit())
	if err != nil {
		return nil, err
	}
	mv, err := parseMoov(payload, size, sampleNone)
	if err != nil {
		return nil, err
	}
	if mv.fragmented {
		return nil, fmt.Errorf("a fragmented MP4 cannot be muxed sample by sample; remux it to a progressive MP4 first (from-mp4, then to-mp4)")
	}
	s := &muxSource{r: r, size: size, payload: payload, mv: mv}
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, err
	}
	for _, b := range boxes {
		switch b.typ {
		case "trak":
			s.traks = append(s.traks, b.payload)
		case "mvhd":
			s.movieTS, _ = parseMovieHeader(b.payload)
		}
	}
	if s.movieTS == 0 {
		return nil, fmt.Errorf("mvhd declares no timescale")
	}
	ftyp, err := sourceFtyp(r, size)
	if err != nil {
		return nil, err
	}
	s.qt = len(ftyp) < 12 || string(ftyp[8:12]) == "qt  "
	s.mdats = sourceMdats(r, size)
	return s, nil
}

// trak returns probe track id's trak ready for the output: chunk offsets in the virtual space, references dropped, movie times in movieTS.
func (s *muxSource) trak(id uint64, movieTS uint32) ([]byte, error) {
	if id == 0 || id > uint64(len(s.mv.tracks)) {
		return nil, fmt.Errorf("track %d not found (the file has %d)", id, len(s.mv.tracks))
	}
	trak := s.traks[s.mv.tracks[id-1].trakIndex]
	if err := relocatable(trak); err != nil {
		return nil, fmt.Errorf("track %d: %w", id, err)
	}
	trak, err := dropTref(trak)
	if err != nil {
		return nil, err
	}
	// Checked against this source alone: in the shared space a chunk past its end would read the next source's bytes.
	chunks, err := trakChunks(trak, s.size)
	if err == nil {
		err = checkInMdat(chunks, s.mdats, s.size)
	}
	if err != nil {
		return nil, fmt.Errorf("track %d: %w", id, err)
	}
	if trak, err = shiftChunkOffsets(trak, s.base); err != nil {
		return nil, err
	}
	return rescaleMovieTime(trak, s.movieTS, movieTS)
}

// commonTimescale is the least common multiple of two movie timescales, or the larger one past 2^30.
func commonTimescale(a, b uint32) uint32 {
	x, y := uint64(a), uint64(b)
	for y != 0 {
		x, y = y, x%y
	}
	if l := uint64(a) / x * uint64(b); l <= 1<<30 {
		return uint32(l)
	}
	return max(a, b)
}

// patchMvhdTimescale sets an mvhd's timescale (v0 at 12, v1 at 20).
func patchMvhdTimescale(p []byte, ts uint32) ([]byte, error) {
	off := 12
	if len(p) > 0 && p[0] == 1 {
		off = 20
	}
	if len(p) < off+4 {
		return nil, fmt.Errorf("mvhd too short")
	}
	out := append([]byte(nil), p...)
	binary.BigEndian.PutUint32(out[off:], ts)
	return out, nil
}

// hdlrForm writes a trak's hdlr name as a QuickTime Pascal string (qt) or an ISO C string.
func hdlrForm(trak []byte, qt bool) ([]byte, error) {
	return rewriteAt(trak, []string{"mdia"}, func(mb []memBox) ([][]byte, error) {
		out := make([][]byte, 0, len(mb))
		for _, b := range mb {
			p := b.payload
			if b.typ == "hdlr" && len(p) >= 24 {
				name := bytes.TrimRight(p[24:], "\x00")
				pascal := len(name) > 1 && int(name[0]) == len(name)-1
				switch {
				case qt && !pascal && len(name) < 256:
					p = append(append(append([]byte(nil), p[:24]...), byte(len(name))), name...)
				case !qt && pascal:
					p = append(append(append([]byte(nil), p[:24]...), name[1:]...), 0)
				}
			}
			out = append(out, box(b.typ, p))
		}
		return out, nil
	})
}

// isoForm rewrites a QuickTime trak's mdhd language (a Macintosh code) in its ISO form.
func isoForm(trak []byte) ([]byte, error) {
	return rewriteAt(trak, []string{"mdia"}, func(mb []memBox) ([][]byte, error) {
		out := make([][]byte, 0, len(mb))
		for _, b := range mb {
			p := b.payload
			if b.typ == "mdhd" {
				off := 20
				if len(p) > 0 && p[0] == 1 {
					off = 32
				}
				if len(p) >= off+2 {
					if v := binary.BigEndian.Uint16(p[off:]); v < 0x400 || v == 0x7fff {
						lang := decodeMdhdLanguage(v)
						if lang == "" || v == 0x7fff {
							lang = "und"
						}
						p = append([]byte(nil), p...)
						binary.BigEndian.PutUint16(p[off:], packLanguage(lang))
					}
				}
			}
			out = append(out, box(b.typ, p))
		}
		return out, nil
	})
}

// dropTref removes a trak's track references: they name tracks of its own source.
func dropTref(trak []byte) ([]byte, error) {
	tb, err := iterBoxes(trak)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(tb))
	for _, b := range tb {
		if b.typ != "tref" {
			out = append(out, box(b.typ, b.payload))
		}
	}
	return joinBoxes(out), nil
}

// shiftChunkOffsets adds base to every chunk offset, written as co64.
func shiftChunkOffsets(trak []byte, base int64) ([]byte, error) {
	return rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
		offsets, err := parseChunkOffsets(sb)
		if err != nil {
			return nil, err
		}
		out := make([][]byte, 0, len(sb))
		for _, b := range sb {
			if b.typ != "stco" && b.typ != "co64" {
				out = append(out, box(b.typ, b.payload))
				continue
			}
			out = append(out, fullBox("co64", 0, 0, func(w *bw) {
				w.u32(uint32(len(offsets)))
				for _, off := range offsets {
					w.u64(uint64(off + base))
				}
			}))
		}
		return out, nil
	})
}

// setTrakSlot sets a trak's tkhd track_ID, and its alternate_group to its type's: tracks muxed from files that grouped them apart group alike.
func setTrakSlot(trak []byte, id uint32) ([]byte, error) {
	return rewriteAt(trak, nil, func(tb []memBox) ([][]byte, error) {
		group := alternateGroup(trakHandler(tb), false) // a mux carries no chapter track: it rebuilds one
		out := make([][]byte, 0, len(tb))
		for _, b := range tb {
			if b.typ != "tkhd" {
				out = append(out, box(b.typ, b.payload))
				continue
			}
			p := append([]byte(nil), b.payload...)
			off, gOff := 12, 34
			if len(p) > 0 && p[0] == 1 {
				off, gOff = 20, 46
			}
			if len(p) < gOff+2 {
				return nil, fmt.Errorf("tkhd too short")
			}
			binary.BigEndian.PutUint32(p[off:], id)
			binary.BigEndian.PutUint16(p[gOff:], group)
			out = append(out, box("tkhd", p))
		}
		return out, nil
	})
}

// rescaleMovieTime converts a trak's movie-timescale fields (tkhd duration, edit segment durations) from one movie timescale to another.
func rescaleMovieTime(trak []byte, from, to uint32) ([]byte, error) {
	if from == to {
		return trak, nil
	}
	tb, err := iterBoxes(trak)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(tb))
	for _, b := range tb {
		switch b.typ {
		case "tkhd":
			dur, err := trakDuration(trak)
			if err != nil {
				return nil, err
			}
			p, err := patchTkhdDuration(b.payload, rescale(dur, from, to))
			if err != nil {
				return nil, err
			}
			out = append(out, box("tkhd", p))
		case "edts":
			entries, err := trakElstEntries(tb)
			if err != nil {
				return nil, err
			}
			for i := range entries {
				entries[i].segDur = rescale(entries[i].segDur, from, to)
			}
			out = append(out, buildElstBox(entries))
		default:
			out = append(out, box(b.typ, b.payload))
		}
	}
	return joinBoxes(out), nil
}

// assembleMux builds the output moov payload on the first source's: its mvhd (duration, next_track_ID), the traks, its udta and meta.
func assembleMux(first *muxSource, traks [][]byte, movieDur int64, nextID, movieTS uint32) ([]byte, error) {
	boxes, err := iterBoxes(first.payload)
	if err != nil {
		return nil, err
	}
	var w bw
	for _, b := range boxes {
		switch b.typ {
		case "mvhd":
			p, err := patchMvhdTimescale(b.payload, movieTS)
			if err != nil {
				return nil, err
			}
			if p, err = patchMvhdDuration(p, movieDur); err != nil {
				return nil, err
			}
			if len(p) < 4 {
				return nil, fmt.Errorf("mvhd too short")
			}
			p[len(p)-4], p[len(p)-3], p[len(p)-2], p[len(p)-1] = byte(nextID>>24), byte(nextID>>16), byte(nextID>>8), byte(nextID)
			w.bytes(box("mvhd", p))
			for _, t := range traks {
				w.bytes(box("trak", t))
			}
		case "trak", "iods":
		default:
			w.bytes(box(b.typ, b.payload))
		}
	}
	return w.b, nil
}
