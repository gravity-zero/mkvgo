// Package ebml provides low-level EBML (Extensible Binary Meta Language) encoding and decoding.
// EBML is the binary format underlying Matroska (.mkv) and WebM containers.
package ebml

// Element IDs — EBML header
const (
	IDEBMLHeader         = 0x1A45DFA3
	IDEBMLVersion        = 0x4286
	IDEBMLReadVersion    = 0x42F7
	IDEBMLMaxIDLength    = 0x42F2
	IDEBMLMaxSizeLength  = 0x42F3
	IDDocType            = 0x4282
	IDDocTypeVersion     = 0x4287
	IDDocTypeReadVersion = 0x4285
)
