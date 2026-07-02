package commands

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdAddAttachment adds a file (font, cover art, …) as a Matroska attachment.
// The MIME type is sniffed from the content when -mime is not given.
func CmdAddAttachment(args []string) {
	usage := CmdUsage["add-attachment"]
	source := ""
	var outPath, attPath, name, mime string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i < len(args) {
				outPath = args[i]
			}
		case "-name":
			i++
			if i < len(args) {
				name = args[i]
			}
		case "-mime":
			i++
			if i < len(args) {
				mime = args[i]
			}
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 2 || outPath == "" {
		Fatal("usage: " + usage)
	}
	source, attPath = rest[0], rest[1]
	GuardOverwrite(outPath)

	data, err := os.ReadFile(attPath)
	if err != nil {
		Fatal(err.Error())
	}
	if len(data) == 0 {
		Fatal(fmt.Sprintf("%s is empty", attPath))
	}
	if name == "" {
		name = filepath.Base(attPath)
	}
	if mime == "" {
		mime = sniffMIME(name, data)
	}

	err = matroska.AddAttachment(context.Background(), source, outPath, matroska.Attachment{
		Name: name, MIMEType: mime, Data: data,
	}, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("attached %s (%s, %s) → %s\n", name, mime, FormatBytes(int64(len(data))), outPath)
}

// CmdRemoveAttachment removes an attachment by ID or by exact name.
func CmdRemoveAttachment(args []string) {
	usage := CmdUsage["remove-attachment"]
	var outPath string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i < len(args) {
				outPath = args[i]
			}
			continue
		}
		rejectFlagArg(args[i])
		rest = append(rest, args[i])
	}
	if len(rest) < 2 || outPath == "" {
		Fatal("usage: " + usage)
	}
	source, target := rest[0], rest[1]
	GuardOverwrite(outPath)

	// RemoveAttachment resolves the target BEFORE writing anything, so a bad
	// ID/name fails without leaving an output file behind.
	err := matroska.RemoveAttachment(context.Background(), source, outPath, target,
		matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(fmt.Sprintf("%v (use `mkvgo attachments` to list them)", err))
	}
	fmt.Printf("removed attachment %s → %s\n", target, outPath)
}

// sniffMIME detects an attachment's MIME type from its magic bytes (the types
// commonly attached to Matroska: images, fonts), with an extension fallback.
// Deliberately local: importing net/http for DetectContentType would drag the
// whole network stack (and its cgo resolver) into the static binary.
func sniffMIME(name string, data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(data, []byte{0x00, 0x01, 0x00, 0x00}):
		return "font/ttf"
	case bytes.HasPrefix(data, []byte("OTTO")):
		return "font/otf"
	case bytes.HasPrefix(data, []byte("wOFF")):
		return "font/woff"
	case bytes.HasPrefix(data, []byte("wOF2")):
		return "font/woff2"
	case bytes.HasPrefix(data, []byte("%PDF")):
		return "application/pdf"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".srt", ".txt", ".nfo":
		return "text/plain"
	case ".xml":
		return "text/xml"
	}
	return "application/octet-stream"
}
