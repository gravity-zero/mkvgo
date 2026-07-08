package commands_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
)

// TestCmdSalvage_CleanFile runs the CLI salvage command over an intact
// sample file: it should behave like reindex, printing "no damage found".
func TestCmdSalvage_CleanFile(t *testing.T) {
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "salvaged.mkv")

	out := capture(t, func() { commands.CmdSalvage([]string{src, dst}) })
	if out == "" {
		t.Fatal("expected report output, got none")
	}

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open salvaged output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Error("expected non-empty Cues in salvaged output")
	}
}

// TestCmdSalvage_JSON checks --json prints a decodable SalvageReport with no
// damaged ranges for a clean source.
func TestCmdSalvage_JSON(t *testing.T) {
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "salvaged.mkv")

	out := capture(t, func() { commands.CmdSalvage([]string{src, dst, "--json"}) })

	var report matroska.SalvageReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode JSON report: %v\noutput: %s", err, out)
	}
	if len(report.DamagedRanges) != 0 {
		t.Errorf("expected zero damaged ranges for a clean file, got %d", len(report.DamagedRanges))
	}
	if report.ClustersCopied == 0 {
		t.Error("expected ClustersCopied > 0")
	}
}

// TestCmdSalvage_UsageError checks the wrong number of positional args exits
// via Fatal rather than panicking or silently doing nothing.
func TestCmdSalvage_UsageError(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdSalvage([]string{"only-one-arg.mkv"})
	})
}

// TestCmdSalvage_GuardsOverwrite checks salvage refuses to clobber an
// existing output file without -f/--force.
func TestCmdSalvage_GuardsOverwrite(t *testing.T) {
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "existing.mkv")
	if err := matroska.Reindex(context.Background(), src, dst); err != nil {
		t.Fatalf("seed existing output: %v", err)
	}

	mustFatal(t, func() {
		commands.CmdSalvage([]string{src, dst})
	})
}
