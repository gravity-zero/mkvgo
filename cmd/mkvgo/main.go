package main

import (
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var filtered []string
	for _, a := range os.Args[1:] {
		switch a {
		case "-json":
			commands.JsonOutput = true
		case "-f", "--force", "-force":
			commands.Force = true
		case "--version", "-version":
			fmt.Printf("mkvgo %s\n", version)
			os.Exit(0)
		default:
			filtered = append(filtered, a)
		}
	}
	if len(filtered) == 0 || filtered[0] == "--help" || filtered[0] == "-h" {
		usage()
		os.Exit(0)
	}

	cmd := filtered[0]
	args := filtered[1:]

	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		commands.CmdHelp(cmd)
		return
	}

	switch cmd {
	case "info":
		commands.RequireArgs(args, 1, "mkvgo info [-json] <file.mkv>")
		commands.CmdInfo(args[0])
	case "tracks":
		commands.RequireArgs(args, 1, "mkvgo tracks [-json] <file.mkv>")
		commands.CmdTracks(args[0])
	case "chapters":
		commands.RequireArgs(args, 1, "mkvgo chapters [-json] <file.mkv>")
		commands.CmdChapters(args[0])
	case "attachments":
		commands.RequireArgs(args, 1, "mkvgo attachments [-json] <file.mkv>")
		commands.CmdAttachments(args[0])
	case "tags":
		commands.RequireArgs(args, 1, "mkvgo tags [-json] <file.mkv>")
		commands.CmdTags(args[0])
	case "probe":
		commands.RequireArgs(args, 1, "mkvgo probe [-json] <file.mkv|.mp4>")
		commands.CmdProbe(args[0])
	case "keyframes":
		commands.RequireArgs(args, 1, "mkvgo keyframes [-json] <file.mkv|.mp4>")
		commands.CmdKeyframes(args[0])
	case "to-vtt":
		commands.CmdToVTT(args)
	case "demux":
		commands.CmdDemux(args)
	case "mux":
		commands.CmdMux(args)
	case "remove-track":
		commands.CmdRemoveTrack(args)
	case "add-track":
		commands.CmdAddTrack(args)
	case "edit":
		commands.CmdEdit(args)
	case "edit-title":
		commands.CmdEditTitle(args)
	case "edit-track":
		commands.CmdEditTrack(args)
	case "extract-attachment":
		commands.CmdExtractAttachment(args)
	case "add-attachment":
		commands.CmdAddAttachment(args)
	case "set-chapters":
		commands.CmdSetChapters(args)
	case "extract-chapters":
		commands.CmdExtractChapters(args)
	case "remove-attachment":
		commands.CmdRemoveAttachment(args)
	case "extract-subtitle":
		commands.CmdExtractSubtitle(args)
	case "split":
		commands.CmdSplit(args)
	case "join":
		commands.CmdJoin(args)
	case "validate":
		commands.RequireArgs(args, 1, "mkvgo validate [-json] [-strict] <file.mkv>")
		commands.CmdValidate(args)
	case "hash":
		commands.CmdHash(args)
	case "verify":
		commands.CmdVerify(args)
	case "compare":
		commands.RequireArgs(args, 2, "mkvgo compare [-json] [-blocks] <a.mkv|.mp4> <b.mkv|.mp4>")
		commands.CmdCompare(args)
	case "reindex":
		commands.RequireArgs(args, 2, "mkvgo reindex <input.mkv> <output.mkv>")
		commands.CmdReindex(args)
	case "merge":
		commands.CmdMerge(args)
	case "merge-subtitle":
		commands.CmdMergeSubtitle(args)
	case "edit-inplace":
		commands.CmdEditInPlace(args)
	case "to-mp4":
		commands.CmdToMP4(args)
	case "from-mp4":
		commands.CmdFromMP4(args)
	case "to-webm":
		commands.CmdToWebM(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `mkvgo — pure Go MKV/WebM toolkit

Commands:
  info          Show container info (MKV/WebM or MP4)
  tracks        List tracks (MKV/WebM or MP4)
  chapters      List chapters (MKV/WebM or MP4)
  attachments   List attachments
  demux         Extract tracks to raw streams
  mux           Combine tracks into a single MKV
  remove-track  Remove tracks from an MKV
  add-track     Add a track from another MKV
  edit          Edit metadata from JSON (arg or stdin)
  edit-title    Change the container title
  edit-track    Edit track properties (lang, name, default, forced)
  extract-attachment  Extract an attachment to file
  add-attachment      Attach a file (font, cover art; MIME sniffed)
  remove-attachment   Remove an attachment by ID or name
  extract-subtitle    Extract subtitle track as SRT/ASS/WebVTT (MKV or MP4)
  to-vtt        Convert an external .srt/.ass/.vtt sidecar to WebVTT
  keyframes     List video keyframe timestamps (from Cues / sample table)
  split         Split MKV by time ranges or chapters
  join          Concatenate multiple MKVs
  merge         Combine all tracks from multiple MKVs
  merge-subtitle  Inject an external SRT into an MKV
  edit-inplace  Edit metadata without rewriting clusters (instant)
  set-chapters  Replace chapters from an OGM-format text file
  extract-chapters  Export chapters as OGM text (mkvmerge/ffmpeg compatible)
  tags          Show tags
  probe         Full dump of all metadata (MKV/WebM or MP4: colour, Dolby Vision, keyframes, dropped tracks)
  validate      Check MKV structure for issues
  hash          Store per-track content hashes (self-verifying file)
  verify        Recompute content hashes; exit 1 on corruption
  compare       Diff metadata of two files (MKV/WebM or MP4 — verify a remux)
  reindex       Rebuild the seek index (Cues) of a file
  to-mp4        Remux an MKV/WebM to MP4 (--faststart, --skip-unsupported, --flatten-subs, --webvtt-native, --mp3-container-delay)
  from-mp4      Remux an MP4 to MKV (--mp3-container-delay)
  to-webm       Remux an MKV/WebM to WebM (WebM-subset codecs only)

Global flags:
  -json         Output as JSON (info, tracks, chapters, attachments, tags,
                probe, keyframes, validate, compare; ignored elsewhere)
  -f, --force   Overwrite an existing output file (commands that write a new
                file refuse to clobber one without it)

Exit codes: 0 success; 1 error. validate also exits 1 on error-severity issues
(-strict: warnings too); compare exits 1 when the files differ.

Usage: mkvgo <command> [options]`)
}
