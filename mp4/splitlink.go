package mp4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// The freeform atoms that chain a split's parts, MP4's stand-in for Matroska's SegmentUID and PrevUID.
const (
	splitUIDAtom  = "SPLIT_UID"
	splitPrevAtom = "SPLIT_PREV"
)

// splitLink names a part and the part a split cut just before it.
type splitLink struct{ uid, prev string }

// continues reports whether l is the part a split cut right after prev.
func (l splitLink) continues(prev splitLink) bool {
	return l.prev != "" && l.prev == prev.uid
}

// splitLinks chains the parts cut at bounds (start, end ns) by the source's moov and each cut: deterministic, never linking splits at other cuts.
func splitLinks(seed [32]byte, src splitLink, bounds [][2]int64) []splitLink {
	n := len(bounds)
	links := make([]splitLink, n)
	for i := range links {
		h := sha256.New()
		h.Write([]byte("mkvgo.mp4.split-uid.v2"))
		h.Write(seed[:])
		var cut [16]byte
		binary.BigEndian.PutUint64(cut[:8], uint64(bounds[i][0]))
		binary.BigEndian.PutUint64(cut[8:], uint64(bounds[i][1]))
		h.Write(cut[:])
		links[i].uid = hex.EncodeToString(h.Sum(nil)[:16])
		if i > 0 {
			links[i].prev = links[i-1].uid
		}
	}
	if n > 0 {
		links[0].prev = src.prev
		if src.uid != "" {
			links[n-1].uid = src.uid
		}
	}
	return links
}

// readSplitLink reads the link atoms of a moov's movie-level udta.
func readSplitLink(moovBoxes []memBox) splitLink {
	var l splitLink
	udta, ok := findMemBox(moovBoxes, "udta")
	if !ok {
		return l
	}
	ub, err := iterBoxes(udta.payload)
	if err != nil {
		return l
	}
	meta, ok := findMemBox(ub, "meta")
	if !ok {
		return l
	}
	_, _ = mapIlst(meta.payload, func(atoms []memBox) [][]byte {
		for _, a := range atoms {
			switch name, v, ok := freeformText(a.payload); {
			case !ok || a.typ != "----":
			case name == splitUIDAtom:
				l.uid = v
			case name == splitPrevAtom:
				l.prev = v
			}
		}
		return nil
	})
	return l
}

// withSplitLink sets a moov payload's link atoms to l, the zero link dropping them.
func withSplitLink(payload []byte, l splitLink) ([]byte, error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, err
	}
	var w bw
	sawUdta := false
	for _, b := range boxes {
		if b.typ != "udta" {
			w.bytes(box(b.typ, b.payload))
			continue
		}
		sawUdta = true
		u, err := udtaWithSplitLink(b.payload, l)
		if err != nil {
			return nil, err
		}
		w.bytes(u)
	}
	if !sawUdta && l != (splitLink{}) {
		u, _ := udtaWithSplitLink(nil, l)
		w.bytes(u)
	}
	return w.b, nil
}

// udtaWithSplitLink rebuilds a movie udta with l as its link atoms, adding a meta box when l needs one.
func udtaWithSplitLink(p []byte, l splitLink) ([]byte, error) {
	ub, err := iterBoxes(p)
	if err != nil {
		return nil, err
	}
	relink := func(atoms []memBox) [][]byte {
		var kids [][]byte
		for _, a := range atoms {
			if name, _, ok := freeformText(a.payload); a.typ == "----" && ok && (name == splitUIDAtom || name == splitPrevAtom) {
				continue
			}
			kids = append(kids, box(a.typ, a.payload))
		}
		if l.uid != "" {
			kids = append(kids, freeformAtom(splitUIDAtom, l.uid))
		}
		if l.prev != "" {
			kids = append(kids, freeformAtom(splitPrevAtom, l.prev))
		}
		return kids
	}
	var kids [][]byte
	sawMeta := false
	for _, bx := range ub {
		if bx.typ != "meta" {
			kids = append(kids, box(bx.typ, bx.payload))
			continue
		}
		sawMeta = true
		m, err := mapIlst(bx.payload, relink)
		if err != nil {
			return nil, err
		}
		kids = append(kids, m)
	}
	if !sawMeta && l != (splitLink{}) {
		kids = append(kids, ilstMeta(relink(nil)...))
	}
	return container("udta", kids...), nil
}
