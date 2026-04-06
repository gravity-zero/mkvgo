package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func EditInPlace(ctx context.Context, path string, edit func(*mkv.Container), opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return err
	}

	edit(c)

	region, err := findMetadataRegion(path, fs)
	if err != nil {
		return fmt.Errorf("scan metadata: %w", err)
	}

	var newMeta bytes.Buffer
	if err := writer.WriteSegmentInfo(&newMeta, &c.Info, c.DurationMs); err != nil {
		return err
	}
	if len(c.Tracks) > 0 {
		if err := writer.WriteTracks(&newMeta, c.Tracks); err != nil {
			return err
		}
	}
	if len(c.Chapters) > 0 {
		if err := writer.WriteChapters(&newMeta, c.Chapters); err != nil {
			return err
		}
	}
	if len(c.Attachments) > 0 {
		if err := writer.WriteAttachments(&newMeta, c.Attachments); err != nil {
			return err
		}
	}
	if len(c.Tags) > 0 {
		if err := writer.WriteTags(&newMeta, c.Tags); err != nil {
			return err
		}
	}

	newSize := int64(newMeta.Len())
	available := region.end - region.start

	if newSize > available {
		return fmt.Errorf("new metadata (%d bytes) exceeds available space (%d bytes) — use full rewrite instead", newSize, available)
	}

	f, err := fs.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(region.start, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(newMeta.Bytes()); err != nil {
		return err
	}

	remaining := int(available - newSize)
	if remaining >= 2 {
		if err := writer.WriteVoid(f, remaining); err != nil {
			return err
		}
	} else if remaining == 1 {
		if _, err := f.Write([]byte{0}); err != nil {
			return fmt.Errorf("write padding byte: %w", err)
		}
	}

	return nil
}

type metadataRegion struct {
	start int64
	end   int64
}

func findMetadataRegion(path string, fs *mkv.FS) (metadataRegion, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return metadataRegion{}, err
	}
	defer f.Close()

	h, _, err := ebml.ReadElementHeader(f)
	if err != nil {
		return metadataRegion{}, err
	}
	if _, err := f.Seek(h.Size, io.SeekCurrent); err != nil {
		return metadataRegion{}, err
	}

	h, _, err = ebml.ReadElementHeader(f)
	if err != nil {
		return metadataRegion{}, err
	}
	if h.ID != mkv.IDSegment {
		return metadataRegion{}, fmt.Errorf("expected Segment")
	}

	var segEnd int64 = -1
	if h.Size >= 0 {
		cur, _ := f.Seek(0, io.SeekCurrent)
		segEnd = cur + h.Size
	}

	region := metadataRegion{start: -1}

	for {
		if segEnd >= 0 {
			cur, _ := f.Seek(0, io.SeekCurrent)
			if cur >= segEnd {
				break
			}
		}

		pos, _ := f.Seek(0, io.SeekCurrent)
		eh, _, err := ebml.ReadElementHeader(f)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return metadataRegion{}, err
		}

		switch eh.ID {
		case mkv.IDInfo, mkv.IDTracks, mkv.IDChapters, mkv.IDAttachments, mkv.IDTags, mkv.IDSeekHead, mkv.IDVoid:
			if region.start < 0 {
				region.start = pos
			}
			region.end = pos + int64(ebml.ElementIDLen(eh.ID)) + int64(ebml.DataSizeLen(eh.Size)) + eh.Size
			if _, err := f.Seek(eh.Size, io.SeekCurrent); err != nil {
				return metadataRegion{}, err
			}
		case mkv.IDCluster:
			if region.start < 0 {
				region.start = pos
			}
			if region.end == 0 {
				region.end = pos
			}
			return region, nil
		default:
			if eh.Size < 0 {
				return metadataRegion{}, fmt.Errorf("unknown-size element 0x%X", eh.ID)
			}
			if _, err := f.Seek(eh.Size, io.SeekCurrent); err != nil {
				return metadataRegion{}, err
			}
		}
	}

	if region.start < 0 {
		return metadataRegion{}, fmt.Errorf("no metadata found")
	}
	return region, nil
}
