package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdValidate exits 1 when error-severity issues are found (warnings are
// printed but do not fail), so the command is scriptable
// (`mkvgo validate f.mkv && ...`). -strict makes warnings fail too.
func CmdValidate(args []string) {
	var strict bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-strict", "--strict":
			strict = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["validate"])
	}
	path := rest[0]

	issues, err := matroska.Validate(context.Background(), path)
	if err != nil {
		Fatal(err.Error())
	}
	switch {
	case JsonOutput:
		PrintJSON(issues)
	case len(issues) == 0:
		fmt.Printf("%s: OK\n", path)
	default:
		for _, issue := range issues {
			fmt.Println(issue)
		}
	}
	failing := 0
	for _, issue := range issues {
		if issue.Severity == matroska.SeverityError || strict {
			failing++
		}
	}
	if failing > 0 {
		osExit(1)
	}
}

// CmdCompare exits 0 when the metadata is identical and 1 when it differs.
// Either side may be an MP4/MOV (read via the head-only MP4 probe), so a
// remux round-trip can be verified: `mkvgo compare in.mkv out.mp4`.
func CmdCompare(pathA, pathB string) {
	a, _ := loadContainer(pathA, false)
	b, _ := loadContainer(pathB, false)
	diffs := matroska.CompareContainers(a, b)
	switch {
	case JsonOutput:
		PrintJSON(diffs)
	case len(diffs) == 0:
		fmt.Println("identical metadata")
	default:
		for _, d := range diffs {
			fmt.Println(d)
		}
	}
	if len(diffs) > 0 {
		osExit(1)
	}
}
