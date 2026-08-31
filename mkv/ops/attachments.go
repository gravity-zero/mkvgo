package ops

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// attachmentSource is the way an MKVWriter reaches an attachment payload that
// reader.WithoutAttachmentData left on disk (Data nil, DataPath/DataOffset
// set): opened and positioned on demand, copied straight through, never
// resident. Every op that merely CARRIES a source's attachments to its output
// - a split, a join, a track removed or added, subtitles merged, metadata
// edited - opens the source that way and hands the writer this; only the ops
// that inspect a payload (extracting one) read it whole. An attachment added
// by the caller, Data in hand, is written from Data as before.
func attachmentSource(fs *mkv.FS) func(*mkv.Attachment) (io.Reader, error) {
	return func(a *mkv.Attachment) (io.Reader, error) {
		f, err := fs.DoOpen(a.DataPath)
		if err != nil {
			return nil, err
		}
		if _, err := f.Seek(a.DataOffset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
}

// AddAttachment rewrites srcPath to dstPath with att appended to the
// attachments. A zero att.ID is assigned the next free ID; att.Size defaults
// to len(att.Data).
func AddAttachment(ctx context.Context, srcPath, dstPath string, att mkv.Attachment, opts ...mkv.Options) error {
	if len(att.Data) == 0 {
		return fmt.Errorf("attachment %q has no data", att.Name)
	}
	if att.Size == 0 {
		att.Size = int64(len(att.Data))
	}
	return EditMetadata(ctx, srcPath, dstPath, func(c *mkv.Container) {
		if att.ID == 0 {
			for _, a := range c.Attachments {
				if a.ID >= att.ID {
					att.ID = a.ID + 1
				}
			}
			if att.ID == 0 {
				att.ID = 1
			}
		}
		c.Attachments = append(c.Attachments, att)
	}, opts...)
}

// RemoveAttachment rewrites srcPath to dstPath without the attachment matching
// target - a decimal attachment ID or an exact attachment name. It errors
// (before writing anything) when no attachment matches.
func RemoveAttachment(ctx context.Context, srcPath, dstPath, target string, opts ...mkv.Options) error {
	id, idErr := strconv.ParseUint(target, 10, 64)
	matches := func(a mkv.Attachment) bool {
		return (idErr == nil && a.ID == id) || a.Name == target
	}

	fs := mkv.FSFrom(opts)
	probe, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	found := false
	for _, a := range probe.Attachments {
		if matches(a) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no attachment matching %q", target)
	}

	return EditMetadata(ctx, srcPath, dstPath, func(c *mkv.Container) {
		kept := c.Attachments[:0]
		for _, a := range c.Attachments {
			if matches(a) {
				continue
			}
			kept = append(kept, a)
		}
		c.Attachments = kept
	}, opts...)
}

// SetChapters rewrites srcPath to dstPath with its chapters replaced by
// chapters (e.g. from mkv.ParseOGMChapters).
func SetChapters(ctx context.Context, srcPath, dstPath string, chapters []mkv.Chapter, opts ...mkv.Options) error {
	return EditMetadata(ctx, srcPath, dstPath, func(c *mkv.Container) {
		c.Chapters = chapters
	}, opts...)
}
