package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/gravity-zero/mkvgo/matroska"
)

type EditPatch struct {
	Title    *string            `json:"title"`
	Tracks   []TrackPatch       `json:"tracks"`
	Chapters []matroska.Chapter `json:"chapters"`
	Tags     []matroska.Tag     `json:"tags"`
}

type TrackPatch struct {
	ID        uint64  `json:"id"`
	Language  *string `json:"language"`
	Name      *string `json:"name"`
	IsDefault *bool   `json:"is_default"`
	IsForced  *bool   `json:"is_forced"`
}

func applyPatch(c *matroska.Container, patch EditPatch) {
	if patch.Title != nil {
		c.Info.Title = *patch.Title
	}
	for _, tp := range patch.Tracks {
		for i := range c.Tracks {
			if c.Tracks[i].ID != tp.ID {
				continue
			}
			if tp.Language != nil {
				c.Tracks[i].Language = *tp.Language
			}
			if tp.Name != nil {
				c.Tracks[i].Name = *tp.Name
			}
			if tp.IsDefault != nil {
				c.Tracks[i].IsDefault = *tp.IsDefault
			}
			if tp.IsForced != nil {
				c.Tracks[i].IsForced = *tp.IsForced
			}
		}
	}
	if patch.Chapters != nil {
		c.Chapters = patch.Chapters
	}
	if patch.Tags != nil {
		c.Tags = patch.Tags
	}
}

func CmdEdit(args []string) {
	if len(args) < 4 {
		Fatal(`usage: mkvgo edit <file.mkv> -o <out.mkv> '<json>'
       mkvgo edit <file.mkv> -o <out.mkv> -    (read from stdin)

JSON schema:
{
  "title": "New Title",
  "tracks": [{"id": 2, "language": "fre", "name": "French", "is_default": true, "is_forced": false}],
  "chapters": [{"id": 1, "title": "Intro", "start_ms": 0, "end_ms": 5000}],
  "tags": [{"target_type": "MOVIE", "simple_tags": [{"name": "TITLE", "value": "X"}]}]
}`)
	}
	source := args[0]
	var outPath, jsonStr string

	for i := 1; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			outPath = args[i]
		} else {
			jsonStr = args[i]
		}
	}
	if outPath == "" {
		Fatal("missing -o <out.mkv>")
	}

	var raw []byte
	if jsonStr == "-" {
		var err error
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			Fatal(err.Error())
		}
	} else {
		raw = []byte(jsonStr)
	}

	var patch EditPatch
	if err := json.Unmarshal(raw, &patch); err != nil {
		Fatal(fmt.Sprintf("invalid JSON: %v", err))
	}

	err := matroska.EditMetadata(context.Background(), source, outPath, func(c *matroska.Container) {
		applyPatch(c, patch)
	})
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("edited → %s\n", outPath)
}

func CmdEditTitle(args []string) {
	if len(args) < 4 {
		Fatal("usage: mkvgo edit-title <file.mkv> -o <out.mkv> <title>")
	}
	source := args[0]
	var outPath, title string

	for i := 1; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			outPath = args[i]
		} else {
			title = args[i]
		}
	}
	if outPath == "" || title == "" {
		Fatal("usage: mkvgo edit-title <file.mkv> -o <out.mkv> <title>")
	}

	err := matroska.EditMetadata(context.Background(), source, outPath, func(c *matroska.Container) {
		c.Info.Title = title
	}, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("title set to %q → %s\n", title, outPath)
}

func CmdEditTrack(args []string) {
	if len(args) < 5 {
		Fatal("usage: mkvgo edit-track <file.mkv> -o <out.mkv> -t <trackID> [-lang x] [-name x] [-default|-no-default] [-forced|-no-forced]")
	}
	source := args[0]
	var outPath string
	var trackID uint64
	var lang, name *string
	var setDefault, setForced *bool

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			outPath = args[i]
		case "-t":
			i++
			id, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid track ID %q", args[i]))
			}
			trackID = id
		case "-lang":
			i++
			lang = &args[i]
		case "-name":
			i++
			name = &args[i]
		case "-default":
			v := true
			setDefault = &v
		case "-no-default":
			v := false
			setDefault = &v
		case "-forced":
			v := true
			setForced = &v
		case "-no-forced":
			v := false
			setForced = &v
		}
	}
	if outPath == "" || trackID == 0 {
		Fatal("usage: mkvgo edit-track <file.mkv> -o <out.mkv> -t <trackID> [-lang x] [-name x]")
	}

	err := matroska.EditMetadata(context.Background(), source, outPath, func(c *matroska.Container) {
		for i := range c.Tracks {
			if c.Tracks[i].ID != trackID {
				continue
			}
			if lang != nil {
				c.Tracks[i].Language = *lang
			}
			if name != nil {
				c.Tracks[i].Name = *name
			}
			if setDefault != nil {
				c.Tracks[i].IsDefault = *setDefault
			}
			if setForced != nil {
				c.Tracks[i].IsForced = *setForced
			}
			return
		}
		Fatal(fmt.Sprintf("track %d not found", trackID))
	})
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("edited track %d → %s\n", trackID, outPath)
}

func CmdEditInPlace(args []string) {
	if len(args) < 2 {
		Fatal(`usage: mkvgo edit-inplace <file.mkv> '<json>'
       mkvgo edit-inplace <file.mkv> -    (read from stdin)

Edits metadata in-place without rewriting the file. Instant on large files.
Fails if new metadata is larger than the original.`)
	}
	source := args[0]
	jsonStr := args[1]

	var raw []byte
	if jsonStr == "-" {
		var err error
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			Fatal(err.Error())
		}
	} else {
		raw = []byte(jsonStr)
	}

	var patch EditPatch
	if err := json.Unmarshal(raw, &patch); err != nil {
		Fatal(fmt.Sprintf("invalid JSON: %v", err))
	}

	err := matroska.EditInPlace(context.Background(), source, func(c *matroska.Container) {
		applyPatch(c, patch)
	})
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("edited in-place → %s\n", source)
}
