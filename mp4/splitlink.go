package mp4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
)

// The freeform atoms that chain a split's parts, MP4's stand-in for Matroska's SegmentUID and PrevUID.
const (
	splitUIDAtom  = "SPLIT_UID"
	splitPrevAtom = "SPLIT_PREV"
	splitCutAtom  = "SPLIT_CUT"
)

// splitLink names a part and the part a split cut just before it; cutNs is how far into the part (ns) the split cut it,
// 0 when it runs to the source's end: a join places the next part there, whatever track runs past it.
type splitLink struct {
	uid, prev string
	cutNs     int64
}

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
		if bounds[i][1] > 0 {
			links[i].cutNs = bounds[i][1] - bounds[i][0]
		}
	}
	if n > 0 {
		links[0].prev = src.prev
		if src.uid != "" {
			links[n-1].uid = src.uid
		}
		if last := &links[n-1]; last.cutNs == 0 && src.cutNs > bounds[n-1][0] {
			last.cutNs = src.cutNs - bounds[n-1][0] // a part of a part: its last piece ends at the cut the source names
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
			case name == splitCutAtom:
				if ns, err := strconv.ParseInt(v, 10, 64); err == nil && ns > 0 {
					l.cutNs = ns
				}
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
			if name, _, ok := freeformText(a.payload); a.typ == "----" && ok && (name == splitUIDAtom || name == splitPrevAtom || name == splitCutAtom) {
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
		if l.cutNs > 0 {
			kids = append(kids, freeformAtom(splitCutAtom, strconv.FormatInt(l.cutNs, 10)))
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
