package reader

import (
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// maxDocTypeLen bounds the DocType string a header parse keeps. The spec's
// values are short ASCII tokens ("matroska", "webm"); a longer one is not a
// DocType, so it is skipped rather than pulled into memory, and the field stays
// empty as for a header that declares none.
const maxDocTypeLen = 256

// parseEBMLHeaderBody consumes the EBML header body (exactly size bytes from r)
// and fills c.DocType, c.DocTypeVersion and c.DocTypeReadVersion from it. The
// header used to be skipped outright, so the parse is lenient by design: a
// child that is malformed (unknown size, or larger than what is left of the
// header) ends the walk, the rest of the body is discarded, and the file opens
// exactly as it did before - only an I/O error is returned. Whatever was parsed
// before the malformed child is kept.
func parseEBMLHeaderBody(r io.Reader, size int64, c *mkv.Container) error {
	lr := &io.LimitedReader{R: r, N: size}
	for lr.N > 0 {
		h, _, err := ebml.ReadElementHeader(lr)
		if err != nil || h.Size < 0 || h.Size > lr.N {
			break
		}
		switch h.ID {
		case ebml.IDDocType:
			if h.Size > maxDocTypeLen {
				break
			}
			v, err := ebml.ReadString(lr, h.Size)
			if err != nil {
				return err
			}
			c.DocType = v
			continue
		case ebml.IDDocTypeVersion, ebml.IDDocTypeReadVersion:
			if h.Size > 8 {
				break
			}
			v, err := ebml.ReadUint(lr, h.Size)
			if err != nil {
				return err
			}
			if h.ID == ebml.IDDocTypeVersion {
				c.DocTypeVersion = v
			} else {
				c.DocTypeReadVersion = v
			}
			continue
		}
		if _, err := io.CopyN(io.Discard, lr, h.Size); err != nil {
			return err
		}
	}
	// Land on the header's end whether the walk finished or bailed out.
	_, err := io.CopyN(io.Discard, lr, lr.N)
	return err
}
