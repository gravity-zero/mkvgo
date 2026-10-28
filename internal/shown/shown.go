// Package shown renders a source path for a message: a URL loses its credentials, query and fragment, where a
// password or a signature lives.
package shown

import (
	"net/url"
	"strings"
)

// Path returns p as an error may show it: a URL is shown as its scheme, host and path, cut at its query or fragment
// ("?..." in their place), any user:password@ left out; a URL that does not parse shows its scheme alone, since what
// follows may hold a password the parse could not set apart. Any other path is returned whole, since '?' and '@' are
// legal file name characters.
func Path(p string) string {
	scheme := strings.Index(p, "://")
	if scheme < 0 {
		return p
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return p[:scheme] + "://(a URL that does not parse)"
	}
	rest := p[scheme+len("://"):]
	path, cut := "", false
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		path = rest[i:]
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
