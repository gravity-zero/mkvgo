package commands_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// mp4Copy copies an MP4 fixture into a temp dir.
func mp4Copy(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEditPatchRefusesWhatItDoesNotKnow(t *testing.T) {
	src := mp4Copy(t, "bframes_aac.mp4")
	for _, patch := range []string{`{"bogus":1}`, `{"title":"a"} {"x":1}`, `{"title":"a"}}`, `{"title":"a"}]`, `{"tracks":[{"id":9,"language":"fre"}]}`} {
		mustFatal(t, func() { commands.CmdEditInPlace([]string{src, patch}) })
	}
	commands.CmdEditInPlace([]string{src, `{"title":"ok","tracks":[{"id":2,"language_bcp47":"pt-BR"}]}`})
}

func TestOutputThatIsAnInputIsRefused(t *testing.T) {
	src := mp4Copy(t, "bframes_aac.mp4")
	link := filepath.Join(t.TempDir(), "link.mp4")
	if err := os.Link(src, link); err != nil {
		t.Skip(err)
	}
	commands.Force = true
	defer func() { commands.Force = false }()
	mustFatal(t, func() { commands.CmdToMP4([]string{src, link}) })
	mustFatal(t, func() { commands.CmdRemoveTrack([]string{src, "-o", src, "-t", "2"}) })
	mustFatal(t, func() { commands.CmdJoin([]string{"-o", link, src, src}) })
}

func TestMixedContainersAndMissingInputsAreRefused(t *testing.T) {
	src := mp4Copy(t, "bframes_aac.mp4")
	out := filepath.Join(t.TempDir(), "o.mp4")
	mustFatal(t, func() {
		commands.CmdMerge([]string{"-o", out, src, filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv")})
	})
	mustFatal(t, func() { commands.CmdMerge([]string{"-o", out, src, "nonexistent.mp4"}) })
	mustFatal(t, func() { commands.CmdEditTitle([]string{src, "-o", filepath.Join(t.TempDir(), "o.mkv"), "x"}) })
}
