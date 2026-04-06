package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

func Compare(ctx context.Context, pathA, pathB string, opts ...mkv.Options) ([]mkv.Diff, error) {
	fs := mkv.FSFrom(opts)
	a, err := reader.OpenWithFS(ctx, pathA, fs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", pathA, err)
	}
	b, err := reader.OpenWithFS(ctx, pathB, fs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", pathB, err)
	}

	var diffs []mkv.Diff
	diffs = append(diffs, compareInfo(&a.Info, &b.Info)...)
	diffs = append(diffs, compareTracks(a.Tracks, b.Tracks)...)
	diffs = append(diffs, compareChapters(a.Chapters, b.Chapters)...)
	diffs = append(diffs, compareAttachments(a.Attachments, b.Attachments)...)

	if a.DurationMs != b.DurationMs {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "duration", Detail: fmt.Sprintf("%dms → %dms", a.DurationMs, b.DurationMs)})
	}
	return diffs, nil
}

func compareInfo(a, b *mkv.SegmentInfo) []mkv.Diff {
	var diffs []mkv.Diff
	if a.Title != b.Title {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "info.title", Detail: fmt.Sprintf("%q → %q", a.Title, b.Title)})
	}
	if a.MuxingApp != b.MuxingApp {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "info.muxing_app", Detail: fmt.Sprintf("%q → %q", a.MuxingApp, b.MuxingApp)})
	}
	if a.WritingApp != b.WritingApp {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "info.writing_app", Detail: fmt.Sprintf("%q → %q", a.WritingApp, b.WritingApp)})
	}
	return diffs
}

func compareTracks(a, b []mkv.Track) []mkv.Diff {
	var diffs []mkv.Diff
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}

	for i := 0; i < maxLen; i++ {
		if i >= len(a) {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffAdded, Section: fmt.Sprintf("track[%d]", i+1), Detail: formatTrackSummary(&b[i])})
			continue
		}
		if i >= len(b) {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffRemoved, Section: fmt.Sprintf("track[%d]", i+1), Detail: formatTrackSummary(&a[i])})
			continue
		}
		ta, tb := &a[i], &b[i]
		prefix := fmt.Sprintf("track[%d]", i+1)

		if ta.Type != tb.Type {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: prefix + ".type", Detail: fmt.Sprintf("%s → %s", ta.Type, tb.Type)})
		}
		if ta.Codec != tb.Codec {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: prefix + ".codec", Detail: fmt.Sprintf("%s → %s", ta.Codec, tb.Codec)})
		}
		if ta.Language != tb.Language {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: prefix + ".language", Detail: fmt.Sprintf("%s → %s", ta.Language, tb.Language)})
		}
		if ta.Name != tb.Name {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: prefix + ".name", Detail: fmt.Sprintf("%q → %q", ta.Name, tb.Name)})
		}
		if ta.IsDefault != tb.IsDefault {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: prefix + ".default", Detail: fmt.Sprintf("%v → %v", ta.IsDefault, tb.IsDefault)})
		}
		if ta.IsForced != tb.IsForced {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: prefix + ".forced", Detail: fmt.Sprintf("%v → %v", ta.IsForced, tb.IsForced)})
		}
	}
	return diffs
}

func compareChapters(a, b []mkv.Chapter) []mkv.Diff {
	var diffs []mkv.Diff
	if len(a) != len(b) {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "chapters.count", Detail: fmt.Sprintf("%d → %d", len(a), len(b))})
	}
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	for i := 0; i < minLen; i++ {
		if a[i].Title != b[i].Title {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: fmt.Sprintf("chapter[%d].title", i+1), Detail: fmt.Sprintf("%q → %q", a[i].Title, b[i].Title)})
		}
		if a[i].StartMs != b[i].StartMs || a[i].EndMs != b[i].EndMs {
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: fmt.Sprintf("chapter[%d].time", i+1),
				Detail: fmt.Sprintf("%d-%dms → %d-%dms", a[i].StartMs, a[i].EndMs, b[i].StartMs, b[i].EndMs)})
		}
	}
	return diffs
}

func compareAttachments(a, b []mkv.Attachment) []mkv.Diff {
	var diffs []mkv.Diff
	if len(a) != len(b) {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "attachments.count", Detail: fmt.Sprintf("%d → %d", len(a), len(b))})
	}
	return diffs
}

func formatTrackSummary(t *mkv.Track) string {
	return fmt.Sprintf("%s %s lang=%s name=%q", t.Type, t.Codec, t.Language, t.Name)
}
