// Package lang holds the language rules every engine applies: how a language a user gives splits into a container's
// 3-letter code and a BCP-47 tag, and when a language set on a track drops the tag that would take precedence.
package lang

import "strings"

// Style is the ISO 639-2 form a container states: Terminology for ISO-BMFF's mdhd, Bibliographic for Matroska.
type Style int

const (
	Terminology Style = iota
	Bibliographic
)

// Fields splits a language as a user gives it into the 3-letter code of style (ISO 639-2 for a tag's 2-letter
// language, a 3-letter code kept as given) and, for a BCP-47 tag, the tag; "und" when no code can be told.
func Fields(tag string, style Style) (code, bcp47 string) {
	if len(tag) == 3 && isLowerAlpha(tag) {
		return tag, ""
	}
	primary, _, _ := strings.Cut(tag, "-")
	primary = strings.ToLower(primary)
	if c, ok := iso6391To6392T[primary]; ok {
		primary = c
		if b, ok := terminologyToBibliographic[c]; ok && style == Bibliographic {
			primary = b
		}
	}
	if len(primary) != 3 || !isLowerAlpha(primary) {
		primary = "und"
	}
	return primary, tag
}

// Primary is a language's ISO 639-2 bibliographic code whatever form it comes in (a 2 or 3-letter code, either 639-2
// form, a BCP-47 tag), "und" when none can be told: two tracks state the same language when their Primary match.
func Primary(tag string) string {
	p, _, _ := strings.Cut(strings.ToLower(tag), "-")
	if c, ok := iso6391To6392T[p]; ok {
		p = c
	}
	if b, ok := terminologyToBibliographic[p]; ok {
		p = b
	}
	if len(p) != 3 || !isLowerAlpha(p) {
		return "und"
	}
	return p
}

// Set is the language and tag a Matroska track keeps when newLang is set on it: a new one is split into its code and
// BCP-47 tag (Fields), dropping the old tag; one empty or unchanged (merge passes every track's own) keeps both.
func Set(oldLang, oldTag, newLang string) (language, bcp47 string) {
	if newLang == "" || newLang == oldLang {
		return oldLang, oldTag
	}
	return Fields(newLang, Bibliographic)
}

// Edited is the tag a track keeps after an edit: one the edit left while changing the language goes.
func Edited(beforeLang, beforeTag, afterLang, afterTag string) string {
	if afterLang != beforeLang && afterTag == beforeTag {
		return ""
	}
	return afterTag
}

// isLowerAlpha reports whether s is only lowercase ASCII letters.
func isLowerAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// terminologyToBibliographic holds the ISO 639-2 codes whose bibliographic form differs from the terminology one.
var terminologyToBibliographic = map[string]string{
	"sqi": "alb", "hye": "arm", "eus": "baq", "mya": "bur", "zho": "chi", "ces": "cze", "nld": "dut", "fra": "fre",
	"kat": "geo", "deu": "ger", "ell": "gre", "isl": "ice", "mkd": "mac", "mri": "mao", "msa": "may", "fas": "per",
	"ron": "rum", "slk": "slo", "bod": "tib", "cym": "wel",
}

// iso6391To6392T maps the two-letter ISO 639-1 codes to the three-letter ISO 639-2/T ones.
var iso6391To6392T = map[string]string{
	"aa": "aar", "ab": "abk", "ae": "ave", "af": "afr", "ak": "aka", "am": "amh", "an": "arg", "ar": "ara",
	"as": "asm", "av": "ava", "ay": "aym", "az": "aze", "ba": "bak", "be": "bel", "bg": "bul", "bi": "bis",
	"bm": "bam", "bn": "ben", "bo": "bod", "br": "bre", "bs": "bos", "ca": "cat", "ce": "che", "ch": "cha",
	"co": "cos", "cr": "cre", "cs": "ces", "cu": "chu", "cv": "chv", "cy": "cym", "da": "dan", "de": "deu",
	"dv": "div", "dz": "dzo", "ee": "ewe", "el": "ell", "en": "eng", "eo": "epo", "es": "spa", "et": "est",
	"eu": "eus", "fa": "fas", "ff": "ful", "fi": "fin", "fj": "fij", "fo": "fao", "fr": "fra", "fy": "fry",
	"ga": "gle", "gd": "gla", "gl": "glg", "gn": "grn", "gu": "guj", "gv": "glv", "ha": "hau", "he": "heb",
	"hi": "hin", "ho": "hmo", "hr": "hrv", "ht": "hat", "hu": "hun", "hy": "hye", "hz": "her", "ia": "ina",
	"id": "ind", "ie": "ile", "ig": "ibo", "ii": "iii", "ik": "ipk", "io": "ido", "is": "isl", "it": "ita",
	"iu": "iku", "ja": "jpn", "jv": "jav", "ka": "kat", "kg": "kon", "ki": "kik", "kj": "kua", "kk": "kaz",
	"kl": "kal", "km": "khm", "kn": "kan", "ko": "kor", "kr": "kau", "ks": "kas", "ku": "kur", "kv": "kom",
	"kw": "cor", "ky": "kir", "la": "lat", "lb": "ltz", "lg": "lug", "li": "lim", "ln": "lin", "lo": "lao",
	"lt": "lit", "lu": "lub", "lv": "lav", "mg": "mlg", "mh": "mah", "mi": "mri", "mk": "mkd", "ml": "mal",
	"mn": "mon", "mr": "mar", "ms": "msa", "mt": "mlt", "my": "mya", "na": "nau", "nb": "nob", "nd": "nde",
	"ne": "nep", "ng": "ndo", "nl": "nld", "nn": "nno", "no": "nor", "nr": "nbl", "nv": "nav", "ny": "nya",
	"oc": "oci", "oj": "oji", "om": "orm", "or": "ori", "os": "oss", "pa": "pan", "pi": "pli", "pl": "pol",
	"ps": "pus", "pt": "por", "qu": "que", "rm": "roh", "rn": "run", "ro": "ron", "ru": "rus", "rw": "kin",
	"sa": "san", "sc": "srd", "sd": "snd", "se": "sme", "sg": "sag", "si": "sin", "sk": "slk", "sl": "slv",
	"sm": "smo", "sn": "sna", "so": "som", "sq": "sqi", "sr": "srp", "ss": "ssw", "st": "sot", "su": "sun",
	"sv": "swe", "sw": "swa", "ta": "tam", "te": "tel", "tg": "tgk", "th": "tha", "ti": "tir", "tk": "tuk",
	"tl": "tgl", "tn": "tsn", "to": "ton", "tr": "tur", "ts": "tso", "tt": "tat", "tw": "twi", "ty": "tah",
	"ug": "uig", "uk": "ukr", "ur": "urd", "uz": "uzb", "ve": "ven", "vi": "vie", "vo": "vol", "wa": "wln",
	"wo": "wol", "xh": "xho", "yi": "yid", "yo": "yor", "za": "zha", "zh": "zho", "zu": "zul",
}
