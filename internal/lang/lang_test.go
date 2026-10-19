package lang

import "testing"

// TestFields pins how a language a user gives splits into a container's 3-letter code and its BCP-47 tag.
func TestFields(t *testing.T) {
	cases := []struct {
		in        string
		style     Style
		code, bcp string
	}{
		{"fre", Terminology, "fre", ""}, // a 3-letter code is the caller's, never rewritten
		{"fr", Terminology, "fra", "fr"},
		{"fr", Bibliographic, "fre", "fr"},
		{"pt-BR", Terminology, "por", "pt-BR"},
		{"de-CH", Bibliographic, "ger", "de-CH"},
		{"zh-Hant", Bibliographic, "chi", "zh-Hant"},
		{"en", Bibliographic, "eng", "en"},
		{"x1", Terminology, "und", "x1"},
	}
	for _, c := range cases {
		if code, bcp := Fields(c.in, c.style); code != c.code || bcp != c.bcp {
			t.Errorf("Fields(%q, %v) = %q, %q; want %q, %q", c.in, c.style, code, bcp, c.code, c.bcp)
		}
	}
}

// TestSet pins the rule a language setter applies: a language that changes drops the tag that would take
// precedence over it, one passed through unchanged keeps it.
func TestSet(t *testing.T) {
	cases := []struct{ oldLang, oldTag, newLang, lang, tag string }{
		{"eng", "en", "fre", "fre", ""},
		{"por", "pt-BR", "por", "por", "pt-BR"},
		{"fre", "fr-CA", "", "fre", "fr-CA"},
		{"eng", "", "pt-BR", "por", "pt-BR"},
		{"eng", "", "de", "ger", "de"},
	}
	for _, c := range cases {
		if lang, tag := Set(c.oldLang, c.oldTag, c.newLang); lang != c.lang || tag != c.tag {
			t.Errorf("Set(%q, %q, %q) = %q, %q; want %q, %q", c.oldLang, c.oldTag, c.newLang, lang, tag, c.lang, c.tag)
		}
	}
}

// TestEdited pins what an edit callback leaves: a tag it did not touch goes when it changed the language.
func TestEdited(t *testing.T) {
	cases := []struct{ beforeLang, beforeTag, afterLang, afterTag, want string }{
		{"eng", "en", "jpn", "en", ""},
		{"eng", "en", "jpn", "ja", "ja"},
		{"eng", "en", "eng", "en", "en"},
		{"eng", "en", "eng", "en-GB", "en-GB"},
	}
	for _, c := range cases {
		if got := Edited(c.beforeLang, c.beforeTag, c.afterLang, c.afterTag); got != c.want {
			t.Errorf("Edited(%q, %q, %q, %q) = %q; want %q", c.beforeLang, c.beforeTag, c.afterLang, c.afterTag, got, c.want)
		}
	}
}
