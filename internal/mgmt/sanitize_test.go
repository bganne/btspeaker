package mgmt

import "testing"

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Phone":               "Phone",
		"Ph\x00o\nne\x1b[0m":  "Phone[0m",
		"bad\xff\xfeutf":      "badutf",
		"Café ♪":              "Café ♪",
		"evil\u202egnp.exe":   "evilgnp.exe",
		"👨\u200d👩\u200d👧":     "👨\u200d👩\u200d👧", // ZWJ emoji sequence
		"می\u200cخواهم":       "می\u200cخواهم",   // ZWNJ
		"a\u2066b\u061cc":     "abc",
		"\u200d\u200c \u200d": "", // nothing visible
		" \u202e ":            "",
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
