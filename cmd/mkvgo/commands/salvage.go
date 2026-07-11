package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const salvageUsage = "usage: mkvgo salvage <in.mkv> <out.mkv> [--json] [--clean-cut] | mkvgo salvage <in.mkv> --dry-run [--json] [--clean-cut]"

// CmdSalvage runs a best-effort recovery copy of a damaged Matroska/WebM
// file (ops.Salvage): metadata and cluster payloads are carried over
// verbatim and the seek index rebuilt, exactly like reindex, but a
// structural failure inside the cluster stream is not fatal - the damaged
// region is repaired surgically when the bytes allow it (lying sizes
// corrected, valid blocks around a gap kept) and only what cannot be
// recovered is skipped and reported.
//
// --dry-run maps the damage without writing anything: the report printed is
// the one the real salvage would produce, so the operator can decide with
// the numbers in hand.
//
// Exit contract: exit 0 whenever an output file was written (or the dry-run
// completed), whether damage was found or not. Exit 1 with no output only on
// a hard failure - the bounded resync scan giving up without reaching a
// valid Cluster or real EOF, or a genuine I/O error.
func CmdSalvage(args []string) {
	var jsonOut, dryRun, cleanCut bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "--dry-run":
			dryRun = true
		case "--clean-cut":
			cleanCut = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}

	opts := matroska.Options{Progress: NewProgressBar(), CleanCut: cleanCut}

	if dryRun {
		if len(rest) != 1 {
			Fatal(salvageUsage)
		}
		src := rest[0]
		report, err := matroska.MapDamage(context.Background(), src, opts)
		ClearProgress()
		if err != nil {
			Fatal(err.Error())
		}
		if jsonOut || JsonOutput {
			PrintJSON(report)
			return
		}
		fmt.Printf("damage map of %s (dry-run, nothing written)\n", src)
		printSalvageDetail(report)
		return
	}

	if len(rest) != 2 {
		Fatal(salvageUsage)
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)

	report, err := matroska.Salvage(context.Background(), src, dst, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}

	if jsonOut || JsonOutput {
		PrintJSON(report)
		return
	}
	fmt.Printf("salvaged %s -> %s\n", src, dst)
	printSalvageDetail(report)
}

func printSalvageDetail(r *matroska.SalvageReport) {
	fmt.Printf("  %d cluster(s) copied, %s recovered\n", r.ClustersCopied, FormatBytes(r.BytesCopied))
	for i, rr := range r.RepairedRanges {
		fmt.Printf("  repaired range %d: offset %d-%d, %s of media kept that a plain resync would have dropped\n",
			i+1, rr.StartOffset, rr.EndOffset, FormatBytes(rr.BytesKept))
	}
	if r.CleanCutBytes > 0 {
		fmt.Printf("  clean cut: %s of post-gap video dropped up to the next keyframe\n", FormatBytes(r.CleanCutBytes))
	}
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
