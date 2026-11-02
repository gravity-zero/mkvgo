package ops

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestFindingKindsComplete: every Kind the triage emits (ops and mp4) is one
// of mkv.FindingKinds, written through its constant, and every documented
// list of kinds - the Go doc, the CLI and library guides, the TypeScript
// type - names them all.
func TestFindingKindsComplete(t *testing.T) {
	kinds := mkv.FindingKinds()
	literal := regexp.MustCompile(`Kind:\s*"`)
	for _, src := range []string{"diagnose.go", "../../mp4/diagnose.go"} {
		code, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if loc := literal.FindIndex(code); loc != nil {
			t.Errorf("%s: a Kind written as a string literal at byte %d: use its mkv.Finding* constant", src, loc[0])
		}
	}
	root := filepath.Join("..", "..")
	for _, doc := range []string{"mkv/reports.go", "docs/cli.md", "docs/library.md", "web/mkvgo.ts"} {
		text, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range kinds {
			if !strings.Contains(string(text), `"`+k+`"`) && !strings.Contains(string(text), "`"+k+"`") && !strings.Contains(string(text), "'"+k+"'") {
				t.Errorf("%s does not name the finding kind %q", doc, k)
			}
		}
	}
	if len(kinds) != len(slices.Compact(slices.Sorted(slices.Values(kinds)))) {
		t.Errorf("FindingKinds repeats a kind: %v", kinds)
	}
}

// TestFindingScope: the findings about where the file is defective say
// where - its head, its body, its tail - so "damaged" reads on its own.
func TestFindingScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := readAll(t, salvageFixture(t, dir, "src.mkv", 8))
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data[:offsets[5]])
	junk := filepath.Join(dir, "junk.mkv")
	writeAll(t, junk, append(append([]byte{}, data...), make([]byte, 512)...))
	mid := append([]byte{}, data...)
	for i := offsets[2] + 2; i < offsets[3]-2; i++ {
		mid[i] = 0xEE
	}
	body := filepath.Join(dir, "body.mkv")
	writeAll(t, body, mid)
	for _, c := range []struct {
		path, kind, scope string
		opts              []mkv.Options
	}{
		{cut, mkv.FindingTruncated, mkv.ScopeTail, nil},
		{junk, mkv.FindingTrailingJunk, mkv.ScopeTail, nil},
		{body, mkv.FindingDamaged, mkv.ScopeBody, []mkv.Options{{DeepVerify: true}}},
	} {
		d, err := Diagnose(ctx, c.path, c.opts...)
		if err != nil {
			t.Fatal(err)
		}
		f := hasFinding(d, c.kind)
		if f == nil {
			t.Errorf("%s: no %s finding in %v", filepath.Base(c.path), c.kind, findingKinds(d))
			continue
		}
		if f.Scope != c.scope {
			t.Errorf("%s: %s scope %q, want %q", filepath.Base(c.path), c.kind, f.Scope, c.scope)
		}
	}
}
