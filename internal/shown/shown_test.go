package shown

import "testing"

func TestPath(t *testing.T) {
	for in, want := range map[string]string{
		"https://h/a.mkv?X-Amz-Signature=s": "https://h/a.mkv?...",
		"https://h/a.mkv#t=1":               "https://h/a.mkv?...",
		"s3://b/k.mkv":                      "s3://b/k.mkv",
		"/media/what?.mkv":                  "/media/what?.mkv",
		"https://u:secret@h/a.mkv?sig=s":    "https://h/a.mkv?...",
		"https://u@h/a@b.mkv":               "https://h/a@b.mkv",
		// A password the authority's end cuts short ('/', '?', '#' in it) made a URL that does not parse; what was
		// shown of it was the user and the password's start.
		"https://user:pa/ss@host/x.mkv": "https://(a URL that does not parse)",
		"https://u:p?w@h/a.mkv":         "https://(a URL that does not parse)",
		"https://u:p#w@h/a.mkv":         "https://(a URL that does not parse)",
		"http://[::1]:8080/a b.mkv":     "http://[::1]:8080/a b.mkv",
		"https://h?sig=s":               "https://h?...",
	} {
		if got := Path(in); got != want {
			t.Errorf("Path(%q) = %q, want %q", in, got, want)
		}
	}
}
