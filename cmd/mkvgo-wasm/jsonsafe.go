package main

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// maxSafeInteger is the largest integer a JavaScript number holds exactly (2^53 - 1).
const maxSafeInteger = 1<<53 - 1

// jsSafeJSON re-encodes JSON for JSON.parse: every UID becomes a string, as does any integer beyond 2^53 - 1.
func jsSafeJSON(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(jsSafeValue(v, ""))
}

// jsSafeValue rewrites v in place; parent names the array or object key that holds it.
func jsSafeValue(v any, parent string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if n, ok := e.(json.Number); ok && isUIDKey(parent, k) {
				x[k] = string(n)
				continue
			}
			x[k] = jsSafeValue(e, k)
		}
	case []any:
		for i, e := range x {
			x[i] = jsSafeValue(e, parent)
		}
	case json.Number:
		if !isInteger(string(x)) {
			return x
		}
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil && i >= -maxSafeInteger && i <= maxSafeInteger {
			return x
		}
		return string(x)
	}
	return v
}

// isUIDKey reports whether key under parent holds a Matroska UID (TrackUID, ChapterUID, FileUID, a tag's target UID).
func isUIDKey(parent, key string) bool {
	switch key {
	case "uid", "target_id":
		return true
	case "id":
		return parent == "chapters" || parent == "sub_chapters" || parent == "attachments"
	}
	return false
}

// isInteger reports whether a JSON number literal has no fraction or exponent.
func isInteger(s string) bool {
	return !bytes.ContainsAny([]byte(s), ".eE")
}
