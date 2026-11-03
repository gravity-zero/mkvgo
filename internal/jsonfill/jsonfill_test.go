package jsonfill

import (
	"encoding/json"
	"testing"
)

type inner struct {
	List []int `json:"list"`
}

type report struct {
	Items    []string       `json:"items"`
	Optional []string       `json:"optional,omitempty"`
	Counts   map[string]int `json:"counts"`
	Nested   *inner         `json:"nested"`
	Inners   []inner        `json:"inners"`
	Any      any            `json:"any"`
	Bytes    []byte         `json:"bytes"`
	hidden   []string
	Embedded
}

type Embedded struct {
	Deep []int `json:"deep"`
}

// TestOf: nil lists and maps come out [] and {}, at any depth, behind a
// pointer, an interface or an embedding; omitempty still omits; the value
// given is not modified, an unexported field is carried over.
func TestOf(t *testing.T) {
	r := &report{Nested: &inner{}, Inners: []inner{{}}, Any: inner{}, hidden: []string{"kept"}}
	got, err := json.Marshal(Of(r))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[],"counts":{},"nested":{"list":[]},"inners":[{"list":[]}],"any":{"list":[]},"bytes":null,"deep":[]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if c := Of(r).(*report); len(c.hidden) != 1 || c.hidden[0] != "kept" {
		t.Errorf("an unexported field was not carried over: %v", c.hidden)
	}
	if r.Items != nil || r.Nested.List != nil || r.Inners[0].List != nil {
		t.Error("the value given was modified")
	}
	if got, _ := json.Marshal(Of([]int(nil))); string(got) != "[]" {
		t.Errorf("a nil top-level list: %s, want []", got)
	}
	if Of(nil) != nil {
		t.Error("Of(nil) is not nil")
	}
}
