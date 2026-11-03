package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// nullPaths lists the paths of v holding null.
func nullPaths(v any, path string, out *[]string) {
	switch x := v.(type) {
	case nil:
		*out = append(*out, path)
	case map[string]any:
		for k, e := range x {
			nullPaths(e, path+"."+k, out)
		}
	case []any:
		for i, e := range x {
			nullPaths(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// TestJSONReportsCarryNoNull: the JSON reports carry an empty list as [] and
// leave an optional section out; none holds null, so a jq `.x[]` or a JS
// `.map` never meets one. Run on a whole file and on one cut short.
func TestJSONReportsCarryNoNull(t *testing.T) {
	whole, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sources := map[string][]byte{"whole": whole, "cut": whole[:len(whole)*6/10]}
	JsonOutput = true
	t.Cleanup(func() { JsonOutput = false })
	for name, data := range sources {
		src := filepath.Join(dir, name+".mkv")
		if err := os.WriteFile(src, data, 0o644); err != nil {
			t.Fatal(err)
		}
		commands := map[string]func(){
			"info":              func() { CmdInfo(src) },
			"tracks":            func() { CmdTracks(src) },
			"chapters":          func() { CmdChapters(src) },
			"attachments":       func() { CmdAttachments(src) },
			"tags":              func() { CmdTags(src) },
			"probe":             func() { CmdProbe(src) },
			"keyframes":         func() { CmdKeyframes(src) },
			"validate":          func() { CmdValidate([]string{src}) },
			"compare":           func() { CmdCompare([]string{src, src}) },
			"analyze":           func() { CmdAnalyze([]string{src}) },
			"playability":       func() { CmdPlayability([]string{src}) },
			"track-ends":        func() { CmdTrackEnds([]string{src}) },
			"cue-health":        func() { CmdCueHealth([]string{src}) },
			"diagnose":          func() { CmdDiagnose([]string{src}) },
			"salvage --dry-run": func() { CmdSalvage([]string{"--dry-run", src}) },
		}
		for cmd, run := range commands {
			out := captureStdout(t, func() { exitStatus(t, run) })
			if len(out) == 0 {
				continue // refused: the error goes to stderr
			}
			var v any
			if err := json.Unmarshal(out, &v); err != nil {
				t.Errorf("%s on the %s file: not one JSON value: %v", cmd, name, err)
				continue
			}
			var nulls []string
			nullPaths(v, "", &nulls)
			sort.Strings(nulls)
			for _, p := range nulls {
				t.Errorf("%s on the %s file: %s is null", cmd, name, p)
			}
		}
	}
}
