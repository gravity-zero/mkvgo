package ops

import (
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// writeStartLike opens mw with the EBML header the source declared, so a
// rewrite never silently turns a WebM file into a Matroska one: a "webm"
// source keeps its DocType (at its declared DocTypeVersion, raised when the
// tracks need more - AV1 is a version-4 feature), any other declaration is
// written as Matroska, as before.
//
// added names the tracks the operation introduces that the source did not
// carry. A WebM declaration only stays true while every track fits the WebM
// codec profile, so an added track outside it (an SRT or ASS subtitle, a
// track pulled from an MKV) makes the result plain Matroska - the file
// changed profile, and saying so beats a declaration the player will not
// trust. Tracks the source already carried are not audited: a file that
// declared webm around an off-profile codec keeps the declaration it came
// with, the rewrite did not add the mismatch.
func writeStartLike(mw *writer.MKVWriter, src *mkv.Container, tracks []mkv.Track, added ...mkv.Track) error {
	if !src.IsWebM() {
		return mw.WriteStart()
	}
	for _, t := range added {
		if !mkv.IsWebMCodec(t.Codec) {
			return mw.WriteStart()
		}
	}
	version := mkv.WebMDocTypeVersion(&mkv.Container{Tracks: tracks})
	if src.DocTypeVersion > version {
		version = src.DocTypeVersion
	}
	return mw.WriteStartWebM(version)
}

// joinedDocTypeSource is the source whose declaration a Join output follows:
// the first one, unless a later source declares something else - then the
// join spans profiles and is written as Matroska (a synthetic source with an
// empty DocType, which writeStartLike maps to Matroska).
func joinedDocTypeSource(conts []*mkv.Container) *mkv.Container {
	first := conts[0]
	for _, c := range conts[1:] {
		if c.DocType != first.DocType {
			return &mkv.Container{}
		}
	}
	return first
}
