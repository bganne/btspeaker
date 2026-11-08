package hci

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"btspeaker/internal/bt"

	"golang.org/x/sys/unix"
)

func TestAddrFromWire(t *testing.T) {
	// 11:22:33:44:55:66 is stored by the kernel as 66 55 44 33 22 11.
	got := addrFromWire([6]byte{0x66, 0x55, 0x44, 0x33, 0x22, 0x11})
	if got != (bt.Addr{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}) || got.String() != "11:22:33:44:55:66" {
		t.Fatalf("got %v", got)
	}
}

func TestListenCloseUnblocksAccept(t *testing.T) {
	l, err := Listen(bt.Addr{}, 0x1001)
	if err != nil {
		t.Skipf("no usable bluetooth stack: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	time.Sleep(50 * time.Millisecond)
	l.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept not unblocked by Close")
	}
}

// Close must make a blocked Accept return net.ErrClosed, or acceptLoop spins.
func TestListenerCloseUnblocksAccept(t *testing.T) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skip(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: "@btspeaker-hci-test"}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	l := &Listener{f: os.NewFile(uintptr(fd), "test"), psm: 1}
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	time.Sleep(50 * time.Millisecond) // let Accept block in the poller
	l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept still blocked after Close")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Accept: %v, want net.ErrClosed", err)
	}
}
