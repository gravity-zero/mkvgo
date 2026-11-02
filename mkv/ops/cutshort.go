package ops

import (
	"bytes"
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// CutShort is a source that stops before the end it declares: its metadata reads whole, its media does not.
type CutShort struct {
	FileSize     int64 `json:"file_size"`
	DeclaredSize int64 `json:"declared_size"`
}

// SourceCutShort reports whether the Matroska/WebM or MP4/MOV source at path stops before the end it declares - its
// Segment, its top-level boxes - from the head only: nil for a whole file, trailing junk, a Segment of unknown size or
// a fragmented MP4 (no declared end). Diagnose says "truncated" on the same sizes.
func SourceCutShort(ctx context.Context, path string, opts ...mkv.Options) (*CutShort, error) {
	if err := oneopt.Check(opts); err != nil {
		return nil, err
	}
	f, err := mkv.FSFrom(opts).DoOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return CutShortOf(ctx, f)
}

// CutShortOf is SourceCutShort over an open source, read from its start.
func CutShortOf(ctx context.Context, r io.ReadSeeker) (*CutShort, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, nil // too short to declare anything
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var declared int64
	if bytes.Equal(magic, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		end, known, err := segmentDeclaredEnd(r)
		if err != nil || !known {
			return nil, nil
		}
		declared = end
	} else if _, declared, err = mp4.DeclaredSizeOf(ctx, r); err != nil {
		return nil, err
	}
	if declared <= size {
		return nil, nil
	}
	return &CutShort{FileSize: size, DeclaredSize: declared}, nil
}
