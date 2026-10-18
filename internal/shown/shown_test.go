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
	} {
		if got := Path(in); got != want {
			t.Errorf("Path(%q) = %q, want %q", in, got, want)
		}
	}
}
