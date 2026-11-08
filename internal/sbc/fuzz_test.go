package sbc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FuzzDecode: arbitrary bytes must never panic or over-consume the input.
func FuzzDecode(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.sbc")
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if len(data) > 1200 {
			data = data[:1200]
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDecoder()
		pcm := make([]int16, 256)
		for len(data) > 0 {
			c, n, _, err := d.Decode(data, pcm)
			if errors.Is(err, ErrCRC) && c > 0 {
				if c > len(data) {
					t.Fatalf("bad-CRC frame consumed %d of %d", c, len(data))
				}
				// The media path skips a bad-CRC frame and keeps decoding.
				d.Reset()
				data = data[c:]
				continue
			}
			if err != nil {
				return
			}
			if c <= 0 || c > len(data) || n > len(pcm) {
				t.Fatalf("consumed %d n %d of %d", c, n, len(data))
			}
			data = data[c:]
		}
	})
}
