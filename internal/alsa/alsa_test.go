package alsa

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestStructSizes(t *testing.T) {
	var hw, sw, xf uintptr
	switch runtime.GOARCH {
	case "amd64", "arm64":
		hw, sw, xf = 608, 136, 24
	case "arm", "386":
		hw, sw, xf = 604, 104, 12
	default:
		t.Skip("unknown arch")
	}
	if got := unsafe.Sizeof(hwParams{}); got != hw {
		t.Errorf("hwParams = %d want %d", got, hw)
	}
	if got := unsafe.Sizeof(swParams{}); got != sw {
		t.Errorf("swParams = %d want %d", got, sw)
	}
	if got := unsafe.Sizeof(xferi{}); got != xf {
		t.Errorf("xferi = %d want %d", got, xf)
	}
	if unsafe.Sizeof(mask{}) != 32 || unsafe.Sizeof(interval{}) != 12 {
		t.Error("mask/interval size")
	}
}

func TestIoctlNumbersAmd64(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip()
	}
	for name, c := range map[string][2]uintptr{
		"PVERSION":     {ioctlPVersion, 0x80044100},
		"HW_REFINE":    {ioctlHWRefine, 0xc2604110},
		"HW_PARAMS":    {ioctlHWParams, 0xc2604111},
		"SW_PARAMS":    {ioctlSWParams, 0xc0884113},
		"PREPARE":      {ioctlPrepare, 0x4140},
		"DRAIN":        {ioctlDrain, 0x4144},
		"WRITEI_FRAME": {ioctlWriteFrames, 0x40184150},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %#x want %#x", name, c[0], c[1])
		}
	}
}

func TestMaskInterval(t *testing.T) {
	var m mask
	m.any()
	if !m.test(10) || !m.test(255) {
		t.Error("any")
	}
	m.only(33)
	if !m.test(33) || m.test(32) || m.test(2) || m.bits[1] != 2 {
		t.Errorf("only: %v", m.bits)
	}
	var i interval
	i.any()
	if i.empty() || i.max != ^uint32(0) {
		t.Error("any interval")
	}
	i.set(5, 3)
	if !i.empty() {
		t.Error("min>max must be empty")
	}
	i.set(44100, 44100)
	if i.empty() || i.flags&ivlInteger == 0 {
		t.Error("set")
	}
	i.flags |= ivlEmpty
	if !i.empty() {
		t.Error("empty flag")
	}
	var h hwParams
	h.setAny()
	if h.ivl(paramRate) != &h.intervals[3] || h.ivl(paramBufferSz) != &h.intervals[9] {
		t.Error("ivl indexing")
	}
}

func fakeProc(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	write := func(p, s string) {
		p = filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cards", " 0 [Generic        ]: HDA-Intel - HD-Audio Generic\n                      HD-Audio Generic at 0xfd3c8000 irq 99\n 2 [Spdif          ]: USB-Audio - Some SPDIF\n                      Vendor Some SPDIF at usb\n")
	write("card0/pcm3p/info", "card: 0\ndevice: 3\nid: HDMI 0\nname: HDMI 0\n")
	write("card0/pcm0c/info", "name: cap\n")
	write("card2/pcm0p/info", "name: USB Audio\n")
	oldP, oldD := procRoot, devRoot
	procRoot, devRoot = root, "/dev/snd"
	t.Cleanup(func() { procRoot, devRoot = oldP, oldD })
}

func TestParseDevice(t *testing.T) {
	fakeProc(t)
	for in, want := range map[string]string{
		"hw:0,3":                "/dev/snd/pcmC0D3p",
		"hw:2,0":                "/dev/snd/pcmC2D0p",
		"hw:Spdif,1":            "/dev/snd/pcmC2D1p",
		"hw:CARD=Generic,DEV=7": "/dev/snd/pcmC0D7p",
		"hw:CARD=Spdif":         "/dev/snd/pcmC2D0p",
		"hw:5,0":                "/dev/snd/pcmC5D0p",
	} {
		got, err := ParseDevice(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"default", "hw:Nope,0", "hw:", "hw:0,x", "plughw:0,0"} {
		if _, err := ParseDevice(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestListPlayback(t *testing.T) {
	fakeProc(t)
	l, err := ListPlayback()
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[0].Device != "hw:0,3" || l[0].Name != "Generic" || l[1].Device != "hw:2,0" || l[1].Name != "Spdif" {
		t.Fatalf("%+v", l)
	}
	t.Logf("%+v", l)
}

func TestErrBusyIs(t *testing.T) {
	if !errors.Is(ErrBusy, ErrBusy) {
		t.Fatal()
	}
}

// After an xrun, Write must queue start_threshold frames of silence after PREPARE
// and before the remaining real data, then START.
func TestXrunSilenceCushion(t *testing.T) {
	type call struct {
		req    uintptr
		frames int
		zero   bool
	}
	var calls []call
	writes := 0
	old := ioctl
	defer func() { ioctl = old }()
	ioctl = func(fd int, req uintptr, arg unsafe.Pointer) error {
		c := call{req: req}
		if req == ioctlWriteFrames {
			x := (*xferi)(arg)
			b := unsafe.Slice((*byte)(x.buf), int(x.frames)*4)
			c.frames, c.zero = int(x.frames), true
			for _, v := range b {
				if v != 0 {
					c.zero = false
				}
			}
			calls = append(calls, c)
			writes++
			if writes == 1 { // first write: 2 frames then xrun
				x.result = 2
				return unix.EPIPE
			}
			x.result = x.frames
			return nil
		}
		calls = append(calls, c)
		return nil
	}
	p := &PCM{fd: 3, frameBytes: 4, startThr: 100}
	buf := make([]byte, 10*4)
	for i := range buf {
		buf[i] = 1
	}
	if err := p.Write(buf); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{ioctlWriteFrames, 10, false},
		{req: ioctlPrepare},
		{ioctlWriteFrames, 100, true},
		{ioctlWriteFrames, 8, false},
		{req: ioctlStart},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
	if p.Xruns() != 1 {
		t.Fatalf("xruns = %d", p.Xruns())
	}
}

func TestWriteXrunRecoveryCapped(t *testing.T) {
	writes := 0
	old := ioctl
	defer func() { ioctl = old }()
	ioctl = func(fd int, req uintptr, arg unsafe.Pointer) error {
		if req == ioctlWriteFrames {
			writes++
			return unix.EPIPE
		}
		return nil
	}
	p := &PCM{fd: 3, frameBytes: 4, startThr: 8}
	if err := p.Write(make([]byte, 40)); err == nil || err == errAborted {
		t.Fatalf("err = %v, want repeated xruns error", err)
	}
	// 6 failed data writes, plus 5 silence writes (each also EPIPE; best effort)
	if writes > 6+5 {
		t.Fatalf("%d writes, recovery not capped", writes)
	}
}

func TestWriteErrorAfterAbortIsErrAborted(t *testing.T) {
	old := ioctl
	defer func() { ioctl = old }()
	p := &PCM{fd: 3, frameBytes: 4}
	ioctl = func(fd int, req uintptr, arg unsafe.Pointer) error {
		p.aborted.Store(true)
		return unix.EIO
	}
	if err := p.Write(make([]byte, 40)); err != errAborted {
		t.Fatalf("err = %v, want errAborted", err)
	}
}
