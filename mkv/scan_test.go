package mkv

import (
	"encoding/json"
	"testing"
)

func TestFieldOrderName(t *testing.T) {
	cases := map[uint64]string{0: "progressive", 1: "tt", 2: "", 6: "bb", 9: "bt", 14: "tb", 3: "", 99: ""}
	for v, want := range cases {
		if got := FieldOrderName(v); got != want {
			t.Errorf("FieldOrderName(%d) = %q, want %q", v, got, want)
		}
	}
}

func TestScanTypeOf(t *testing.T) {
	cases := map[string]string{
		"progressive": "progressive",
		"tt":          "interlaced", "bb": "interlaced", "tb": "interlaced", "bt": "interlaced",
		"interlaced": "interlaced", // legacy value: it named the scan type
		"":           "",
		"bogus":      "",
	}
	for fo, want := range cases {
		if got := ScanTypeOf(fo); got != want {
			t.Errorf("ScanTypeOf(%q) = %q, want %q", fo, got, want)
		}
	}
}

// TestTrackUnmarshalJSONLegacyFieldOrder: JSON persisted before ScanType existed
// carried "interlaced" in field_order. It reads back as ScanType "interlaced"
// with no order - none was ever known - and the other legacy shapes resolve too.
func TestTrackUnmarshalJSONLegacyFieldOrder(t *testing.T) {
	cases := []struct {
		name, in   string
		scan, want string
	}{
		{"legacy interlaced", `{"type":"video","field_order":"interlaced"}`, "interlaced", ""},
		{"legacy progressive", `{"type":"video","field_order":"progressive"}`, "progressive", "progressive"},
		{"order only", `{"type":"video","field_order":"tt"}`, "interlaced", "tt"},
		{"scan only, interlaced", `{"type":"video","scan_type":"interlaced"}`, "interlaced", ""},
		{"scan only, progressive", `{"type":"video","scan_type":"progressive"}`, "progressive", "progressive"},
		{"both", `{"type":"video","scan_type":"interlaced","field_order":"bb"}`, "interlaced", "bb"},
		{"neither", `{"type":"video"}`, "", ""},
		{"unknown order passes through", `{"type":"video","field_order":"weird"}`, "", "weird"},
	}
	for _, c := range cases {
		var tr Track
		if err := json.Unmarshal([]byte(c.in), &tr); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if tr.Type != VideoTrack {
			t.Errorf("%s: the plain fields must still decode, Type = %q", c.name, tr.Type)
		}
		if tr.ScanType != c.scan || tr.FieldOrder != c.want {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want %q/%q", c.name, tr.ScanType, tr.FieldOrder, c.scan, c.want)
		}
	}
	var tr Track
	if err := json.Unmarshal([]byte(`{"type":`), &tr); err == nil {
		t.Error("malformed JSON must still fail")
	}
}

// TestTrackJSONRoundTrip: what a current Track writes reads back unchanged,
// through the custom UnmarshalJSON, nested pointers included.
func TestTrackJSONRoundTrip(t *testing.T) {
	w := uint32(1920)
	in := Track{ID: 1, Type: VideoTrack, Codec: "h264", Width: &w, ScanType: "interlaced", FieldOrder: "tb", Name: "main"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Track
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 1 || out.Codec != "h264" || out.Width == nil || *out.Width != 1920 || out.Name != "main" {
		t.Errorf("round trip lost plain fields: %+v", out)
	}
	if out.ScanType != "interlaced" || out.FieldOrder != "tb" {
		t.Errorf("ScanType/FieldOrder = %q/%q, want interlaced/tb", out.ScanType, out.FieldOrder)
	}
}
