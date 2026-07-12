package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

const retimeUsage = "usage: mkvgo retime <file.mkv|.mp4> --shift <track>=<ms> [--shift <track>=<ms> ...] [--in-place | --replace] [--keep-backup] [--deep-verify] [--strict] [--rollback-delta <file>] (MP4: moov edit-list only, mode flags do not apply)"

// CmdRetime cancels a constant A/V desync by shifting the block timecodes of
// the given tracks in place (2 bytes per block, crash-safe journal, no
// rewrite, no temp file). The classic use is the repack defect where audio
// content starts late: `--shift 2=-900` moves track 2 earlier by 900 ms.
func CmdRetime(args []string) {
	var deepVerify, strict, inPlace, replace, keepBackup bool
	var deltaPath string
	shift := map[uint64]int64{}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--deep-verify":
			deepVerify = true
		case "--strict":
			strict = true
		case "--in-place":
			inPlace = true
		case "--replace":
			replace = true
		case "--keep-backup":
			keepBackup = true
		case "--shift":
			if i+1 >= len(args) {
				Fatal("--shift needs a <track>=<ms> value")
			}
			i++
			track, ms, err := parseShift(args[i])
			if err != nil {
				Fatal(err.Error())
			}
			shift[track] = ms * 1_000_000 // ms -> ns
		case "--rollback-delta":
			if i+1 >= len(args) {
				Fatal("--rollback-delta needs a file path")
			}
			i++
			deltaPath = args[i]
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) != 1 {
		Fatal(retimeUsage)
	}
	if len(shift) == 0 {
		Fatal("at least one --shift <track>=<ms> is required")
	}
	if inPlace && replace {
		Fatal("--in-place and --replace are mutually exclusive (omit both for the automatic choice)")
	}
	if keepBackup && inPlace {
		Fatal("--keep-backup only applies to the rewrite engine (--replace or automatic)")
	}
	path := rest[0]

	// MP4/MOV routes to the edit-list engine: same flag, same sign, but the
	// repair is a moov-only rewrite (no per-block patching, hence none of the
	// Matroska engine's mode flags). Routed by the file's FIRST BYTES, never
	// its name, like the library router - a mislabeled file lands right.
	if isMP4Content(path) {
		if inPlace || replace || keepBackup || deepVerify || strict || deltaPath != "" {
			Fatal("retime on an MP4 edits the moov only; --in-place/--replace/--keep-backup/--deep-verify/--strict/--rollback-delta do not apply")
		}
		if err := mp4.RetimeTracks(context.Background(), path, shift); err != nil {
			Fatal(err.Error())
		}
		fmt.Printf("retimed %s (edit list):\n", path)
		for track, ns := range shift {
			fmt.Printf("  track %d shifted by %+d ms\n", track, ns/1_000_000)
		}
		return
	}

	opts := matroska.Options{Progress: NewProgressBar(), DeepVerify: deepVerify, StrictVerify: strict, KeepBackup: keepBackup}
	printPreexisting := armPreexisting(&opts)
	printDelta := func() {}
	if deltaPath != "" {
		var closeDelta func()
		printDelta, closeDelta = armRollbackDelta(&opts, deltaPath)
		defer closeDelta()
	}

	retimeFn := matroska.RetimeTracks
	switch {
	case inPlace:
		retimeFn = matroska.RetimeTracksInPlace
	case replace:
		retimeFn = matroska.RetimeTracksReplace
	}
	err := retimeFn(context.Background(), path, shift, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	printPreexisting()
	fmt.Printf("retimed %s:\n", path)
	for track, ns := range shift {
		fmt.Printf("  track %d shifted by %+d ms\n", track, ns/1_000_000)
	}
	printDelta()
}

// parseShift parses "<track>=<ms>" (ms may be negative: earlier).
func parseShift(s string) (track uint64, ms int64, err error) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 || eq == len(s)-1 {
		return 0, 0, fmt.Errorf("--shift wants <track>=<ms>, got %q", s)
	}
	track, err = strconv.ParseUint(s[:eq], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("--shift: bad track number %q", s[:eq])
	}
	ms, err = strconv.ParseInt(s[eq+1:], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("--shift: bad millisecond value %q", s[eq+1:])
	}
	if ms == 0 {
		return 0, 0, fmt.Errorf("--shift: a zero shift does nothing")
	}
	return track, ms, nil
}
