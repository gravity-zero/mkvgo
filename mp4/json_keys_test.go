package mp4

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestJSONKeysSnakeCase: every key the JSON of a Container (info --json, the
// wasm probe) and of a dropped track (dropped_tracks[]) carries is snake_case.
func TestJSONKeysSnakeCase(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
	seen := map[reflect.Type]bool{}
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				walk(f.Type, path)
				continue
			}
			if name == "" {
				name = f.Name
			}
			if !snake.MatchString(name) {
				t.Errorf("%s.%s: JSON key %q is not snake_case", path, f.Name, name)
			}
			walk(f.Type, path+"."+name)
		}
	}
	walk(reflect.TypeFor[mkv.Container](), "Container")
	walk(reflect.TypeFor[DroppedTrack](), "DroppedTrack")
}
