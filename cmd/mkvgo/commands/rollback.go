package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
)

const rollbackUsage = "usage: mkvgo rollback <repaired.mkv> <delta.rbd> <restored.mkv>"

// CmdRollback reconstructs the pre-repair original from a repaired file and
// the rollback delta a repair wrote via --rollback-delta. The reconstruction
// is refused when the repaired file changed since the repair, and never
// delivered unless it hashes back to the original exactly.
func CmdRollback(args []string) {
	var rest []string
	for _, a := range args {
		rejectFlagArg(a)
		rest = append(rest, a)
	}
	if len(rest) != 3 {
		Fatal(rollbackUsage)
	}
	repaired, deltaPath, out := rest[0], rest[1], rest[2]
	GuardOverwrite(out)

	delta, err := os.Open(deltaPath)
	if err != nil {
		Fatal(err.Error())
	}
	defer delta.Close()

	if err := matroska.ApplyRollback(context.Background(), repaired, delta, out); err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("restored %s from %s + %s (verified against the original's sha256)\n", out, repaired, deltaPath)
}

// armPreexisting wires the deep-verify diff's preexisting-issue reporting
// into opts and returns the printer to call after ClearProgress on success:
// defects the file already carried do not block a correct operation, but the
// operator must see them (they have their own remedy).
func armPreexisting(opts *matroska.Options) (printPreexisting func()) {
	var seen []matroska.Issue
	opts.OnPreexisting = func(is matroska.Issue) { seen = append(seen, is) }
	return func() {
		for _, is := range seen {
			fmt.Printf("  preexisting issue (not from this operation): %s\n", is.Message)
		}
	}
}

// armRollbackDelta wires a --rollback-delta flag into opts: the delta file is
// created up front and the repair is made to FAIL if the delta cannot be
// written (a user asking for the file must not get a silent empty one).
// printSummary reports the written entry (call it after ClearProgress, on the
// success path); closeFn closes the delta file and must be called when the
// command is done writing.
func armRollbackDelta(opts *matroska.Options, path string) (printSummary, closeFn func()) {
	GuardOverwrite(path)
	f, err := os.Create(path)
	if err != nil {
		Fatal(err.Error())
	}
	opts.RollbackSink = f
	opts.RollbackRequired = true
	var written *matroska.RollbackInfo
	opts.OnRollback = func(i matroska.RollbackInfo) { written = &i }
	printSummary = func() {
		if written != nil {
			fmt.Printf("  rollback delta: %s (%s) - restore with: mkvgo rollback <repaired> %s <restored>\n",
				path, FormatBytes(written.Bytes), path)
		}
	}
	closeFn = func() {
		if err := f.Close(); err != nil {
			Fatal(fmt.Sprintf("close rollback delta %s: %v", path, err))
		}
	}
	return printSummary, closeFn
}
