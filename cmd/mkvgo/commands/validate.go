package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdValidate exits 0 when the file is clean and 1 when issues are found, so
// the command is scriptable (`mkvgo validate f.mkv && ...`).
func CmdValidate(path string) {
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
	if len(issues) > 0 {
		osExit(1)
	}
}

// CmdCompare exits 0 when the metadata is identical and 1 when it differs.
func CmdCompare(pathA, pathB string) {
	diffs, err := matroska.Compare(context.Background(), pathA, pathB)
	if err != nil {
		Fatal(err.Error())
	}
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
