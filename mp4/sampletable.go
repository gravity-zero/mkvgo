package mp4

import "encoding/binary"

// sampletable.go — turns an stbl's child boxes into a flat list of samples with
// absolute file offsets and millisecond decode/composition times. Everything is
// bounds-checked: a malformed table yields an error, never a panic or an
// attacker-sized allocation.

type stscEntry struct {
	firstChunk uint32
	perChunk   uint32
}

// buildSampleTable populates tr.samples from the sample table boxes.
func buildSampleTable(tr *inTrack, stblBoxes []memBox, fileSize int64) error {
	stsz, ok := findMemBox(stblBoxes, "stsz")
	if !ok {
		return errf("stbl without stsz")
	}
	sizes, err := parseStsz(stsz.payload)
	if err != nil {
		return err
	}
	n := len(sizes)

	chunkOffsets, err := parseChunkOffsets(stblBoxes)
	if err != nil {
		return err
	}
	stsc, ok := findMemBox(stblBoxes, "stsc")
	if !ok {
		return errf("stbl without stsc")
	}
	stscEntries, err := parseStsc(stsc.payload)
	if err != nil {
		return err
	}

	durations, err := parseStts(mustFind(stblBoxes, "stts"), n)
	if err != nil {
		return err
	}
	cttsOffsets := parseCtts(stblBoxes, n)
	syncSet := parseStss(stblBoxes)

	samples := make([]inSample, n)

	// Resolve each sample's file offset from the chunk layout.
	si := 0
	for ci, off := range chunkOffsets {
		perChunk := samplesForChunk(uint32(ci+1), stscEntries)
		pos := off
		for k := uint32(0); k < perChunk && si < n; k++ {
			size := sizes[si]
			end := pos + int64(size)
			if pos < 0 || end > fileSize {
				return errf("sample %d at [%d:%d] is outside the file (size %d)", si, pos, end, fileSize)
			}
			samples[si].offset = pos
			samples[si].size = size
			pos = end
			si++
		}
	}
	if si != n {
		return errf("chunk table covers %d samples but stsz lists %d", si, n)
	}

	// Resolve times. dts accumulates in the media timescale; cts = dts + ctts.
	ts := tr.timescale
	dts := int64(0)
	for i := 0; i < n; i++ {
		samples[i].dtsMs = ticksToMs(dts, ts)
		samples[i].ctsMs = ticksToMs(dts+int64(cttsOffsets[i]), ts)
		samples[i].durMs = ticksToMs(int64(durations[i]), ts)
		samples[i].sync = syncSet == nil || syncSet[i+1]
		dts += int64(durations[i])
	}

	tr.samples = samples
	return nil
}

func ticksToMs(ticks int64, timescale uint32) int64 {
	if timescale == 0 {
		return ticks
	}
	return ticks * 1000 / int64(timescale)
}

// mustFind returns the box payload or nil; the caller validates emptiness.
func mustFind(boxes []memBox, typ string) []byte {
	b, _ := findMemBox(boxes, typ)
	return b.payload
}

func parseStsz(payload []byte) ([]uint32, error) {
	if len(payload) < 12 {
		return nil, errf("stsz too short")
	}
	sampleSize := binary.BigEndian.Uint32(payload[4:8])
	count := binary.BigEndian.Uint32(payload[8:12])
	if count > maxSamples {
		return nil, errf("stsz sample_count %d exceeds limit", count)
	}
	sizes := make([]uint32, count)
	if sampleSize != 0 {
		for i := range sizes {
			sizes[i] = sampleSize
		}
		return sizes, nil
	}
	if len(payload) < 12+int(count)*4 {
		return nil, errf("stsz declares %d samples but is too short", count)
	}
	for i := uint32(0); i < count; i++ {
		sizes[i] = binary.BigEndian.Uint32(payload[12+i*4 : 16+i*4])
	}
	return sizes, nil
}

func parseChunkOffsets(stblBoxes []memBox) ([]int64, error) {
	if stco, ok := findMemBox(stblBoxes, "stco"); ok {
		if len(stco.payload) < 8 {
			return nil, errf("stco too short")
		}
		count := binary.BigEndian.Uint32(stco.payload[4:8])
		if len(stco.payload) < 8+int(count)*4 {
			return nil, errf("stco declares %d chunks but is too short", count)
		}
		out := make([]int64, count)
		for i := uint32(0); i < count; i++ {
			out[i] = int64(binary.BigEndian.Uint32(stco.payload[8+i*4 : 12+i*4]))
		}
		return out, nil
	}
	if co64, ok := findMemBox(stblBoxes, "co64"); ok {
		if len(co64.payload) < 8 {
			return nil, errf("co64 too short")
		}
		count := binary.BigEndian.Uint32(co64.payload[4:8])
		if len(co64.payload) < 8+int(count)*8 {
			return nil, errf("co64 declares %d chunks but is too short", count)
		}
		out := make([]int64, count)
		for i := uint32(0); i < count; i++ {
			out[i] = int64(binary.BigEndian.Uint64(co64.payload[8+i*8 : 16+i*8]))
		}
		return out, nil
	}
	return nil, errf("stbl without stco/co64")
}

func parseStsc(payload []byte) ([]stscEntry, error) {
	if len(payload) < 8 {
		return nil, errf("stsc too short")
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if len(payload) < 8+int(count)*12 {
		return nil, errf("stsc declares %d entries but is too short", count)
	}
	out := make([]stscEntry, count)
	for i := uint32(0); i < count; i++ {
		base := 8 + i*12
		out[i] = stscEntry{
			firstChunk: binary.BigEndian.Uint32(payload[base : base+4]),
			perChunk:   binary.BigEndian.Uint32(payload[base+4 : base+8]),
		}
	}
	return out, nil
}

// samplesForChunk returns the samples-per-chunk for 1-based chunk number c, per
// the run-length stsc encoding (the count of the last entry whose first_chunk
// is <= c).
func samplesForChunk(c uint32, entries []stscEntry) uint32 {
	var spc uint32
	for _, e := range entries {
		if c >= e.firstChunk {
			spc = e.perChunk
		} else {
			break
		}
	}
	return spc
}

// parseStts expands the time-to-sample box into n per-sample durations. Missing
// entries default to 0; the expansion is capped at n so a forged run cannot blow
// up memory.
func parseStts(payload []byte, n int) ([]uint32, error) {
	durations := make([]uint32, n)
	if len(payload) < 8 {
		if n == 0 {
			return durations, nil
		}
		return nil, errf("stts missing or too short")
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if len(payload) < 8+int(count)*8 {
		return nil, errf("stts declares %d entries but is too short", count)
	}
	idx := 0
	for i := uint32(0); i < count && idx < n; i++ {
		base := 8 + i*8
		run := binary.BigEndian.Uint32(payload[base : base+4])
		delta := binary.BigEndian.Uint32(payload[base+4 : base+8])
		for j := uint32(0); j < run && idx < n; j++ {
			durations[idx] = delta
			idx++
		}
	}
	return durations, nil
}

// parseCtts expands the composition offset box (version 0 unsigned or version 1
// signed) into n per-sample offsets. Absent or malformed → all zero.
func parseCtts(stblBoxes []memBox, n int) []int32 {
	offsets := make([]int32, n)
	ctts, ok := findMemBox(stblBoxes, "ctts")
	if !ok || len(ctts.payload) < 8 {
		return offsets
	}
	count := binary.BigEndian.Uint32(ctts.payload[4:8])
	if len(ctts.payload) < 8+int(count)*8 {
		return offsets
	}
	idx := 0
	for i := uint32(0); i < count && idx < n; i++ {
		base := 8 + i*8
		run := binary.BigEndian.Uint32(ctts.payload[base : base+4])
		off := int32(binary.BigEndian.Uint32(ctts.payload[base+4 : base+8]))
		for j := uint32(0); j < run && idx < n; j++ {
			offsets[idx] = off
			idx++
		}
	}
	return offsets
}

// parseStss returns the set of 1-based sync sample numbers, or nil when the box
// is absent (meaning every sample is a sync sample).
func parseStss(stblBoxes []memBox) map[int]bool {
	stss, ok := findMemBox(stblBoxes, "stss")
	if !ok || len(stss.payload) < 8 {
		return nil
	}
	count := binary.BigEndian.Uint32(stss.payload[4:8])
	if len(stss.payload) < 8+int(count)*4 {
		return nil
	}
	set := make(map[int]bool, count)
	for i := uint32(0); i < count; i++ {
		set[int(binary.BigEndian.Uint32(stss.payload[8+i*4:12+i*4]))] = true
	}
	return set
}
