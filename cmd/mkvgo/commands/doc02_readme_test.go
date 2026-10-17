package commands

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestDOC02_ReadmeCommandTable checks the README command table names every command CmdUsage knows, and only those.
func TestDOC02_ReadmeCommandTable(t *testing.T) {
	data, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "| Category | Command | Description |")
	if start < 0 {
		t.Fatal("no command table in the README")
	}
	table := text[start:]
	if end := strings.Index(table, "\n\n"); end >= 0 {
		table = table[:end]
	}
	listed := map[string]bool{}
	name := regexp.MustCompile("`([a-z0-9-]+)`")
	for _, row := range strings.Split(table, "\n") {
		cells := strings.Split(row, "|")
		if len(cells) < 4 {
			continue
		}
		for _, m := range name.FindAllStringSubmatch(cells[2], -1) {
			listed[m[1]] = true
		}
	}
	var missing, unknown []string
	for cmd := range CmdUsage {
		if !listed[cmd] {
			missing = append(missing, cmd)
		}
	}
	for cmd := range listed {
		if _, ok := CmdUsage[cmd]; !ok {
			unknown = append(unknown, cmd)
		}
	}
	slices.Sort(missing)
	slices.Sort(unknown)
	if len(missing) > 0 || len(unknown) > 0 {
		t.Fatalf("README command table: missing %v, not commands %v", missing, unknown)
	}
}
