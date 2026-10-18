package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

// EditInPlace applies fn's changes to an MP4/MOV's title, tags, chapters and track language/name/default/forced flags by a crash-ordered moov-only rewrite (see RetimeTracks); any other change is refused.
func EditInPlace(ctx context.Context, path string, fn func(c *mkv.Container), opts ...Options) error {
	o := optionsFrom(opts)
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := o.FS.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return errf("edit: %w", err)
	}
	defer f.Close()

	lay, err := scanTopLevelLayout(f)
	if err != nil {
		return errf("edit: %s: %w", shown.Path(path), err)
	}
	if err := checkMoovSize(lay.moovSize-lay.moovHdr, o.moovLimit()); err != nil {
		return err
	}
	moovRaw, err := readRange(f, lay.moovOff, lay.moovSize)
	if err != nil {
		return errf("edit: read moov: %w", err)
	}
	payload := moovRaw[lay.moovHdr:]
	before, mv, err := moovContainer(f, payload, lay.fileSize)
	if err != nil {
		return errf("edit: %s: %w", shown.Path(path), err)
	}
	if mv.fragmented {
		return errf("edit: %s: a fragmented MP4 keeps its moov ahead of the fragments, so it cannot be edited in place; remux it to a progressive MP4 first (from-mp4, then to-mp4)", shown.Path(path))
	}
	if moovEncrypted(payload) {
		return errf("edit: %s: its encryption data (saio) is addressed by file offset and would be left behind by the moved moov: mkvgo does not edit encrypted MP4", shown.Path(path))
	}
	after, _, err := moovContainer(f, payload, lay.fileSize)
	if err != nil {
		return errf("edit: %s: %w", shown.Path(path), err)
	}
	fn(after)
	if err := checkEditable(before, after); err != nil {
		return errf("edit: %w", err)
	}
	if reflect.DeepEqual(before, after) {
		return nil
	}
	base := lay.fileSize + mdatHeaderLen
	moov, chap, err := patchMoovMeta(payload, mv, before, after, base)
	if err != nil {
		return errf("edit: %s: %w", shown.Path(path), err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var blobs [][]byte
	if chap != nil {
		blobs = append(blobs, mdatHeader(int64(len(chap))), chap)
	}
	return appendAndRetire(f, lay, "edit", append(blobs, moov)...)
}

// moovContainer reads a moov payload into the metadata OpenMeta reports, plus the parsed movie.
func moovContainer(r io.ReadSeeker, payload []byte, size int64) (*mkv.Container, *movie, error) {
	mv, err := parseMoov(payload, size, sampleNone)
	if err != nil {
		return nil, nil, err
	}
	if len(mv.chapters) == 0 && mv.chapterTrak != nil {
		if samples := chapterTrackSamples(mv.chapterTrak, size); len(samples) > 0 {
			mv.chapters = readChapterTrack(r, samples)
		}
	}
	return containerFromMovie(mv), mv, nil
}

// editableHint closes every refusal of a field EditInPlace cannot change.
const editableHint = "an MP4 edit changes the title, the tags, the chapters and each track's language, name, default and forced flags; for anything else remux to MKV (from-mp4), edit, and remux back (to-mp4)"

// checkEditable refuses an edit that changes a field outside the editable set, or a value MP4 cannot store.
func checkEditable(before, after *mkv.Container) error {
	if len(after.Tracks) != len(before.Tracks) {
		return fmt.Errorf("the track list cannot change (%d tracks before, %d after): %s", len(before.Tracks), len(after.Tracks), editableHint)
	}
	want := *before
	want.Info.Title = after.Info.Title
	want.Chapters = after.Chapters
	want.Tags = after.Tags
	want.Tracks = slices.Clone(before.Tracks)
	for i := range want.Tracks {
		w, a := &want.Tracks[i], &after.Tracks[i]
		w.Language, w.LanguageBCP47, w.LanguagePresent = a.Language, a.LanguageBCP47, a.LanguagePresent
		w.Name = a.Name
		w.IsDefault, w.DefaultPresent = a.IsDefault, a.DefaultPresent
		w.IsForced, w.ForcedPresent = a.IsForced, a.ForcedPresent
	}
	if field := firstDiff(reflect.ValueOf(want), reflect.ValueOf(*after), ""); field != "" {
		return fmt.Errorf("%s cannot be changed: %s", field, editableHint)
	}
	if !reflect.DeepEqual(want, *after) {
		return fmt.Errorf("an internal field cannot be changed: %s", editableHint)
	}
	for i := range after.Tracks {
		b, a := &before.Tracks[i], &after.Tracks[i]
		if a.Language != b.Language && a.Language != "" && (len(a.Language) != 3 || !isLowerAlpha(a.Language)) {
			return fmt.Errorf("track %d: language %q: MP4 stores a 3-letter ISO 639-2 code here (e.g. \"fre\"); a BCP-47 tag such as \"fr-CA\" goes in LanguageBCP47 (JSON \"language_bcp47\")", a.ID, a.Language)
		}
		if a.LanguageBCP47 != b.LanguageBCP47 && a.LanguageBCP47 != "" && !validBCP47(a.LanguageBCP47) {
			return fmt.Errorf("track %d: %q is not a BCP-47 language tag (letters and digits in subtags of 1 to 8, joined by \"-\", e.g. \"pt-BR\")", a.ID, a.LanguageBCP47)
		}
		if strings.ContainsRune(a.Name, 0) || strings.ContainsRune(a.LanguageBCP47, 0) {
			return fmt.Errorf("track %d: a name or language holding a NUL byte cannot be stored", a.ID)
		}
	}
	if !reflect.DeepEqual(before.Tags, after.Tags) {
		for _, tg := range after.Tags {
			if tg.TargetID != 0 {
				return fmt.Errorf("a tag targeting track %d cannot be stored in an MP4: only file-level tags can", tg.TargetID)
			}
			for _, st := range tg.SimpleTags {
				if _, ok := mp4MetaAtoms[st.Name]; !ok && st.Name != "TITLE" {
					return fmt.Errorf("tag %q has no MP4 counterpart: the tags an MP4 stores are TITLE, %s", st.Name, strings.Join(slices.Sorted(maps.Keys(mp4MetaAtoms)), ", "))
				}
			}
		}
	}
	return nil
}

// validBCP47 checks a tag's shape: a 2 to 8 letter language, then subtags of 1 to 8 letters or digits, 35 characters at most.
func validBCP47(tag string) bool {
	if len(tag) > 35 {
		return false
	}
	for i, sub := range strings.Split(tag, "-") {
		if len(sub) < 1 || len(sub) > 8 || (i == 0 && len(sub) < 2) {
			return false
		}
		for _, r := range sub {
			alpha := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			if !alpha && (i == 0 || r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

// firstDiff names the first field where a and b differ ("" when equal).
func firstDiff(a, b reflect.Value, path string) string {
	if a.Kind() == reflect.Struct {
		for i := 0; i < a.NumField(); i++ {
			if !a.Type().Field(i).IsExported() {
				continue
			}
			if d := firstDiff(a.Field(i), b.Field(i), joinField(path, a.Type().Field(i).Name)); d != "" {
				return d
			}
		}
		return ""
	}
	if a.Kind() == reflect.Slice && a.Len() == b.Len() && a.Type().Elem().Kind() == reflect.Struct {
		for i := 0; i < a.Len(); i++ {
			if d := firstDiff(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(a.Interface(), b.Interface()) {
		return path
	}
	return ""
}

func joinField(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// patchMoovMeta rebuilds the moov with after's metadata, plus the chapter samples to store as mdat payload at chapBase.
func patchMoovMeta(payload []byte, mv *movie, before, after *mkv.Container, chapBase int64) (moov, chapData []byte, err error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, nil, err
	}
	mvhd, ok := findMemBox(boxes, "mvhd")
	if !ok {
		return nil, nil, fmt.Errorf("moov without mvhd")
	}
	movieTS, _ := parseMovieHeader(mvhd.payload)
	if movieTS == 0 {
		return nil, nil, fmt.Errorf("mvhd declares no timescale")
	}
	byTrak := map[int]int{}
	for i := range mv.tracks {
		byTrak[mv.tracks[i].trakIndex] = i
	}
	chaptersChanged := !reflect.DeepEqual(before.Chapters, after.Chapters)
	metaChanged := before.Info.Title != after.Info.Title || !reflect.DeepEqual(before.Tags, after.Tags)

	// The chapter traks the media traks point at are replaced as a whole.
	oldChap := map[uint32]bool{}
	var chapID uint32
	if chaptersChanged {
		oldChap = chapterTrackRefs(boxes)
	}
	maxID := uint32(0)
	trakIdx := -1
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		trakIdx++
		id, _ := trakID(b.payload)
		maxID = max(maxID, id)
		if oldChap[id] {
			if i, visible := byTrak[trakIdx]; visible {
				return nil, nil, fmt.Errorf("track %d is also the file's QuickTime chapter track, so the chapters cannot change without changing that track: remux to MKV (from-mp4), edit, and remux back (to-mp4)", before.Tracks[i].ID)
			}
			if chapID == 0 {
				chapID = id
			}
		}
	}
	var chapTrak []byte
	_, movieTicks := parseMovieHeader(mvhd.payload)
	chs := closeChapters(flattenChapters(after.Chapters), headerTicksToMs(movieTicks, movieTS))
	if chaptersChanged && len(chs) > 0 {
		if chapID == 0 {
			chapID = max(maxID+1, mvhdNextTrackID(mvhd.payload))
		}
		chapTrak, chapData, err = buildEditChapterTrak(chs, chapID, chapBase, movieTS)
		if err != nil {
			return nil, nil, err
		}
	}

	children := make([][]byte, 0, len(boxes)+2)
	sawUdta := false
	lastTrak := -1
	trakIdx = -1
	for _, b := range boxes {
		switch b.typ {
		case "mvhd":
			p := b.payload
			if chapTrak != nil {
				if p, err = patchMvhdNextTrackID(p, chapID+1); err != nil {
					return nil, nil, err
				}
			}
			children = append(children, box("mvhd", p))
		case "trak":
			trakIdx++
			id, _ := trakID(b.payload)
			if oldChap[id] {
				continue
			}
			p := b.payload
			if i, ok := byTrak[trakIdx]; ok && !reflect.DeepEqual(before.Tracks[i], after.Tracks[i]) {
				if p, err = patchTrakMeta(p, &before.Tracks[i], &after.Tracks[i]); err != nil {
					return nil, nil, fmt.Errorf("track %d: %w", before.Tracks[i].ID, err)
				}
			}
			if chaptersChanged {
				if p, err = patchTrakChapRef(p, oldChap, chapTrak != nil, chapID); err != nil {
					return nil, nil, err
				}
			}
			children = append(children, box("trak", p))
			lastTrak = len(children) - 1
		case "udta":
			sawUdta = true
			if !chaptersChanged && !metaChanged {
				children = append(children, box("udta", b.payload))
				continue
			}
			u, err := patchMovieUdta(b.payload, before, after, chaptersChanged, metaChanged)
			if err != nil {
				return nil, nil, err
			}
			if u != nil {
				children = append(children, u)
			}
		default:
			children = append(children, box(b.typ, b.payload))
		}
	}
	if chapTrak != nil {
		children = slices.Insert(children, lastTrak+1, chapTrak)
	}
	if !sawUdta && (chaptersChanged || metaChanged) {
		u, err := patchMovieUdta(nil, before, after, chaptersChanged, metaChanged)
		if err != nil {
			return nil, nil, err
		}
		if u != nil {
			children = append(children, u)
		}
	}
	var w bw
	for _, c := range children {
		w.bytes(c)
	}
	return box("moov", w.b), chapData, nil
}

// closeChapters ends an open last chapter at the movie's end and keeps every chapter within it.
func closeChapters(chs []mkv.Chapter, durMs int64) []mkv.Chapter {
	if durMs <= 0 || len(chs) == 0 {
		return chs
	}
	out := slices.Clone(chs)
	for i := range out {
		if out[i].EndMs == 0 && i == len(out)-1 || out[i].EndMs > durMs {
			out[i].EndMs = max(durMs, out[i].StartMs+1)
		}
	}
	return out
}

// moovEncrypted reports whether a trak keeps CENC auxiliary data at absolute offsets (saio), which moving the moov would orphan.
func moovEncrypted(payload []byte) bool {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return false
	}
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		if stbl, err := boxAt(b.payload, "mdia", "minf", "stbl"); err == nil {
			if sb, err := iterBoxes(stbl); err == nil && hasMemBox(sb, "saio") {
				return true
			}
		}
	}
	return false
}

// trakID reads a trak's tkhd track_ID.
func trakID(trak []byte) (uint32, bool) {
	tb, err := iterBoxes(trak)
	if err != nil {
		return 0, false
	}
	tkhd, ok := findMemBox(tb, "tkhd")
	if !ok {
		return 0, false
	}
	return tkhdTrackID(tkhd.payload), true
}

// mvhdNextTrackID reads the mvhd next_track_ID (its last 4 bytes).
func mvhdNextTrackID(p []byte) uint32 {
	if len(p) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(p[len(p)-4:])
}

// patchMvhdNextTrackID raises the mvhd next_track_ID to at least next.
func patchMvhdNextTrackID(p []byte, next uint32) ([]byte, error) {
	if len(p) < 100 {
		return nil, fmt.Errorf("mvhd too short")
	}
	if mvhdNextTrackID(p) >= next {
		return p, nil
	}
	out := slices.Clone(p)
	binary.BigEndian.PutUint32(out[len(out)-4:], next)
	return out, nil
}

// buildEditChapterTrak builds a QuickTime chapter trak (track_ID id) whose samples, returned too, sit at file offset base.
func buildEditChapterTrak(chs []mkv.Chapter, id uint32, base int64, movieTS uint32) (trak, data []byte, err error) {
	t := &outTrack{
		spec:        codecSpec{handler: "text"},
		mp4ID:       id,
		isChapter:   true,
		chapterList: chs,
		sampleEntry: buildChapterTextEntry(),
	}
	var buf bytes.Buffer
	if err := t.emitChapterSamples(&countWriter{w: &buf}); err != nil {
		return nil, nil, err
	}
	co64 := base+int64(buf.Len()) > math.MaxUint32
	trak, _ = buildTrak(t, base, co64)
	if movieTS != movieTimescale {
		if trak, err = rescaleTrakMovieTime(trak, movieTS); err != nil {
			return nil, nil, err
		}
	}
	return trak, buf.Bytes(), nil
}

// rescaleTrakMovieTime converts a built trak's movie-timescale fields (tkhd duration, edit segment durations) from milliseconds to movieTS.
func rescaleTrakMovieTime(trak []byte, movieTS uint32) ([]byte, error) {
	tb, err := iterBoxes(trak[8:])
	if err != nil {
		return nil, err
	}
	scale := func(v int64) int64 { return v * int64(movieTS) / movieTimescale }
	children := make([][]byte, 0, len(tb))
	for _, b := range tb {
		switch b.typ {
		case "tkhd":
			dur, err := trakDuration(trak[8:])
			if err != nil {
				return nil, err
			}
			p, err := patchTkhdDuration(b.payload, scale(dur))
			if err != nil {
				return nil, err
			}
			children = append(children, box("tkhd", p))
		case "edts":
			entries, err := trakElstEntries(tb)
			if err != nil {
				return nil, err
			}
			for i := range entries {
				entries[i].segDur = scale(entries[i].segDur)
			}
			children = append(children, buildElstBox(entries))
		default:
			children = append(children, box(b.typ, b.payload))
		}
	}
	return container("trak", children...), nil
}

// patchTrakChapRef drops a trak's tref/chap entries naming a replaced chapter trak and, when add, points an audio or video trak at chapID.
func patchTrakChapRef(p []byte, oldChap map[uint32]bool, add bool, chapID uint32) ([]byte, error) {
	tb, err := iterBoxes(p)
	if err != nil {
		return nil, err
	}
	ht := trakHandler(tb)
	wantRef := add && (ht == "vide" || ht == "soun")
	children := make([][]byte, 0, len(tb)+1)
	placed := false
	for _, b := range tb {
		if b.typ != "tref" {
			children = append(children, box(b.typ, b.payload))
			if b.typ == "tkhd" && wantRef && !hasMemBox(tb, "tref") {
				children = append(children, buildTrefChap(chapID))
				placed = true
			}
			continue
		}
		refs, err := iterBoxes(b.payload)
		if err != nil {
			return nil, err
		}
		var kept [][]byte
		for _, r := range refs {
			if r.typ != "chap" {
				kept = append(kept, box(r.typ, r.payload))
				continue
			}
			var w bw
			for off := 0; off+4 <= len(r.payload); off += 4 {
				if id := binary.BigEndian.Uint32(r.payload[off:]); !oldChap[id] {
					w.u32(id)
				}
			}
			if wantRef && !placed {
				w.u32(chapID)
				placed = true
			}
			if len(w.b) > 0 {
				kept = append(kept, box("chap", w.b))
			}
		}
		if wantRef && !placed {
			kept = append(kept, boxf("chap", func(w *bw) { w.u32(chapID) }))
			placed = true
		}
		if len(kept) > 0 {
			children = append(children, container("tref", kept...))
		}
	}
	return joinBoxes(children), nil
}

func joinBoxes(children [][]byte) []byte {
	var w bw
	for _, c := range children {
		w.bytes(c)
	}
	return w.b
}

// patchTrakMeta rewrites one trak's language, name, default and forced flags from b to a.
func patchTrakMeta(p []byte, b, a *mkv.Track) ([]byte, error) {
	tb, err := iterBoxes(p)
	if err != nil {
		return nil, err
	}
	nameChanged := a.Name != b.Name
	forcedChanged := a.IsForced != b.IsForced
	udtaNeeded := !hasMemBox(tb, "udta") && ((nameChanged && a.Name != "") || (forcedChanged && a.IsForced))
	children := make([][]byte, 0, len(tb)+1)
	for _, bx := range tb {
		switch bx.typ {
		case "tkhd":
			tk := bx.payload
			if a.IsDefault != b.IsDefault {
				if len(tk) < 4 {
					return nil, fmt.Errorf("tkhd too short")
				}
				tk = slices.Clone(tk)
				if a.IsDefault {
					tk[3] |= 1
				} else {
					tk[3] &^= 1
				}
			}
			children = append(children, box("tkhd", tk))
		case "udta":
			u, err := patchTrakUdta(bx.payload, a, nameChanged, forcedChanged)
			if err != nil {
				return nil, err
			}
			if u != nil {
				children = append(children, u)
			}
		case "mdia":
			if udtaNeeded {
				if u, _ := patchTrakUdta(nil, a, nameChanged, forcedChanged); u != nil {
					children = append(children, u)
				}
				udtaNeeded = false
			}
			m, err := patchMdiaMeta(bx.payload, b, a)
			if err != nil {
				return nil, err
			}
			children = append(children, m)
		default:
			children = append(children, box(bx.typ, bx.payload))
		}
	}
	return joinBoxes(children), nil
}

// patchTrakUdta rebuilds a trak's udta with a's name and forced marker; nil when nothing is left in it.
func patchTrakUdta(p []byte, a *mkv.Track, nameChanged, forcedChanged bool) ([]byte, error) {
	ub, err := iterBoxes(p)
	if err != nil {
		return nil, err
	}
	var children [][]byte
	for _, bx := range ub {
		if nameChanged && bx.typ == "name" {
			continue
		}
		if forcedChanged && bx.typ == "kind" {
			if scheme, value := parseKind(bx.payload); scheme == dashRoleScheme && strings.HasPrefix(value, "forced") {
				continue
			}
		}
		children = append(children, box(bx.typ, bx.payload))
	}
	if nameChanged {
		if nm := buildTrackName(a.Name); nm != nil {
			children = append(children, nm)
		}
	}
	if forcedChanged && a.IsForced {
		children = append(children, buildKind(dashRoleScheme, "forced-subtitle"))
	}
	if len(children) == 0 {
		return nil, nil
	}
	return container("udta", children...), nil
}

// patchMdiaMeta rewrites the mdhd language, the elng tag and the hdlr name from b to a.
func patchMdiaMeta(p []byte, b, a *mkv.Track) ([]byte, error) {
	mb, err := iterBoxes(p)
	if err != nil {
		return nil, err
	}
	langChanged := a.Language != b.Language
	bcpChanged := a.LanguageBCP47 != b.LanguageBCP47
	var children [][]byte
	for _, bx := range mb {
		switch bx.typ {
		case "mdhd":
			md := bx.payload
			if langChanged {
				off := 20
				if len(md) > 0 && md[0] == 1 {
					off = 32
				}
				if len(md) < off+2 {
					return nil, fmt.Errorf("mdhd too short")
				}
				md = slices.Clone(md)
				binary.BigEndian.PutUint16(md[off:], packLanguage(mdhdLanguage(*a)))
			}
			children = append(children, box("mdhd", md))
			if bcpChanged && a.LanguageBCP47 != "" {
				children = append(children, buildElng(a.LanguageBCP47))
			}
		case "elng":
			// A new legacy code makes the old BCP-47 tag a lie; a new BCP-47 tag was written after the mdhd.
			if langChanged || bcpChanged {
				continue
			}
			children = append(children, box("elng", bx.payload))
		case "hdlr":
			h := bx.payload
			if a.Name != b.Name {
				if h, err = renameHdlr(h, a.Name); err != nil {
					return nil, err
				}
			}
			children = append(children, box("hdlr", h))
		default:
			children = append(children, box(bx.typ, bx.payload))
		}
	}
	return container("mdia", children...), nil
}

// renameHdlr sets an hdlr's name to name, or to the generic one for its handler when name is empty, keeping a QuickTime Pascal string a Pascal string.
func renameHdlr(h []byte, name string) ([]byte, error) {
	if len(h) < 24 {
		return nil, fmt.Errorf("hdlr too short")
	}
	ht := string(h[8:12])
	if name == "" {
		switch ht {
		case "vide", "soun":
			name = handlerName(ht)
		default:
			name = "SubtitleHandler"
		}
	}
	old := bytes.TrimRight(h[24:], "\x00")
	out := slices.Clone(h[:24])
	if len(old) > 1 && int(old[0]) == len(old)-1 && len(name) < 256 {
		return append(append(out, byte(len(name))), name...), nil
	}
	return append(append(out, name...), 0), nil
}

// patchMovieUdta rebuilds the movie-level udta (nil p: none yet) with after's chapter list and title/tags; nil when nothing is left in it.
func patchMovieUdta(p []byte, before, after *mkv.Container, chaptersChanged, metaChanged bool) ([]byte, error) {
	ub, err := iterBoxes(p)
	if err != nil {
		return nil, err
	}
	var children [][]byte
	sawMeta := false
	for _, bx := range ub {
		switch {
		case bx.typ == "chpl" && chaptersChanged:
			continue
		case metaChanged && metaAtomNames[bx.typ] != "":
			continue // a QuickTime text atom: its value now lives in the ilst
		case bx.typ == "meta" && metaChanged:
			sawMeta = true
			m, err := patchMetaBox(bx.payload, editedTitle(before, after), globalTags(after))
			if err != nil {
				return nil, err
			}
			children = append(children, m)
		default:
			children = append(children, box(bx.typ, bx.payload))
		}
	}
	if chaptersChanged {
		if chpl := buildChplBox(after.Chapters); chpl != nil {
			children = append([][]byte{chpl}, children...)
		}
	}
	if metaChanged && !sawMeta {
		if m := buildMovieMeta(editedTitle(before, after), globalTags(after), nil, nil); m != nil {
			children = append(children, m)
		}
	}
	if len(children) == 0 {
		return nil, nil
	}
	return container("udta", children...), nil
}

// editedTitle is the ©nam to write: the title when it changed, else a TITLE tag, else the title unchanged.
func editedTitle(before, after *mkv.Container) string {
	if after.Info.Title != before.Info.Title {
		return after.Info.Title
	}
	for _, st := range globalTags(after) {
		if st.Name == "TITLE" {
			return st.Value
		}
	}
	return after.Info.Title
}

// patchMetaBox replaces the text atoms of a meta box's ilst with title and tags, keeping cover art, freeform and unknown atoms.
func patchMetaBox(p []byte, title string, tags []mkv.SimpleTag) ([]byte, error) {
	fresh := buildMovieMeta(title, tags, nil, nil)
	var freshAtoms []memBox
	if fresh != nil {
		fb, err := iterBoxes(fresh[12:])
		if err != nil {
			return nil, err
		}
		if il, ok := findMemBox(fb, "ilst"); ok {
			if freshAtoms, err = iterBoxes(il.payload); err != nil {
				return nil, err
			}
		}
	}
	head, body := p[:0], p
	if len(p) >= 4 {
		if mb, err := iterBoxes(p[4:]); err == nil && (hasMemBox(mb, "ilst") || hasMemBox(mb, "hdlr")) {
			head, body = p[:4], p[4:]
		}
	}
	mb, err := iterBoxes(body)
	if err != nil {
		return nil, err
	}
	rebuildIlst := func(old []byte) ([]byte, error) {
		atoms, err := iterBoxes(old)
		if err != nil {
			return nil, err
		}
		var kids [][]byte
		for _, a := range freshAtoms {
			kids = append(kids, box(a.typ, a.payload))
		}
		for _, a := range atoms {
			if _, managed := metaAtomNames[a.typ]; !managed {
				kids = append(kids, box(a.typ, a.payload))
			}
		}
		return container("ilst", kids...), nil
	}
	var w bw
	w.bytes(head)
	sawIlst := false
	for _, bx := range mb {
		if bx.typ != "ilst" {
			w.bytes(box(bx.typ, bx.payload))
			continue
		}
		sawIlst = true
		il, err := rebuildIlst(bx.payload)
		if err != nil {
			return nil, err
		}
		w.bytes(il)
	}
	if !sawIlst {
		il, _ := rebuildIlst(nil)
		w.bytes(il)
	}
	return box("meta", w.b), nil
}
