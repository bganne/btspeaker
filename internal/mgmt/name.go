package mgmt

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidName is returned by ValidName.
var ErrInvalidName = errors.New("invalid name (1..248 bytes, no control or bidi format characters)")

// ValidName trims name and checks it is a usable adapter name: 1..248 bytes
// of valid UTF-8 that SanitizeName leaves unchanged.
func ValidName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 248 || !utf8.ValidString(name) || SanitizeName(name) != name {
		return "", ErrInvalidName
	}
	return name, nil
}

// SanitizeName makes a remote-supplied device name safe for logs and state.json:
// invalid UTF-8, control and format characters (bidi overrides that can spoof a
// name in the web UI) are dropped. A name with no visible rune left becomes "",
// so callers fall back to the address instead of showing a blank name.
func SanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || r == 0x2028 || r == 0x2029 || unicode.IsControl(r) ||
			(unicode.Is(unicode.Cf, r) && r != 0x200C && r != 0x200D) { // Cf: bidi overrides etc.; keep ZWNJ/ZWJ
			return -1
		}
		return r
	}, s)
	// Names are at most 248 bytes (the mgmt/HCI limit): cap at a rune boundary.
	if len(s) > 248 {
		cut := 248
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) && r != 0x200C && r != 0x200D }) < 0 {
		return ""
	}
	return s
}
