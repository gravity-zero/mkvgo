package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const salvageUsage = "usage: mkvgo salvage <in.mkv> <out.mkv> [--json]"

// CmdSalvage runs a best-effort recovery copy of a damaged Matroska/WebM
// file (ops.Salvage): metadata and cluster payloads are carried over
// verbatim and the seek index rebuilt, exactly like reindex, but a
// structural failure inside the cluster stream is not fatal - it is skipped
// and reported instead of aborting the whole file.
//
// Exit contract: exit 0 whenever an output file was written, whether damage
// was found or not (a clean source behaves like reindex, zero damaged
// ranges). Exit 1 with no output only on a hard failure - the bounded resync
// scan giving up without reaching a valid Cluster or real EOF, or a genuine
// I/O error.
func CmdSalvage(args []string) {
	var jsonOut bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) != 2 {
		Fatal(salvageUsage)
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)

	report, err := matroska.Salvage(context.Background(), src, dst, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}

	if jsonOut || JsonOutput {
		PrintJSON(report)
		return
	}
	printSalvageReport(src, dst, report)
}

func printSalvageReport(src, dst string, r *matroska.SalvageReport) {
	fmt.Printf("salvaged %s -> %s\n", src, dst)
	fmt.Printf("  %d cluster(s) copied, %s recovered\n", r.ClustersCopied, FormatBytes(r.BytesCopied))
	if len(r.DamagedRanges) == 0 {
		fmt.Println("  no damage found (equivalent to reindex)")
		return
	}
	for i, dr := range r.DamagedRanges {
		fmt.Printf("  damaged range %d: offset %d-%d (%s), approx %s-%s\n",
			i+1, dr.StartOffset, dr.EndOffset, FormatBytes(dr.EndOffset-dr.StartOffset),
			FmtMs(dr.ApproxStartMs), FmtMs(dr.ApproxEndMs))
	}
	total := r.BytesCopied + r.BytesSkipped
	pct := 100.0
	if total > 0 {
		pct = float64(r.BytesCopied) / float64(total) * 100
	}
	fmt.Printf("  recovered ~%.1f%% of the media (%s skipped across %d damaged range(s))\n",
		pct, FormatBytes(r.BytesSkipped), len(r.DamagedRanges))
}
