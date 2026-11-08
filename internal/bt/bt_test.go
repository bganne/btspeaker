package bt

import "testing"

func TestParseAddr(t *testing.T) {
	good := map[string]Addr{
		"AA:BB:CC:DD:EE:FF": {0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		"01:aB:00:0f:10:ff": {1, 0xAB, 0, 0x0F, 0x10, 0xFF},
	}
	for s, want := range good {
		if a, err := ParseAddr(s); err != nil || a != want {
			t.Errorf("ParseAddr(%q) = %v, %v", s, a, err)
		}
	}
	for _, s := range []string{"", " 1:BB:CC:DD:EE:FF", "0x:BB:CC:DD:EE:FF", "AA:BB:CC:DD:EE:F", "AA-BB-CC-DD-EE-FF",
		"+1:BB:CC:DD:EE:FF", "AA:BB:CC:DD:EE:FG", "AA:BB:CC:DD:EE:FF0"} {
		if _, err := ParseAddr(s); err == nil {
			t.Errorf("ParseAddr(%q): want error", s)
		}
	}
}
