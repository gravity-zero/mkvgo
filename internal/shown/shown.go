// Package shown renders a source path for a message: a URL loses its credentials, query and fragment, where a
// password or a signature lives.
package shown

import "strings"

// Path returns p as an error may show it: a URL loses any user:password@ before its host and is cut at its query or
// fragment ("?..." in their place); any other path is returned whole, since '?' and '@' are legal file name characters.
func Path(p string) string {
	scheme := strings.Index(p, "://")
	if scheme < 0 {
		return p
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i] + "?..."
	}
	host := scheme + len("://")
	end := len(p)
	if i := strings.IndexAny(p[host:], "/?"); i >= 0 {
		end = host + i
	}
	if at := strings.LastIndex(p[host:end], "@"); at >= 0 {
		p = p[:host] + p[host+at+1:]
	}
	return p
}
