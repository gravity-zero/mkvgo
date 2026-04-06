package ops

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Mux(ctx context.Context, opts mkv.MuxOptions, extra ...mkv.Options) error {
	fs := mkv.FSFrom(extra)
	out, err := fs.DoCreate(opts.OutputPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()

	tracks, trackMap, err := buildMuxTracks(ctx, opts.Tracks, fs)
	if err != nil {
		return err
	}

	blocks, timecodeScale, err := collectBlocks(ctx, opts.Tracks, trackMap, fs)
	if err != nil {
		return err
	}

	var durationMs int64
	if len(blocks) > 0 {
		durationMs = blocks[len(blocks)-1].Timecode
	}

	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: timecodeScale,
			MuxingApp:     "mkvgo",
			WritingApp:    "mkvgo",
		},
		Tracks:     tracks,
		Chapters:   opts.Chapters,
		Tags:       opts.Tags,
		DurationMs: durationMs,
	}

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		return err
	}
	if err := writeBlocksAsClusters(mw, blocks, timecodeScale); err != nil {
		return err
	}
	return mw.Finalize()
}

func buildMuxTracks(ctx context.Context, inputs []mkv.TrackInput, fs *mkv.FS) ([]mkv.Track, map[trackKey]uint64, error) {
	var tracks []mkv.Track
	trackMap := make(map[trackKey]uint64)
	nextID := uint64(1)

	for _, inp := range inputs {
		c, err := reader.OpenWithFS(ctx, inp.SourcePath, fs)
		if err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", inp.SourcePath, err)
		}
		var srcTrack *mkv.Track
		for i := range c.Tracks {
			if c.Tracks[i].ID == inp.TrackID {
				srcTrack = &c.Tracks[i]
				break
			}
		}
		if srcTrack == nil {
			return nil, nil, fmt.Errorf("track %d not found in %s", inp.TrackID, inp.SourcePath)
		}

		t := *srcTrack
		t.ID = nextID
		if inp.Language != "" {
			t.Language = inp.Language
		}
		if inp.Name != "" {
			t.Name = inp.Name
		}
		t.IsDefault = inp.IsDefault

		trackMap[trackKey{inp.SourcePath, inp.TrackID}] = nextID
		tracks = append(tracks, t)
		nextID++
	}
	return tracks, trackMap, nil
}

type trackKey struct {
	path    string
	trackID uint64
}

func collectBlocks(ctx context.Context, inputs []mkv.TrackInput, trackMap map[trackKey]uint64, fs *mkv.FS) ([]mkv.Block, int64, error) {
	type sourceReq struct {
		path       string
		wantTracks map[uint64]uint64
	}
	sourceMap := make(map[string]*sourceReq)
	for _, inp := range inputs {
		sr, ok := sourceMap[inp.SourcePath]
		if !ok {
			sr = &sourceReq{path: inp.SourcePath, wantTracks: make(map[uint64]uint64)}
			sourceMap[inp.SourcePath] = sr
		}
		newID := trackMap[trackKey{inp.SourcePath, inp.TrackID}]
		sr.wantTracks[inp.TrackID] = newID
	}

	var allBlocks []mkv.Block
	var timecodeScale int64

	for _, sr := range sourceMap {
		c, err := reader.OpenWithFS(ctx, sr.path, fs)
		if err != nil {
			return nil, 0, err
		}
		if timecodeScale == 0 {
			timecodeScale = c.Info.TimecodeScale
		}

		f, err := fs.DoOpen(sr.path)
		if err != nil {
			return nil, 0, err
		}

		br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
		if err != nil {
			f.Close()
			return nil, 0, err
		}

		for {
			if ctx.Err() != nil {
				f.Close()
				return nil, 0, ctx.Err()
			}
			blk, err := br.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return nil, 0, err
			}
			newID, ok := sr.wantTracks[blk.TrackNumber]
			if !ok {
				continue
			}
			blk.TrackNumber = newID
			allBlocks = append(allBlocks, blk)
		}
		f.Close()
	}

	sort.Slice(allBlocks, func(i, j int) bool {
		return allBlocks[i].Timecode < allBlocks[j].Timecode
	})

	if timecodeScale == 0 {
		timecodeScale = 1000000
	}
	return allBlocks, timecodeScale, nil
}
