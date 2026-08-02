package ops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"

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
	return CompareContainers(a, b), nil
}

// trackDigest summarises one track's media content: block count, total payload
// bytes and a running SHA-256 over the payloads in file order.
type trackDigest struct {
	blocks int64
	bytes  int64
	hash   [sha256.Size]byte
}

// CompareBlocks diffs the media CONTENT of two Matroska/WebM files, track by
// track (matched by position, like the metadata compare): block count, total
// payload bytes, and a SHA-256 over the payloads in stream order. An empty
// result proves a remux/reindex/split+join round-trip carried every frame
// byte-identically - beyond what the metadata compare can show.
func CompareBlocks(ctx context.Context, pathA, pathB string, opts ...mkv.Options) ([]mkv.Diff, error) {
	fs := mkv.FSFrom(opts)
	_, a, err := digestTracks(ctx, pathA, fs, mkv.ProgressFrom(opts))
	if err != nil {
		return nil, fmt.Errorf("digest %s: %w", pathA, err)
	}
	_, b, err := digestTracks(ctx, pathB, fs, nil)
	if err != nil {
		return nil, fmt.Errorf("digest %s: %w", pathB, err)
	}

	return diffDigests(a, b), nil
}

// CompareBlocksConcat diffs one file's media content against the CONCATENATION
// of several others, in the order given - the question a split asks: "do these
// parts, end to end, still hold everything the source did?".
//
// It answers it WITHOUT building the joined file: the parts are hashed one
// after another into the same per-track accumulator, so a 12-part split of a
// 2 GB film is proven with no temporary copy. An empty result means every track
// matches block for block, byte for byte.
//
// Tracks are matched by POSITION, like CompareBlocks and like the layout Join
// already requires: a part whose track count differs is an error, not a diff,
// because nothing sensible can be lined up after that.
func CompareBlocksConcat(ctx context.Context, path string, parts []string, opts ...mkv.Options) ([]mkv.Diff, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("no parts to compare against")
	}
	fs := mkv.FSFrom(opts)
	_, whole, err := digestTracks(ctx, path, fs, mkv.ProgressFrom(opts))
	if err != nil {
		return nil, fmt.Errorf("digest %s: %w", path, err)
	}
	concat, err := digestConcat(ctx, parts, fs)
	if err != nil {
		return nil, err
	}
	return diffDigests(whole, concat), nil
}

// digestConcat hashes several files as one logical stream per track position.
func digestConcat(ctx context.Context, paths []string, fs *mkv.FS) ([]trackDigest, error) {
	var accs []*digestAcc
	for i, p := range paths {
		c, err := reader.OpenWithFS(ctx, p, fs)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", p, err)
		}
		if accs == nil {
			accs = newTrackAccs(len(c.Tracks))
		} else if len(c.Tracks) != len(accs) {
			return nil, fmt.Errorf("%s has %d tracks, expected %d like %s", p, len(c.Tracks), len(accs), paths[0])
		}
		order := make(map[uint64]int, len(c.Tracks))
		for j, t := range c.Tracks {
			order[t.ID] = j
		}
		if err := digestBlocksInto(ctx, p, fs, c.Info.TimecodeScale, order, accs, nil); err != nil {
			return nil, fmt.Errorf("digest part %d (%s): %w", i+1, p, err)
		}
	}
	return sealTrackAccs(accs), nil
}

func diffDigests(a, b []trackDigest) []mkv.Diff {
	var diffs []mkv.Diff
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		section := fmt.Sprintf("track[%d]", i+1)
		switch {
		case i >= len(a):
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffAdded, Section: section,
				Detail: fmt.Sprintf("%d blocks, %d bytes", b[i].blocks, b[i].bytes)})
		case i >= len(b):
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffRemoved, Section: section,
				Detail: fmt.Sprintf("%d blocks, %d bytes", a[i].blocks, a[i].bytes)})
		case a[i].blocks != b[i].blocks:
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: section + ".blocks",
				Detail: fmt.Sprintf("%d → %d", a[i].blocks, b[i].blocks)})
		case a[i].bytes != b[i].bytes:
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: section + ".bytes",
				Detail: fmt.Sprintf("%d → %d", a[i].bytes, b[i].bytes)})
		case a[i].hash != b[i].hash:
			diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: section + ".content",
				Detail: fmt.Sprintf("payload hash differs (%d blocks, %d bytes each side)", a[i].blocks, a[i].bytes)})
		}
	}
	return diffs
}

// digestAcc accumulates one track's running digest while blocks stream past.
type digestAcc struct {
	h hash.Hash
	d trackDigest
}

func newTrackAccs(n int) []*digestAcc {
	accs := make([]*digestAcc, n)
	for i := range accs {
		accs[i] = &digestAcc{h: sha256.New()}
	}
	return accs
}

func sealTrackAccs(accs []*digestAcc) []trackDigest {
	out := make([]trackDigest, len(accs))
	for i, a := range accs {
		a.h.Sum(a.d.hash[:0])
		out[i] = a.d
	}
	return out
}

// digestBlocksInto walks one file's blocks and folds each payload into the
// accumulator its track maps to. The accumulators are the caller's, so several
// files can be hashed as one stream (see digestConcat).
func digestBlocksInto(ctx context.Context, path string, fs *mkv.FS, timecodeScale int64, order map[uint64]int, accs []*digestAcc, progress mkv.ProgressFunc) error {
	f, err := fs.DoOpen(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, timecodeScale)
	if err != nil {
		return err
	}
	if progress != nil {
		if st, _ := fs.DoStat(path); st != nil {
			br.SetProgress(progress, st.Size())
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		i, ok := order[blk.TrackNumber]
		if !ok {
			continue // block for an undeclared track: not attributable
		}
		accs[i].d.blocks++
		accs[i].d.bytes += int64(len(blk.Data))
		_, _ = accs[i].h.Write(blk.Data) // hash.Hash.Write never errors
	}
}

// digestTracks walks every block of the file and returns the parsed container
// plus one digest per track, ordered like Container.Tracks.
func digestTracks(ctx context.Context, path string, fs *mkv.FS, progress mkv.ProgressFunc) (*mkv.Container, []trackDigest, error) {
	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return nil, nil, err
	}
	order := make(map[uint64]int, len(c.Tracks))
	for i, t := range c.Tracks {
		order[t.ID] = i
	}
	accs := newTrackAccs(len(c.Tracks))
	if err := digestBlocksInto(ctx, path, fs, c.Info.TimecodeScale, order, accs, progress); err != nil {
		return nil, nil, err
	}
	return c, sealTrackAccs(accs), nil
}

// CompareContainers diffs the metadata of two already-parsed containers. It is
// format-agnostic: either side may come from the Matroska reader or from
// mp4.OpenMeta, so a remux round-trip can be verified across formats.
func CompareContainers(a, b *mkv.Container) []mkv.Diff {
	var diffs []mkv.Diff
	diffs = append(diffs, compareInfo(&a.Info, &b.Info)...)
	diffs = append(diffs, compareTracks(a.Tracks, b.Tracks)...)
	diffs = append(diffs, compareChapters(a.Chapters, b.Chapters)...)
	diffs = append(diffs, compareAttachments(a.Attachments, b.Attachments)...)

	if a.DurationMs != b.DurationMs {
		diffs = append(diffs, mkv.Diff{Type: mkv.DiffChanged, Section: "duration", Detail: fmt.Sprintf("%dms → %dms", a.DurationMs, b.DurationMs)})
	}
	return diffs
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
