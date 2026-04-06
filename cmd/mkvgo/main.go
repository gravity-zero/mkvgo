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
		commands.RequireArgs(args, 1, "mkvgo probe [-json] <file.mkv>")
		commands.CmdProbe(args[0])
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
	case "extract-subtitle":
		commands.CmdExtractSubtitle(args)
	case "split":
		commands.CmdSplit(args)
	case "join":
		commands.CmdJoin(args)
	case "validate":
		commands.RequireArgs(args, 1, "mkvgo validate [-json] <file.mkv>")
		commands.CmdValidate(args[0])
	case "compare":
		commands.RequireArgs(args, 2, "mkvgo compare [-json] <a.mkv> <b.mkv>")
		commands.CmdCompare(args[0], args[1])
	case "merge":
		commands.CmdMerge(args)
	case "merge-subtitle":
		commands.CmdMergeSubtitle(args)
	case "edit-inplace":
		commands.CmdEditInPlace(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `mkvgo — pure Go MKV/WebM toolkit

Commands:
  info          Show container info
  tracks        List tracks
  chapters      List chapters
  attachments   List attachments
  demux         Extract tracks to raw streams
  mux           Combine tracks into a single MKV
  remove-track  Remove tracks from an MKV
  add-track     Add a track from another MKV
  edit          Edit metadata from JSON (arg or stdin)
  edit-title    Change the container title
  edit-track    Edit track properties (lang, name, default, forced)
  extract-attachment  Extract an attachment to file
  extract-subtitle    Extract subtitle track as SRT
  split         Split MKV by time ranges or chapters
  join          Concatenate multiple MKVs
  merge         Combine all tracks from multiple MKVs
  merge-subtitle  Inject an external SRT into an MKV
  edit-inplace  Edit metadata without rewriting clusters (instant)
  tags          Show tags
  probe         Full dump of all metadata
  validate      Check MKV structure for issues
  compare       Diff metadata of two MKV files

Global flags:
  -json         Output as JSON (info, tracks, chapters, attachments)

Usage: mkvgo <command> [options]`)
}
