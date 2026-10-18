package mp4

import (
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// metaAtomNames maps the common iTunes/QuickTime ilst atom types to Matroska-style
// tag names. The leading byte 0xA9 is the "©" copyright-sign prefix.
var metaAtomNames = map[string]string{
	"\xa9nam": "TITLE",
	"\xa9ART": "ARTIST",
	"\xa9alb": "ALBUM",
	"\xa9day": "DATE_RELEASED",
	"\xa9gen": "GENRE",
	"\xa9cmt": "COMMENT",
	"\xa9too": "ENCODER",
	"\xa9wrt": "COMPOSER",
	"desc":    "DESCRIPTION",
}

// parseMP4Tags reads file-level metadata from a udta box's meta/ilst atoms (the
// iTunes-style tags). It returns the tags as Matroska SimpleTags plus the title
// (©nam), for Info.Title, and the cover art (covr) as a Matroska-style
// attachment. Other non-text values are skipped.
func parseMP4Tags(udtaBoxes []memBox) (tags []mkv.SimpleTag, title string, cover *mkv.Attachment, hashes map[uint32]string) {
	tags, title, cover, hashes = parseIlstTags(udtaBoxes)
	for _, b := range udtaBoxes {
		name, ok := metaAtomNames[b.typ]
		if !ok || hasTag(tags, name) {
			continue
		}
		if val := quickTimeText(b.payload); val != "" {
			tags = append(tags, mkv.SimpleTag{Name: name, Value: val})
			if name == "TITLE" {
				title = val
			}
		}
	}
	return tags, title, cover, hashes
}

func hasTag(tags []mkv.SimpleTag, name string) bool {
	for _, t := range tags {
		if t.Name == name {
			return true
		}
	}
	return false
}

// quickTimeText reads the first string of a QuickTime user-data text atom (16-bit size, 16-bit language, text).
func quickTimeText(p []byte) string {
	if len(p) < 4 {
		return ""
	}
	n := int(binary.BigEndian.Uint16(p))
	if n > len(p)-4 {
		return ""
	}
	return strings.TrimRight(string(p[4:4+n]), "\x00")
}

// parseIlstTags reads the iTunes-style meta/ilst atoms of a udta.
func parseIlstTags(udtaBoxes []memBox) (tags []mkv.SimpleTag, title string, cover *mkv.Attachment, hashes map[uint32]string) {
	meta, ok := findMemBox(udtaBoxes, "meta")
	if !ok || len(meta.payload) < 4 {
		return nil, "", nil, nil
	}
	// meta is a FullBox: 4 bytes of version/flags precede its child boxes. A few
	// muxers omit them, so fall back to parsing from the start.
	metaBoxes, err := iterBoxes(meta.payload[4:])
	if err != nil || !hasMemBox(metaBoxes, "ilst") {
		if mb, err2 := iterBoxes(meta.payload); err2 == nil && hasMemBox(mb, "ilst") {
			metaBoxes = mb
		}
	}
	ilst, ok := findMemBox(metaBoxes, "ilst")
	if !ok {
		return nil, "", nil, nil
	}
	atoms, err := iterBoxes(ilst.payload)
	if err != nil {
		return nil, "", nil, nil
	}
	for _, a := range atoms {
		if a.typ == "covr" {
			if att := ilstCoverValue(a.payload); att != nil {
				cover = att
			}
			continue
		}
		if a.typ == "----" {
			// Freeform atom: mkvgo's per-track content hashes live here
			// (name "CONTENT_SHA256_<track_ID>"). Not imported as text tags.
			if id, v, ok := freeformContentHash(a.payload); ok {
				if hashes == nil {
					hashes = map[uint32]string{}
				}
				hashes[id] = v
			}
			continue
		}
		name, ok := metaAtomNames[a.typ]
		if !ok {
			continue
		}
		val := ilstDataValue(a.payload)
		if val == "" {
			continue
		}
		tags = append(tags, mkv.SimpleTag{Name: name, Value: val})
		if name == "TITLE" {
			title = val
		}
	}
	return tags, title, cover, hashes
}

// freeformContentHash decodes a freeform ilst atom when it carries one of
// mkvgo's CONTENT_SHA256_<track_ID> values.
func freeformContentHash(atomPayload []byte) (uint32, string, bool) {
	name, val, ok := freeformText(atomPayload)
	if !ok {
		return 0, "", false
	}
	const prefix = "CONTENT_SHA256_"
	if !strings.HasPrefix(name, prefix) {
		return 0, "", false
	}
	id, err := strconv.ParseUint(name[len(prefix):], 10, 32)
	if err != nil {
		return 0, "", false
	}
	return uint32(id), val, true
}

// freeformText decodes a freeform ilst atom's name and UTF-8 value (false when either is missing).
func freeformText(atomPayload []byte) (name, value string, ok bool) {
	boxes, err := iterBoxes(atomPayload)
	if err != nil {
		return "", "", false
	}
	nameBox, found := findMemBox(boxes, "name")
	if !found || len(nameBox.payload) < 4 {
		return "", "", false
	}
	value = ilstDataValue(atomPayload)
	return string(nameBox.payload[4:]), value, value != "" // skip the name box's version/flags
}

// ilstCoverValue extracts a covr atom's image as a Matroska-style attachment.
// The data box's well-known type selects the format: 13 = JPEG, 14 = PNG.
func ilstCoverValue(atomPayload []byte) *mkv.Attachment {
	boxes, err := iterBoxes(atomPayload)
	if err != nil {
		return nil
	}
	data, ok := findMemBox(boxes, "data")
	if !ok || len(data.payload) < 8 {
		return nil
	}
	name, mime := "cover.jpg", "image/jpeg"
	switch binary.BigEndian.Uint32(data.payload[0:4]) & 0xFFFFFF {
	case 13: // JPEG
	case 14: // PNG
		name, mime = "cover.png", "image/png"
	default:
		return nil
	}
	img := data.payload[8:]
	if len(img) == 0 {
		return nil
	}
	return &mkv.Attachment{
		ID: 1, Name: name, MIMEType: mime,
		Size: int64(len(img)), Data: append([]byte(nil), img...),
	}
}

func hasMemBox(boxes []memBox, typ string) bool {
	_, ok := findMemBox(boxes, typ)
	return ok
}

// ilstDataValue extracts the UTF-8 text value from an ilst atom's "data" box,
// or "" when the atom carries a non-text value (binary/integer, e.g. cover art).
func ilstDataValue(atomPayload []byte) string {
	boxes, err := iterBoxes(atomPayload)
	if err != nil {
		return ""
	}
	data, ok := findMemBox(boxes, "data")
	if !ok || len(data.payload) < 8 {
		return ""
	}
	// data: version(1)+well-known-type(3) reserved(4) value… Type 1 is UTF-8 text.
	if binary.BigEndian.Uint32(data.payload[0:4])&0xFFFFFF != 1 {
		return ""
	}
	return strings.TrimRight(string(data.payload[8:]), "\x00")
}

// chapterTrackRefs returns the set of track_IDs referenced as QuickTime chapter
// tracks (trak/tref/chap) by any track in the movie.
func chapterTrackRefs(moovBoxes []memBox) map[uint32]bool {
	ids := map[uint32]bool{}
	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		tb, err := iterBoxes(b.payload)
		if err != nil {
			continue
		}
		tref, ok := findMemBox(tb, "tref")
		if !ok {
			continue
		}
		rb, err := iterBoxes(tref.payload)
		if err != nil {
			continue
		}
		chap, ok := findMemBox(rb, "chap")
		if !ok {
			continue
		}
		for off := 0; off+4 <= len(chap.payload); off += 4 {
			ids[binary.BigEndian.Uint32(chap.payload[off:off+4])] = true
		}
	}
	return ids
}
