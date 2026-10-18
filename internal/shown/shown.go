// Package shown renders a source path for a message: a URL loses its query and fragment, where a signature lives.
package shown

import "strings"

// Path returns p as an error may show it: a URL is cut at its query or fragment ("?..." in their place), any other
// path is returned whole, since a '?' is a legal file name character.
func Path(p string) string {
	if !strings.Contains(p, "://") {
		return p
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		return p[:i] + "?..."
	}
	return p
}
