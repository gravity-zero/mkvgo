package mp4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// restateContentHashes hashes each output trak anew when the source carried CONTENT_SHA256 atoms, which name the source's samples, not the output's.
func restateContentHashes(ctx context.Context, vs vsource, traks []*rwTrak, others []memBox) ([]memBox, error) {
	ui := -1
	for i, b := range others {
		if b.typ == "udta" {
			ui = i
		}
	}
	if ui < 0 || !carriesContentHashes(others[ui].payload) {
		return others, nil
	}
	ids := make([]uint32, 0, len(traks))
	hashes := map[uint32]string{}
	for _, t := range traks {
		tkhd, err := boxAt(t.payload, "tkhd")
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		order := make([]*rwChunk, len(t.chunks))
		var total int64
		for i := range t.chunks {
			order[i] = &t.chunks[i]
			total += t.chunks[i].size
		}
		if err := copyChunks(ctx, h, vs, order, total, nil); err != nil {
			return nil, err
		}
		id := tkhdTrackID(tkhd)
		ids = append(ids, id)
		hashes[id] = hex.EncodeToString(h.Sum(nil))
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	ub, err := iterBoxes(others[ui].payload)
	if err != nil {
		return nil, err
	}
	var kids [][]byte
	for _, bx := range ub {
		if bx.typ != "meta" {
			kids = append(kids, box(bx.typ, bx.payload))
			continue
		}
		m, err := mapIlst(bx.payload, func(atoms []memBox) [][]byte {
			var out [][]byte
			for _, a := range atoms {
				if _, _, ok := freeformContentHash(a.payload); a.typ == "----" && ok {
					continue
				}
				out = append(out, box(a.typ, a.payload))
			}
			for _, id := range ids {
				out = append(out, freeformAtom(fmt.Sprintf("CONTENT_SHA256_%d", id), hashes[id]))
			}
			return out
		})
		if err != nil {
			return nil, err
		}
		kids = append(kids, m)
	}
	out := append([]memBox(nil), others...)
	out[ui] = memBox{typ: "udta", payload: bytes.Join(kids, nil)}
	return out, nil
}

// carriesContentHashes reports whether a movie udta holds mkvgo's CONTENT_SHA256 atoms.
func carriesContentHashes(udta []byte) bool {
	ub, err := iterBoxes(udta)
	if err != nil {
		return false
	}
	meta, ok := findMemBox(ub, "meta")
	if !ok {
		return false
	}
	found := false
	_, _ = mapIlst(meta.payload, func(atoms []memBox) [][]byte {
		for _, a := range atoms {
			if _, _, ok := freeformContentHash(a.payload); a.typ == "----" && ok {
				found = true
			}
		}
		return nil
	})
	return found
}
