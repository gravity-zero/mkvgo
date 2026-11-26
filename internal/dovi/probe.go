package dovi

import "github.com/gravity-zero/mkvgo/mkv"

// TailBytes is how much of a sample's end TailLayer wants: the RPU closes the access unit and is far smaller.
const TailBytes = 8 << 10

// Layered reports whether t is a profile 7 track whose first sample can say what its enhancement layer holds.
func Layered(t *mkv.Track) bool {
	return t.Type == mkv.VideoTrack && t.DolbyVision != nil && t.DolbyVision.Profile == 7 &&
		t.Compression == mkv.CompressionNone && len(t.CodecPrivate) >= hvcCMinBytes
}

// Layer returns what the RPU of a whole sample of t says of the enhancement layer.
func Layer(t *mkv.Track, sample []byte) mkv.DolbyVisionEL {
	r, _ := SampleRPU(sample, int(t.CodecPrivate[hvcCLenAt]&3)+1)
	return r.layer()
}

// TailLayer is Layer from the last bytes of a sample alone: the RPU among the NAL units that end the sample there.
func TailLayer(t *mkv.Track, tail []byte) mkv.DolbyVisionEL {
	lenSize := int(t.CodecPrivate[hvcCLenAt]&3) + 1
	for at := 0; at+lenSize < len(tail); at++ {
		if r, ok := chainRPU(tail[at:], lenSize); ok {
			return r.layer()
		}
	}
	return mkv.DolbyVisionELUnknown
}

// chainRPU reads b as NAL units, length first, that end exactly with it, and returns the RPU among them.
func chainRPU(b []byte, lenSize int) (found RPU, ok bool) {
	for len(b) > 0 {
		if len(b) < lenSize+2 {
			return RPU{}, false
		}
		n := 0
		for _, c := range b[:lenSize] {
			n = n<<8 | int(c)
		}
		if b = b[lenSize:]; n < 2 || n > len(b) {
			return RPU{}, false
		}
		if nal := b[:n]; !ok && nal[0]>>1&0x3F == nalRPU {
			r, err := Read(unescape(nal[2:]))
			found, ok = r, err == nil
		}
		b = b[n:]
	}
	return found, ok
}

func (r RPU) layer() mkv.DolbyVisionEL {
	switch {
	case r.Profile != 7:
		return mkv.DolbyVisionELUnknown
	case r.EL == ELMinimal:
		return mkv.DolbyVisionMEL
	case r.EL == ELFull:
		return mkv.DolbyVisionFEL
	}
	return mkv.DolbyVisionELUnknown
}
