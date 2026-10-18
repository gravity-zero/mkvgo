package outguard

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDistinct pins the one rule every writer applies: an output naming one of its inputs is refused, by path or
// by file, and nothing else is.
func TestDistinct(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "a.mp4")
	link := filepath.Join(dir, "hard.mp4")
	other := filepath.Join(dir, "b.mp4")
	for _, p := range []string{in, other} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(in, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, dst string
		inputs    []string
		refused   bool
	}{
		{"same path", in, []string{in}, true},
		{"unclean path", filepath.Join(dir, "sub", "..", "a.mp4"), []string{in}, true},
		{"hard link", link, []string{in}, true},
		{"another file", other, []string{in}, false},
		{"output not there yet", filepath.Join(dir, "new.mp4"), []string{in}, false},
		{"empty input skipped", other, []string{"", in}, false},
		{"second input", in, []string{other, in}, true},
	}
	for _, c := range cases {
		err := Distinct(os.Stat, c.dst, c.inputs...)
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, refused want %v", c.name, err, c.refused)
		}
	}
	if err := Distinct(os.Stat, in, in); err == nil || err.Error() != in+" is also an input: write the output to another file" {
		t.Errorf("message = %v", err)
	}
}
