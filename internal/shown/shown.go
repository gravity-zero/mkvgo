// Package shown renders a source path for a message: a URL loses its credentials, query and fragment, where a
// password or a signature lives.
package shown

import (
	"net/url"
	"strings"
)

// Path returns p as an error may show it: a URL is shown as its scheme, host and path, cut at its query or fragment
// ("?..." in their place), any user:password@ left out; a URL that does not parse, or whose path holds an '@' its
// authority does not, shows its scheme alone, since what follows may hold a password the parse could not set apart.
// Any other path is returned whole, since '?' and '@' are legal file name characters.
func Path(p string) string {
	scheme := strings.Index(p, "://")
	if scheme < 0 || !isScheme(p[:scheme]) || strings.HasPrefix(p[scheme:], ":///") {
		return p // a file path, or a URL with no authority to carry credentials (file:///...)
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return unparsable(p, scheme)
	}
	rest := p[scheme+len("://"):]
	authority, path, cut := rest, "", false
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if !strings.Contains(authority, "@") && strings.Contains(path, "@") {
		// "user:1234/x@host" parses as host "user" and port 1234: an '@' past an authority without one may close
		// credentials the parse took for a host and a path.
		return unparsable(p, scheme)
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path, cut = path[:i], true
	}
	shown := p[:scheme] + "://" + u.Host + path // the path as written, not as url escapes it
	if cut {
		shown += "?..."
	}
	return shown
}

func unparsable(p string, scheme int) string { return p[:scheme] + "://(a URL that does not parse)" }

// isScheme reports whether s is a URL scheme: a letter, then letters, digits, '+', '-' or '.'.
func isScheme(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}
