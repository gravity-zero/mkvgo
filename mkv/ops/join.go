package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/internal/lang"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Join(ctx context.Context, sources []string, dstPath string, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	if len(sources) == 0 {
		return fmt.Errorf("no sources to join")
	}

	// Open each source once for its metadata (it was opened three times before).
	conts := make([]*mkv.Container, len(sources))
	for i, src := range sources {
		// The attachment payloads stay on disk: Join only forwards them, and a
		// font set held for the whole operation is what a small container cannot
		// afford. They are streamed straight from the source at write time.
		c, err := reader.OpenWithFS(ctx, src, fs, reader.WithoutAttachmentData())
		if err != nil {
			return err
		}
		conts[i] = c
	}
	first := conts[0]

	// Every source must line up with the first: same track count, types AND codecs
	// (a codec mismatch - e.g. H.264 + HEVC - would silently produce a broken file).
	var totalDurationMs int64
	for i, c := range conts {
		if i > 0 {
			if len(c.Tracks) != len(first.Tracks) {
				return fmt.Errorf("%s has %d tracks, expected %d", sources[i], len(c.Tracks), len(first.Tracks))
			}
			for j, t := range c.Tracks {
				ft := first.Tracks[j]
				switch {
				case t.Type != ft.Type:
					return fmt.Errorf("%s track %d: type %s, expected %s", sources[i], j+1, t.Type, ft.Type)
				case otherLanguage(&t, &ft):
					return fmt.Errorf("%s track %d is in %s here and %s in the first file: the join pairs tracks by position, so put them in the same order first (mux), or correct a wrong language (edit-track -lang)", shown.Path(sources[i]), j+1, t.ResolvedLanguage(), ft.ResolvedLanguage())
				case t.Codec != ft.Codec:
					return fmt.Errorf("%s track %d: codec %s, expected %s (cannot concatenate)", sources[i], j+1, t.Codec, ft.Codec)
				case !bytes.Equal(t.CodecPrivate, ft.CodecPrivate):
					return fmt.Errorf("%s track %d: codec configuration differs from the first file (cannot concatenate)", sources[i], j+1)
				case t.Compression != ft.Compression || !bytes.Equal(t.HeaderStripping, ft.HeaderStripping) || t.Encryption != ft.Encryption:
					return fmt.Errorf("%s track %d: its blocks are stored with another compression or encryption than the first file's (cannot concatenate): remux the files the same way first", sources[i], j+1)
				case !samePtr(t.Width, ft.Width) || !samePtr(t.Height, ft.Height):
					return fmt.Errorf("%s track %d: the picture size differs from the first file's (cannot concatenate)", sources[i], j+1)
				case t.Type == mkv.AudioTrack && (orDefault(t.SampleRate, 8000) != orDefault(ft.SampleRate, 8000) ||
					orDefault(t.OutputSampleRate, orDefault(t.SampleRate, 8000)) != orDefault(ft.OutputSampleRate, orDefault(ft.SampleRate, 8000))):
					return fmt.Errorf("%s track %d: the sample rate differs from the first file's (cannot concatenate)", sources[i], j+1)
				case t.Type == mkv.AudioTrack && (orDefault(t.Channels, 1) != orDefault(ft.Channels, 1) || t.BitDepth != nil && ft.BitDepth != nil && *t.BitDepth != *ft.BitDepth):
					return fmt.Errorf("%s track %d: the channels or bit depth differ from the first file's (cannot concatenate)", sources[i], j+1)
				}
			}
		}
		totalDurationMs += c.DurationMs
	}

	out, err := createOutput(fs, dstPath, sources...)
	if err != nil {
		return err
	}
	defer removeUnfinished(fs, dstPath, &err) // a failed write leaves no truncated output
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	if err := writeStartLike(mw, joinedDocTypeSource(conts), first.Tracks); err != nil {
		return err
	}
	// The joined file is as long as its sources put together, not as long as
	// the first one - whose Info the metadata is otherwise copied from.
	//
	// Chapters come from EVERY source, not just the first: they are timeline
	// data, and keeping only the first file's would silently return one chapter
	// where twelve were joined. They cannot be written yet though - each one
	// needs the offset its own source ends up at - so the head only books the
	// room here (sized on the list, which is already known) and the timestamps
	// are filled in once the last block has been written.
	//
	// The content hash and statistics of the first file describe the first file
	// only; the joined one measures its own while streaming and states them
	// after the clusters.
	meta := metaForNewDuration(first)
	meta.Chapters, meta.OtherEditions = nil, nil // the other editions' times are the first part's
	// The joined file is a new segment, not the first part wearing its name: the
	// identity copied along with the Info would have it claim to BE that part,
	// and its NextUID would send a player looking for the second one - which is
	// now inside this very file.
	meta.Info.SegmentUID, meta.Info.PrevUID, meta.Info.NextUID = nil, nil, nil
	// Attachments are pooled, not taken from the first file alone: the fonts an
	// ASS track needs may only be attached to the part that uses them, and a
	// first-wins policy drops them. Matched on name + size, so the same font
	// attached to every part lands once.
	meta.Attachments = mergeAttachments(conts, fs)
	plan := planContentTags(first.Tags).withStatistics()
	meta.Tags = nil // booked below, in the head, filled once measured
	digests, stats := plan.digestsFor(), plan.statsFor()
	// The payloads are still on disk: hand the writer the way to reach them so
	// the element is written from its original position, byte for byte, without
	// a font ever becoming resident.
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := mw.WriteMetadata(&meta, first.Tracks, totalDurationMs); err != nil {
		return err
	}
	if err := mw.ReserveChapters(concatChapters(conts, nil)); err != nil {
		return err
	}
	if err := mw.ReserveTags(plan.upperBoundTags(first.Tracks)); err != nil {
		return err
	}

	// Aggregated progress across the sources (bytes processed / total bytes).
	progress := mkv.ProgressFrom(opts)
	var progDone, progTotal int64
	if progress != nil {
		for _, src := range sources {
			if st, _ := fs.DoStat(src); st != nil {
				progTotal += st.Size()
			}
		}
	}

	// ONE offset shared by every track: a file is appended after the end of the
	// whole previous file, so all its tracks shift by the same amount and keep
	// the alignment they had at the source.
	//
	// Rebasing each track on its own end instead (what this did) slides a track
	// forward relative to the others by however much earlier it happened to
	// stop: a subtitle track whose last cue is minutes before the end of a part
	// comes back minutes early in the next one. On a real episode split at
	// 10:00 that is 5m50s of desync, and the block then lands so far outside its
	// cluster that the write fails on SimpleBlock's int16 relative timecode.
	//
	// The offset is the LAST FRAME'S END across the tracks, not the declared
	// duration: a container that declares more than it holds must not open a
	// hole at the seam.
	//
	// Slices of one timeline get a tighter seam - see videoSeamOffset - because
	// for them the latest end overshoots by the interleaving the cut ran through.
	outVideo := cutVideoTrack(first.Tracks) // output track number of the picture parts are cut on
	// The other video tracks run past a cut up to their own keyframe (Split): neither their end nor the subtitles'
	// says where the picture's seam is.
	outLoose := subtitleTrackSet(first.Tracks)
	for id := range otherVideoTracks(first.Tracks) {
		outLoose[id] = true
	}
	var offset, prevEnd, prevVideoEnd, prevMediaEnd int64 // prevMediaEnd: the end without the subtitle cues
	var lastSubs map[uint64]mkv.Block                     // the previous source's last cue per output subtitle track
	var prevTrackEnds map[uint64]int64                    // the previous source's end per output track
	var prevHasVideoEnd bool
	offsets := make([]int64, len(sources)) // where each source's timeline starts
	for i, src := range sources {
		if i > 0 {
			offset = prevEnd
			if prevHasVideoEnd && linkedTo(&conts[i-1].Info, &conts[i].Info) {
				seam, sounds, ok, err := videoSeamOffset(ctx, conts[i], prevVideoEnd, fs)
				if err != nil {
					return err
				}
				// Never behind the previous source's own start, and never more
				// than the reordering a muxer is allowed - past that it is not
				// interleaving that separates the two ends, it is content, and
				// pulling the seam over it would bury the previous part's tail.
				// Unless the next part's sound takes up exactly where the previous part's stops: that is
				// interleaving, however far ahead of its picture the sound was muxed.
				// A cue on screen past the cut (one a split carried over it) holds no picture: it does not count.
				continuous := soundContinues(sounds, conts[i].Tracks, first.Tracks, prevTrackEnds, seam)
				if ok && seam >= offsets[i-1] && (prevMediaEnd-seam <= defaultClusterDurationMs || continuous) {
					offset = seam
				}
			}
		}
		offsets[i] = offset
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c := conts[i]
		remap := make(map[uint64]uint64, len(c.Tracks))
		for j, t := range c.Tracks {
			remap[t.ID] = first.Tracks[j].ID
		}
		var srcProgress mkv.ProgressFunc
		if progress != nil {
			done := progDone
			srcProgress = func(p, _ int64) { progress(done+p, progTotal) }
			if st, _ := fs.DoStat(src); st != nil {
				progDone += st.Size()
			}
		}
		trackEnds := make(map[uint64]int64, len(c.Tracks))
		var dropLeading map[uint64][]byte // a split's cue across the cut, which the previous part already shows
		if i > 0 && linkedTo(&conts[i-1].Info, &c.Info) {
			for id, last := range lastSubs {
				if last.Timecode+last.Duration > offset-offsets[i-1] { // still on screen at the seam
					if dropLeading == nil {
						dropLeading = map[uint64][]byte{}
					}
					dropLeading[id] = last.Data
				}
			}
		}
		lastSubs = map[uint64]mkv.Block{}
		var priming map[uint64]primingCut
		if i > 0 && !linkedTo(&conts[i-1].Info, &c.Info) {
			priming = primingCuts(c.Tracks) // a part continuing the one before opens on real sound
		}
		var held map[uint64]int64 // the picture the split held to the cut its linked next part opens on
		if i+1 < len(conts) && linkedTo(&c.Info, &conts[i+1].Info) {
			for id, at := range splitHoldAt(c.Tracks, c.Tags) {
				if out, ok := remap[id]; ok {
					if held == nil {
						held = map[uint64]int64{}
					}
					held[out] = at
				}
			}
		}
		if err := streamToWriter(ctx, mw, src, c.Info.TimecodeScale, fs, streamOpts{
			remap: remap, timeOffset: offset, trackEnds: trackEnds,
			subtitleTracks: subtitleTrackSet(c.Tracks), lastSubs: lastSubs, dropLeadingSub: dropLeading,
			primingEndsMs: priming, heldAt: held,
			outScale:       first.Info.TimecodeScale,
			contentDigests: digests, contentStats: stats,
			progress: srcProgress,
		}); err != nil {
			return fmt.Errorf("join %s: %w", shown.Path(src), err)
		}
		prevEnd, prevVideoEnd, prevMediaEnd, prevHasVideoEnd, prevTrackEnds = offset, 0, offset, false, trackEnds
		for id, end := range trackEnds {
			if end > prevEnd {
				prevEnd = end
			}
			if !outLoose[id] && end > prevMediaEnd {
				prevMediaEnd = end
			}
			if outVideo[id] && (!prevHasVideoEnd || end > prevVideoEnd) {
				prevVideoEnd, prevHasVideoEnd = end, true
			}
		}
		offset = prevEnd
	}

	// What was actually written runs to the last seam offset, which is more than
	// the sum of the sources' DECLARED durations whenever they were cut on
	// keyframes (each part keeps the GOP straddling its end). Left as declared,
	// a rejoined film's seek bar stops short - 24.8 s short on a 12-part split.
	if err := mw.RestateDuration(offset); err != nil {
		return err
	}

	// The seam offsets are known now: same values the blocks were written with.
	if err := mw.WriteReservedChapters(concatChapters(conts, offsets)); err != nil {
		return err
	}
	if err := mw.WriteReservedTags(plan.tagsForOutput(first.Tracks, digests, stats)); err != nil {
		return err
	}
	return mw.Finalize()
}

// linkedTo reports whether next is the segment that FOLLOWS prev, in the sense
// Matroska gives hard-linked segments: the parts of one timeline, cut apart.
//
// Join is asked to do two different things with the same call. Appending files
// that have nothing to do with each other - episodes, recorder chunks - the
// seam belongs after everything the previous file holds, its audio tail
// included, and the measured end of the latest track is that. Rejoining slices
// of one file it belongs where the cut was, which is a different place. Nothing
// in the numbers tells the two apart: a part whose audio runs past its picture
// and a film whose audio runs past its picture measure the same. The chain does,
// and it is precise - joining parts 1 and 3 of a split leaves 3 pointing back at
// 2, so the link does not hold and the seam stays where it was.
func linkedTo(prev, next *mkv.SegmentInfo) bool {
	return len(prev.SegmentUID) > 0 && bytes.Equal(next.PrevUID, prev.SegmentUID)
}

// seamSoundSlackMs is how far apart the end of a part's sound and the start of the next one's may be, around the
// picture's seam, for the two to be read as one stream cut there: an audio frame or two.
const seamSoundSlackMs = 50

// seamSoundWindowMs is how far into a part its first sound is looked for: past any lead a muxer gives its sound.
const seamSoundWindowMs = 5000

// soundContinues reports whether every sound track the next source starts within reach takes up, at the seam, where
// the same output track of the previous source stops: the two are one stream cut there. next and out are the next
// source's and the output's tracks, in the order that pairs them.
func soundContinues(sounds map[uint64]int64, next, out []mkv.Track, prevEnds map[uint64]int64, seam int64) bool {
	matched := 0
	for j, t := range next {
		first, ok := sounds[t.ID]
		if !ok || j >= len(out) {
			continue
		}
		end, ok := prevEnds[out[j].ID]
		if !ok {
			continue
		}
		if abs64(end-(seam+first)) > seamSoundSlackMs {
			return false
		}
		matched++
	}
	return matched > 0
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// videoSeamOffset returns where a slice's timeline must start for its picture to
// carry on from the previous slice's, where in the slice each other track (no subtitle) starts, as far as it was
// looked for, and whether that could be established.
//
// A keyframe-aligned cut runs down the file at a video keyframe, so it is exact
// on the video track and on no other: interleaving leaves the part BEFORE the
// cut holding audio from after that keyframe, and the part AFTER it opening on
// audio from before it. Measured, the first part therefore ends later than the
// picture does and the second part's zero stands for an earlier instant than
// its first frame - and rejoined on the latest end, each seam gains the sum of
// the two. On a real film that is 83 ms a seam, 909 ms over eleven of them,
// every one of which the picture had to be dragged through.
//
// So the seam is the picture's: the previous part's last frame ends, less how
// far into this one its own first frame sits. Both parts keep the overlap they
// were cut with, which is the interleaving the source was muxed with.
func videoSeamOffset(ctx context.Context, next *mkv.Container, prevVideoEnd int64, fs *mkv.FS) (int64, map[uint64]int64, bool, error) {
	video, subs := videoTrackSet(next.Tracks), subtitleTrackSet(next.Tracks)
	if len(video) == 0 {
		return 0, nil, false, nil
	}
	f, err := fs.DoOpen(next.Path)
	if err != nil {
		return 0, nil, false, err
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, next.Info.TimecodeScale)
	if err != nil {
		return 0, nil, false, err
	}
	br.SetHeaderOnly(true) // where the blocks are, not what they hold

	// The first video block met is not necessarily the earliest one - a muxer
	// may store the picture slightly behind the sound - so the smallest video
	// timecode of the opening cluster is what counts, and one cluster is as far
	// as that reordering goes (the window Split settles a part's own zero on).
	// The sound may start further on, in a later cluster, when it was muxed far ahead of the picture: it is looked
	// for up to seamSoundWindowMs in.
	var firstTC, best int64
	sounds := map[uint64]int64{}
	var seen, found bool
	for {
		if ctx.Err() != nil {
			return 0, nil, false, ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, nil, false, err
		}
		if !seen {
			firstTC, seen = blk.Timecode, true
		}
		// A scan window, not a cluster being written: how far into the file to
		// look for the seam. It is deliberately NOT clusterSpanMs - nothing here
		// encodes a block offset, so the timebase does not bound it.
		inWindow := br.ClusterCount() <= 1 && blk.Timecode-firstTC < defaultClusterDurationMs
		switch {
		case video[blk.TrackNumber]:
			if inWindow && (!found || blk.Timecode < best) {
				best, found = blk.Timecode, true
			}
		case !subs[blk.TrackNumber]:
			if first, ok := sounds[blk.TrackNumber]; !ok || blk.Timecode < first {
				sounds[blk.TrackNumber] = blk.Timecode
			}
		}
		if blk.Timecode-firstTC >= seamSoundWindowMs || (!inWindow && len(sounds) >= soundTracks(next.Tracks, video, subs)) {
			break
		}
	}
	if !found {
		return 0, nil, false, nil
	}
	return prevVideoEnd - best, sounds, true, nil
}

// soundTracks counts the tracks that are neither video nor subtitles.
func soundTracks(tracks []mkv.Track, video, subs map[uint64]bool) int {
	n := 0
	for _, t := range tracks {
		if !video[t.ID] && !subs[t.ID] {
			n++
		}
	}
	return n
}

// primingCuts gives the audio tracks whose CodecDelay and frame duration are known where their priming ends.
func primingCuts(tracks []mkv.Track) map[uint64]primingCut {
	var out map[uint64]primingCut
	for _, t := range tracks {
		if t.Type != mkv.AudioTrack || t.CodecDelay <= 0 || t.DefaultDurationNs <= 0 {
			continue
		}
		if out == nil {
			out = map[uint64]primingCut{}
		}
		out[t.ID] = primingCut{endMs: float64(t.CodecDelay) / 1e6, frameMs: float64(t.DefaultDurationNs) / 1e6}
	}
	return out
}

// samePtr reports whether two optional values are both absent or equal.
// otherLanguage reports two tracks that both state a language, and not the same one: tracks in another order.
func otherLanguage(a, b *mkv.Track) bool {
	if !a.LanguagePresent || !b.LanguagePresent {
		return false
	}
	la, lb := lang.Primary(a.ResolvedLanguage()), lang.Primary(b.ResolvedLanguage())
	return la != "und" && lb != "und" && la != lb
}

// orDefault is *v, or def where the file leaves the element out: the schema default a reader assumes.
func orDefault[T any](v *T, def T) T {
	if v == nil {
		return def
	}
	return *v
}

func samePtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
