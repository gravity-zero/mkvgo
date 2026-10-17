package reader

import (
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// The non-zero defaults RFC 9559 section 5.1 gives the elements the readers parse: an element of size 0 takes its
// default (RFC 8794 section 6.1); a zero default is what an empty integer or float reads as anyway.
var (
	schemaUint = map[uint32]uint64{
		mkv.IDTimecodeScale:   1000000,
		mkv.IDFlagDefault:     1,
		mkv.IDFieldOrder:      2,
		mkv.IDColourMatrix:    2,
		mkv.IDColourTransfer:  2,
		mkv.IDColourPrimaries: 2,
		mkv.IDChannels:        1,
		mkv.IDTargetTypeValue: 50,
	}
	schemaFloat  = map[uint32]float64{mkv.IDSamplingFreq: 8000}
	schemaString = map[uint32]string{mkv.IDLanguage: "eng", mkv.IDChapLanguage: "eng", mkv.IDTagLanguage: "und"}
)

// readUintOf reads h's unsigned integer, its schema default when it is empty.
func readUintOf(r io.Reader, h ebml.ElementHeader) (uint64, error) {
	if d, ok := schemaUint[h.ID]; ok && h.Size == 0 {
		return d, nil
	}
	return ebml.ReadUint(r, h.Size)
}

// readFloatOf reads h's float, its schema default when it is empty.
func readFloatOf(r io.Reader, h ebml.ElementHeader) (float64, error) {
	if d, ok := schemaFloat[h.ID]; ok && h.Size == 0 {
		return d, nil
	}
	return ebml.ReadFloat(r, h.Size)
}

// readStringOf reads h's string, its schema default when it is empty.
func readStringOf(r io.Reader, h ebml.ElementHeader) (string, error) {
	if d, ok := schemaString[h.ID]; ok && h.Size == 0 {
		return d, nil
	}
	return ebml.ReadString(r, h.Size)
}
