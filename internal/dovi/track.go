package dovi

import (
	"errors"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
)

const (
	nalRPU       = 62         // the RPU
	nalEL        = 63         // an enhancement layer NAL unit, wrapped
	hvcEType     = 0x68766345 // "hvcE", the enhancement layer's decoder configuration
	hvcCLenAt    = 21
	hvcCMinBytes = 23
)

// Track rewrites the samples of one HEVC Dolby Vision track: enhancement layer out, RPU converted or out.
type Track struct {
	id      uint64
	lenSize int
	to81    bool
}

// For returns the rewriter of t under c and sets t's configuration to what the rewrite leaves; nil when c leaves t as it is.
func For(t *mkv.Track, c mkv.DolbyVisionConversion) (*Track, error) {
	if !c.Applies(t) {
		return nil, nil
	}
	if t.Compression != mkv.CompressionNone {
		return nil, &mkv.DolbyVisionError{Track: t.ID, Reason: "the track's blocks are compressed in the container"}
	}
	if len(t.CodecPrivate) < hvcCMinBytes {
		return nil, &mkv.DolbyVisionError{Track: t.ID, Reason: "the track has no HEVC decoder configuration to read its NAL unit length from"}
	}
	t.DolbyVision = t.DolbyVision.Converted(c)
	kept := t.BlockAdditionMappings[:0:0]
	for _, m := range t.BlockAdditionMappings {
		if m.Type != hvcEType {
			kept = append(kept, m)
		}
	}
	t.BlockAdditionMappings = kept
	return &Track{id: t.ID, lenSize: int(t.CodecPrivate[hvcCLenAt]&3) + 1, to81: c == mkv.DolbyVisionToProfile81}, nil
}

// Sample appends to dst the sample src rewritten, its base layer NAL units byte for byte; timeMs names it in an error.
func (k *Track) Sample(dst, src []byte, timeMs int64) ([]byte, error) {
	out, err := k.sample(dst, src)
	if err != nil {
		return nil, &mkv.DolbyVisionError{Track: k.id, TimeMs: timeMs, Reason: err.Error()}
	}
	return out, nil
}

func (k *Track) sample(dst, src []byte) ([]byte, error) {
	start := len(dst)
	for len(src) > 0 {
		if len(src) < k.lenSize {
			return nil, errors.New("a NAL unit length runs past the sample")
		}
		n := 0
		for _, b := range src[:k.lenSize] {
			n = n<<8 | int(b)
		}
		src = src[k.lenSize:]
		if n < 2 || n > len(src) {
			return nil, errors.New("a NAL unit runs past the sample")
		}
		nal := src[:n]
		src = src[n:]
		typ, layer := nal[0]>>1&0x3F, nal[0]&1<<5|nal[1]>>3
		switch {
		case typ == nalRPU && layer == 0:
			if !k.to81 {
				continue
			}
			rpu, err := To81(unescape(nal[2:]))
			if err != nil {
				return nil, err
			}
			nal = escape(nal[:2:2], rpu)
		case typ == nalEL || layer != 0:
			continue
		}
		if uint64(len(nal)) >= uint64(1)<<(8*k.lenSize) {
			return nil, fmt.Errorf("a converted NAL unit of %d bytes does not fit its %d-byte length", len(nal), k.lenSize)
		}
		for i := k.lenSize - 1; i >= 0; i-- {
			dst = append(dst, byte(len(nal)>>(8*i)))
		}
		dst = append(dst, nal...)
	}
	if len(dst) == start {
		return nil, errors.New("the sample holds no base layer NAL unit")
	}
	return dst, nil
}

// SampleRPU returns what the first RPU of a sample says, and whether the sample holds one.
func SampleRPU(sample []byte, lenSize int) (RPU, bool) {
	for len(sample) > lenSize {
		n := 0
		for _, b := range sample[:lenSize] {
			n = n<<8 | int(b)
		}
		sample = sample[lenSize:]
		if n < 2 || n > len(sample) {
			break
		}
		if nal := sample[:n]; nal[0]>>1&0x3F == nalRPU {
			r, err := Read(unescape(nal[2:]))
			return r, err == nil
		}
		sample = sample[n:]
	}
	return RPU{}, false
}

// unescape returns a NAL unit's payload without its emulation prevention bytes.
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// escape appends payload to dst with the emulation prevention bytes a NAL unit needs.
func escape(dst, payload []byte) []byte {
	zeros := 0
	for _, c := range payload {
		if zeros >= 2 && c <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		dst = append(dst, c)
	}
	return dst
}

// NAL returns rpu as an HEVC NAL unit: its header, then the RPU with its emulation prevention bytes.
func NAL(rpu []byte) []byte { return escape([]byte{nalRPU << 1, 1}, rpu) }
