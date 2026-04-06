package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

func CmdValidate(path string) {
	issues, err := matroska.Validate(context.Background(), path)
	if err != nil {
		Fatal(err.Error())
	}
	if JsonOutput {
		PrintJSON(issues)
		return
	}
	if len(issues) == 0 {
		fmt.Printf("%s: OK\n", path)
		return
	}
	for _, issue := range issues {
		fmt.Println(issue)
	}
}

func CmdCompare(pathA, pathB string) {
	diffs, err := matroska.Compare(context.Background(), pathA, pathB)
	if err != nil {
		Fatal(err.Error())
	}
	if JsonOutput {
		PrintJSON(diffs)
		return
	}
	if len(diffs) == 0 {
		fmt.Println("identical metadata")
		return
	}
	for _, d := range diffs {
		fmt.Println(d)
	}
}
