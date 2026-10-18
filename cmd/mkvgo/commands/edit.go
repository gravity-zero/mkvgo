package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/lang"
	"github.com/gravity-zero/mkvgo/internal/outguard"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

type EditPatch struct {
	Title    *string            `json:"title"`
	Tracks   []TrackPatch       `json:"tracks"`
	Chapters []matroska.Chapter `json:"chapters"`
	Tags     []matroska.Tag     `json:"tags"`
}

type TrackPatch struct {
	ID       uint64  `json:"id"`
	Language *string `json:"language"`
	// LanguageBCP47 is the full BCP-47 tag (e.g. "pt-BR") next to the 3-letter Language.
	LanguageBCP47 *string `json:"language_bcp47"`
	Name          *string `json:"name"`
	IsDefault     *bool   `json:"is_default"`
	IsForced      *bool   `json:"is_forced"`
}

func applyPatch(c *matroska.Container, patch EditPatch) {
	if patch.Title != nil {
		c.Info.Title = *patch.Title
	}
	for _, tp := range patch.Tracks {
		found := false
		for i := range c.Tracks {
			if c.Tracks[i].ID != tp.ID {
				continue
			}
			found = true
			if tp.Language != nil {
				c.Tracks[i].Language, c.Tracks[i].LanguageBCP47 = lang.Set(c.Tracks[i].Language, c.Tracks[i].LanguageBCP47, *tp.Language)
			}
			if tp.LanguageBCP47 != nil {
				c.Tracks[i].LanguageBCP47 = *tp.LanguageBCP47
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
		if !found {
			Fatal(fmt.Sprintf("track %d not found (the file has %d tracks)", tp.ID, len(c.Tracks)))
		}
	}
	if patch.Chapters != nil {
		c.Chapters = patch.Chapters
	}
	if patch.Tags != nil {
		c.Tags = patch.Tags
	}
}

// decodePatch parses an edit patch, refusing a field it does not know rather than ignoring it.
func decodePatch(raw []byte) EditPatch {
	var patch EditPatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		Fatal(fmt.Sprintf("invalid JSON: %v", err))
	}
	if _, err := dec.Token(); err != io.EOF {
		Fatal("invalid JSON: text follows the patch object")
	}
	return patch
}

// mp4Options is the CLI's mp4.Options: a progress bar, and every track left out reported.
func mp4Options() mp4.Options {
	return mp4.Options{Progress: NewProgressBar(), OnDrop: func(d mp4.DroppedTrack) {
		fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}}
}

// guardOutput refuses an output that exists (unless -f) or that is one of the inputs, which -f never allows.
func guardOutput(outPath string, inputs ...string) {
	guardDistinct(outPath, inputs...)
	GuardOverwrite(outPath)
}

// guardDistinct refuses an output that is one of the inputs: writing it would destroy what is being read.
func guardDistinct(outPath string, inputs ...string) {
	if err := outguard.Distinct(os.Stat, outPath, inputs...); err != nil {
		Fatal(err.Error())
	}
}

// editMetadata rewrites source to outPath with edit applied, through the engine of the source's container.
func editMetadata(source, outPath string, edit func(*matroska.Container)) error {
	guardDistinct(outPath, source)
	if isMP4Content(source) {
		guardMP4Output(outPath)
		return mp4.EditMetadata(context.Background(), source, outPath, edit, mp4Options())
	}
	return matroska.EditMetadata(context.Background(), source, outPath, edit, matroska.Options{Progress: NewProgressBar()})
}

// guardMP4Output refuses a Matroska name for the MP4 an MP4 source's edit writes.
func guardMP4Output(outPath string) {
	switch strings.ToLower(filepath.Ext(outPath)) {
	case ".mkv", ".mka", ".mks", ".webm":
		Fatal(fmt.Sprintf("%s: the source is an MP4, so the output is an MP4 too - name it .mp4 (or convert with from-mp4 first)", outPath))
	}
}

func CmdEdit(args []string) {
	if len(args) < 4 {
		Fatal(`usage: mkvgo edit <file.mkv|.mp4> -o <out> '<json>'
       mkvgo edit <file.mkv|.mp4> -o <out> -    (read from stdin)

JSON schema:
{
  "title": "New Title",
  "tracks": [{"id": 2, "language": "fre", "language_bcp47": "fr-CA", "name": "French", "is_default": true, "is_forced": false}],
  "chapters": [{"id": 1, "title": "Intro", "start_ms": 0, "end_ms": 5000}],
  "tags": [{"target_type": "MOVIE", "simple_tags": [{"name": "TITLE", "value": "X"}]}]
}`)
	}
	source := args[0]
	var outPath, jsonStr string

	fl := newFlags("edit")
	fl.String(&outPath, "-o")
	if ops := fl.Parse(args[1:]); len(ops) > 0 {
		jsonStr = ops[len(ops)-1]
	}
	if outPath == "" {
		Fatal("missing -o <out.mkv>")
	}

	var raw []byte
	if jsonStr == "-" {
		var err error
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			FatalErr(err)
		}
	} else {
		raw = []byte(jsonStr)
	}

	patch := decodePatch(raw)
	GuardOverwrite(outPath)

	err := editMetadata(source, outPath, func(c *matroska.Container) {
		applyPatch(c, patch)
	})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("edited → %s\n", outPath)
}

func CmdEditTitle(args []string) {
	if len(args) < 4 {
		Fatal("usage: " + CmdUsage["edit-title"])
	}
	source := args[0]
	var outPath, title string

	fl := newFlags("edit-title")
	fl.String(&outPath, "-o")
	if ops := fl.Parse(args[1:]); len(ops) > 0 {
		title = ops[len(ops)-1]
	}
	if outPath == "" || title == "" {
		Fatal("usage: " + CmdUsage["edit-title"])
	}
	GuardOverwrite(outPath)

	err := editMetadata(source, outPath, func(c *matroska.Container) {
		c.Info.Title = title
	})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("title set to %q → %s\n", title, outPath)
}

func CmdEditTrack(args []string) {
	if len(args) < 5 {
		Fatal("usage: " + CmdUsage["edit-track"])
	}
	source := args[0]
	var outPath string
	var trackID uint64
	var language, name *string
	var setDefault, setForced *bool

	fl := newFlags("edit-track")
	fl.String(&outPath, "-o")
	fl.Func(func(v string) {
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid track ID %q", v))
		}
		trackID = id
	}, "-t")
	fl.Func(func(v string) { language = &v }, "-lang")
	fl.Func(func(v string) { name = &v }, "-name")
	var isDefault, noDefault, isForced, noForced bool
	fl.Do(func() { isDefault = true; setDefault = &isDefault }, "-default")
	fl.Do(func() { noDefault = false; setDefault = &noDefault }, "-no-default")
	fl.Do(func() { isForced = true; setForced = &isForced }, "-forced")
	fl.Do(func() { noForced = false; setForced = &noForced }, "-no-forced")
	fl.Parse(args[1:])
	if outPath == "" || trackID == 0 {
		Fatal("usage: " + CmdUsage["edit-track"])
	}
	GuardOverwrite(outPath)

	err := editMetadata(source, outPath, func(c *matroska.Container) {
		for i := range c.Tracks {
			if c.Tracks[i].ID != trackID {
				continue
			}
			if language != nil && isMP4Content(source) {
				c.Tracks[i].Language, c.Tracks[i].LanguageBCP47 = mp4.LanguageFields(*language)
			} else if language != nil {
				c.Tracks[i].Language, c.Tracks[i].LanguageBCP47 = lang.Set(c.Tracks[i].Language, c.Tracks[i].LanguageBCP47, *language)
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
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("edited track %d → %s\n", trackID, outPath)
}

func CmdEditInPlace(args []string) {
	if len(args) < 2 {
		Fatal(`usage: mkvgo edit-inplace <file.mkv|.mp4> '<json>'
       mkvgo edit-inplace <file.mkv|.mp4> -    (read from stdin)

Edits metadata in-place without rewriting the file. Instant on large files.
MKV: fails if the new metadata outgrows the room reserved for it.
MP4: appends a new moov and retires the old one (crash-ordered).`)
	}
	source := args[0]
	jsonStr := args[1]

	var raw []byte
	if jsonStr == "-" {
		var err error
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			FatalErr(err)
		}
	} else {
		raw = []byte(jsonStr)
	}

	patch := decodePatch(raw)

	edit := func(c *matroska.Container) { applyPatch(c, patch) }
	var err error
	if isMP4Content(source) {
		err = mp4.EditInPlace(context.Background(), source, edit)
	} else {
		err = matroska.EditInPlace(context.Background(), source, edit)
	}
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("edited in-place → %s\n", source)
}
