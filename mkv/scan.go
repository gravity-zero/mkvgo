package mkv

import "encoding/json"

// fieldOrderNames maps the Matroska FieldOrder element (0x9D) to the
// conventional field_order string. 2 (undetermined) and unknown values map to "".
var fieldOrderNames = map[uint64]string{
	0:  "progressive",
	1:  "tt", // top field first, top field stored first
	6:  "bb", // bottom field first, bottom field stored first
	9:  "bt", // bottom field first, top field stored first
	14: "tb", // top field first, bottom field stored first
}

// FieldOrderName maps a Matroska FieldOrder element value to the conventional
// field_order string ("progressive", "tt", "bb", "tb", "bt"), "" when undetermined.
func FieldOrderName(v uint64) string { return fieldOrderNames[v] }

// ScanTypeOf returns the scan type a field_order implies: "progressive" for
// "progressive", "interlaced" for the four field orders, "" when the order is
// unknown. The legacy value "interlaced" - what Track.FieldOrder carried before
// ScanType existed - also maps to "interlaced": it named the scan type, never an
// order.
func ScanTypeOf(fieldOrder string) string {
	switch fieldOrder {
	case "progressive":
		return "progressive"
	case "tt", "bb", "tb", "bt", "interlaced":
		return "interlaced"
	}
	return ""
}

// resolveScan reconciles ScanType and FieldOrder so that a consumer can read
// either without knowing the other: a known FieldOrder implies the scan type, a
// progressive scan type implies the order, and the legacy FieldOrder value
// "interlaced" moves to ScanType (there was never an order behind it).
func (t *Track) resolveScan() {
	if t.FieldOrder == "interlaced" {
		t.FieldOrder = ""
		t.ScanType = "interlaced"
	}
	if t.ScanType == "" {
		t.ScanType = ScanTypeOf(t.FieldOrder)
	}
	if t.FieldOrder == "" && t.ScanType == "progressive" {
		t.FieldOrder = "progressive"
	}
}

// UnmarshalJSON reads a Track, accepting the JSON written before scan type and
// field order were separate fields: a "field_order" of "interlaced" becomes
// ScanType "interlaced" with no FieldOrder, and a "field_order" without a
// "scan_type" fills the scan type it implies.
func (t *Track) UnmarshalJSON(b []byte) error {
	type plain Track // no methods: plain json decoding, no recursion
	if err := json.Unmarshal(b, (*plain)(t)); err != nil {
		return err
	}
	t.resolveScan()
	return nil
}
