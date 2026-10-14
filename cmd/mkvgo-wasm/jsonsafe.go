package main

import "strconv"

// maxSafeInteger is the largest integer a JavaScript number holds exactly (2^53 - 1).
const maxSafeInteger = 1<<53 - 1

// jsonFrame is one open JSON container: an object or an array, and the key that holds it.
type jsonFrame struct {
	object bool
	name   string
}

// jsSafeJSON quotes, in one pass over encoding/json output, every UID and any integer beyond 2^53 - 1, for JSON.parse.
func jsSafeJSON(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+64)
	var stack []jsonFrame
	key := ""
	for i := 0; i < len(raw); {
		switch c := raw[i]; {
		case c == '"':
			j := i + 1
			for raw[j] != '"' {
				if raw[j] == '\\' {
					j++
				}
				j++
			}
			j++
			if k := j; len(stack) > 0 && stack[len(stack)-1].object && k < len(raw) && raw[k] == ':' {
				key = string(raw[i+1 : j-1])
			}
			out = append(out, raw[i:j]...)
			i = j
		case c == '{' || c == '[':
			name := ""
			if n := len(stack); n > 0 {
				if name = stack[n-1].name; stack[n-1].object {
					name = key
				}
			}
			stack = append(stack, jsonFrame{object: c == '{', name: name})
			out = append(out, c)
			i++
		case c == '}' || c == ']':
			stack = stack[:len(stack)-1]
			out = append(out, c)
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(raw) && (raw[j] == '.' || raw[j] == 'e' || raw[j] == 'E' || raw[j] == '+' || raw[j] == '-' || (raw[j] >= '0' && raw[j] <= '9')) {
				j++
			}
			num := raw[i:j]
			top := jsonFrame{}
			if n := len(stack); n > 0 {
				top = stack[n-1]
			}
			if (top.object && isUIDKey(top.name, key)) || unsafeInteger(num) {
				out = append(append(append(out, '"'), num...), '"')
			} else {
				out = append(out, num...)
			}
			i = j
		default:
			out = append(out, c)
			i++
		}
	}
	return out
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

// unsafeInteger reports whether a JSON number literal is an integer a JavaScript number cannot hold exactly.
func unsafeInteger(num []byte) bool {
	for _, c := range num {
		if c == '.' || c == 'e' || c == 'E' {
			return false
		}
	}
	if len(num) < 16 {
		return false
	}
	i, err := strconv.ParseInt(string(num), 10, 64)
	return err != nil || i > maxSafeInteger || i < -maxSafeInteger
}
