package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdCMAFMPD writes a DASH manifest over CMAF rungs an external encoder
// produced. Each positional is a rung directory holding one rendition: its
// initialisation segment (init.mp4, or the only .mp4 there) and its .m4s
// media segments; --audio names an audio rendition's directory the same way.
// The manifest references each file relative to where it is written, so the
// rung directories are normally siblings under the manifest's directory.
func CmdCMAFMPD(args []string) {
	outPath, videos, audios := parseCMAFArgs(args, "cmaf-mpd")
	base := "."
	if outPath != "" && outPath != "-" {
		base = filepath.Dir(outPath)
	}
	p := cmafPresentationFromDirs(videos, audios, base)
	data, err := mp4.DASHFromCMAF(context.Background(), p)
	if err != nil {
		Fatal(err.Error())
	}
	if outPath == "" || outPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			Fatal(err.Error())
		}
		return
	}
	GuardOverwrite(outPath)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		Fatal(err.Error())
	}
	fmt.Fprintf(os.Stderr, "manifest.mpd → %s (%d bytes, %d video + %d audio representations)\n", outPath, len(data), len(p.Video), len(p.Audio))
}

// CmdCMAFHLS is the HLS counterpart of cmaf-mpd: it writes master.m3u8 and
// one <id>.m3u8 media playlist per rendition into the output directory, over
// the same rung directories (normally its subdirectories).
func CmdCMAFHLS(args []string) {
	outDir, videos, audios := parseCMAFArgs(args, "cmaf-hls")
	if outDir == "" || outDir == "-" {
		Fatal("usage: " + CmdUsage["cmaf-hls"])
	}
	p := cmafPresentationFromDirs(videos, audios, outDir)
	playlists, err := mp4.HLSFromCMAF(context.Background(), p)
	if err != nil {
		Fatal(err.Error())
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		Fatal(err.Error())
	}
	names := make([]string, 0, len(playlists))
	for name := range playlists {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(outDir, name)
		GuardOverwrite(path)
		if err := os.WriteFile(path, playlists[name], 0o644); err != nil {
			Fatal(err.Error())
		}
	}
	fmt.Fprintf(os.Stderr, "HLS playlists written → %s (%d files; play master.m3u8)\n", outDir, len(names))
}

// parseCMAFArgs reads the shared cmaf-* command line: -o, repeated --audio,
// and the rung directories.
func parseCMAFArgs(args []string, cmd string) (out string, videos, audios []string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i < len(args) {
				out = args[i]
			}
		case "--audio":
			i++
			if i < len(args) {
				audios = append(audios, args[i])
			}
		default:
			rejectFlagArg(args[i])
			videos = append(videos, args[i])
		}
	}
	if len(videos) == 0 && len(audios) == 0 {
		Fatal("usage: " + CmdUsage[cmd])
	}
	return out, videos, audios
}

// cmafPresentationFromDirs describes every rung directory relative to base.
func cmafPresentationFromDirs(videos, audios []string, base string) mp4.CMAFPresentation {
	var p mp4.CMAFPresentation
	for _, d := range videos {
		rep, err := cmafRepFromDir(d, base)
		if err != nil {
			Fatal(err.Error())
		}
		p.Video = append(p.Video, rep)
	}
	for _, d := range audios {
		rep, err := cmafRepFromDir(d, base)
		if err != nil {
			Fatal(err.Error())
		}
		p.Audio = append(p.Audio, rep)
	}
	return p
}

// cmafRepFromDir describes the rendition in dir: init.mp4 (or the only .mp4)
// plus every .m4s in natural order, referenced relative to base.
func cmafRepFromDir(dir, base string) (mp4.CMAFRepresentation, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return mp4.CMAFRepresentation{}, err
	}
	var init string
	var inits, segs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch strings.ToLower(filepath.Ext(name)) {
		case ".m4s":
			segs = append(segs, name)
		case ".mp4":
			inits = append(inits, name)
			if name == "init.mp4" {
				init = name
			}
		}
	}
	switch {
	case init != "":
	case len(inits) == 1:
		init = inits[0]
	case len(inits) == 0:
		return mp4.CMAFRepresentation{}, fmt.Errorf("%s: no initialisation segment (init.mp4) - each rung directory holds one rendition's init.mp4 and its .m4s segments", dir)
	default:
		return mp4.CMAFRepresentation{}, fmt.Errorf("%s: %d .mp4 files and none named init.mp4 - name the initialisation segment init.mp4", dir, len(inits))
	}
	if len(segs) == 0 {
		return mp4.CMAFRepresentation{}, fmt.Errorf("%s: no .m4s media segments", dir)
	}
	sort.Slice(segs, func(i, j int) bool { return naturalLess(segs[i], segs[j]) })

	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return mp4.CMAFRepresentation{}, err
	}
	prefix := ""
	if rel != "." {
		prefix = filepath.ToSlash(rel) + "/"
	}
	return mp4.CMAFRepresentation{Dir: dir, Init: init, Segments: segs, URLPrefix: prefix}, nil
}

// naturalLess orders names with embedded numbers by value ("seg9" before
// "seg10"), so unpadded segment names still come out in playback order.
func naturalLess(a, b string) bool {
	for len(a) > 0 && len(b) > 0 {
		if isDigitByte(a[0]) && isDigitByte(b[0]) {
			ia, ib := digitRun(a), digitRun(b)
			na, nb := strings.TrimLeft(a[:ia], "0"), strings.TrimLeft(b[:ib], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if ia != ib { // same value, fewer leading zeros first
				return ia < ib
			}
			a, b = a[ia:], b[ib:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func digitRun(s string) int {
	n := 0
	for n < len(s) && isDigitByte(s[n]) {
		n++
	}
	return n
}
