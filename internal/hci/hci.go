// Package hci wraps the raw Bluetooth sockets btspeaker needs: the mgmt
// control socket and L2CAP SOCK_SEQPACKET listeners. All fds are non-blocking
// and owned by *os.File so the Go poller handles them and Close unblocks I/O.
package hci

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"btspeaker/internal/bt"
)

const (
	hciDevNone = 0xffff // HCI_DEV_NONE

	solBluetooth     = unix.SOL_BLUETOOTH
	btSecurity       = 4 // BT_SECURITY
	btSecurityMedium = 2 // BT_SECURITY_MEDIUM
	l2capOptions     = 1 // L2CAP_OPTIONS

	defaultMTU = 672
	// mediaIMTU is the receive MTU offered on the AVDTP PSM: one 3-DH5 baseband
	// packet minus the L2CAP header. High-bitpool SBC (XQ) frames are 200-320
	// bytes, so the default 672 would fit only 2-3 frames per media packet.
	mediaIMTU = 1017
	psmAVDTP  = 25
)

// OpenMgmt opens the mgmt (HCI_CHANNEL_CONTROL) socket. Each Read returns one
// mgmt packet, each Write sends one command. Most commands need CAP_NET_ADMIN.
func OpenMgmt() (*os.File, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("mgmt socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: hciDevNone, Channel: unix.HCI_CHANNEL_CONTROL}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("mgmt bind: %w", err)
	}
	return os.NewFile(uintptr(fd), "mgmt"), nil
}

// Listener accepts L2CAP connections on one PSM.
type Listener struct {
	f      *os.File
	psm    uint16
	closed atomic.Bool
}

// Listen creates an L2CAP listener on psm bound to the adapter address
// (zero Addr = BDADDR_ANY). Except for SDP (PSM 1), the socket requires medium security, which makes
// the kernel authenticate (and, for unpaired peers, pair via mgmt) before the
// connection is accepted.
func Listen(local bt.Addr, psm uint16) (*Listener, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.BTPROTO_L2CAP)
	if err != nil {
		return nil, fmt.Errorf("l2cap socket: %w", err)
	}
	fail := func(what string, err error) (*Listener, error) {
		unix.Close(fd)
		return nil, fmt.Errorf("l2cap psm %d %s: %w", psm, what, err)
	}
	// x/sys reverses Addr into wire order when marshalling, so pass display order.
	if err := unix.Bind(fd, &unix.SockaddrL2{PSM: psm, Addr: local}); err != nil {
		return fail("bind", err)
	}
	// struct bt_security { __u8 level; __u8 key_size; }
	// SDP keeps the kernel's BT_SECURITY_SDP (set at bind): peers query it before pairing.
	if psm != 1 {
		if err := unix.SetsockoptString(fd, solBluetooth, btSecurity, string([]byte{btSecurityMedium, 0})); err != nil {
			return fail("BT_SECURITY", err)
		}
	}
	if psm == psmAVDTP {
		if err := setIMTU(fd, mediaIMTU); err != nil {
			return fail("L2CAP_OPTIONS", err)
		}
	}
	if err := unix.Listen(fd, 4); err != nil {
		return fail("listen", err)
	}
	return &Listener{f: os.NewFile(uintptr(fd), fmt.Sprintf("l2cap-%d", psm)), psm: psm}, nil
}

// PSM returns the listening PSM.
func (l *Listener) PSM() uint16 { return l.psm }

// Close stops the listener and unblocks Accept (which then returns net.ErrClosed).
func (l *Listener) Close() error {
	l.closed.Store(true)
	return l.f.Close()
}

// Accept waits for the next connection.
func (l *Listener) Accept() (bt.Conn, error) {
	// A closed raw fd reports poll.ErrFileClosing ("use of closed file"),
	// which is not os.ErrClosed, so go by the flag instead.
	closedErr := func(err error) error {
		if l.closed.Load() || errors.Is(err, os.ErrClosed) {
			return net.ErrClosed
		}
		return err
	}
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, closedErr(err)
	}
	for {
		var nfd int
		var sa unix.Sockaddr
		var aerr error
		err := rc.Read(func(fd uintptr) bool {
			nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
			return aerr != unix.EAGAIN
		})
		if err != nil {
			return nil, closedErr(err)
		}
		if aerr == unix.EINTR || aerr == unix.ECONNABORTED {
			continue
		}
		if aerr != nil {
			return nil, closedErr(aerr)
		}
		var remote bt.Addr
		if l2, ok := sa.(*unix.SockaddrL2); ok {
			remote = addrFromWire(l2.Addr)
		}
		f := os.NewFile(uintptr(nfd), "l2cap-conn")
		omtu, imtu := mtus(f)
		return &conn{f: f, remote: remote, psm: l.psm, imtu: imtu, omtu: omtu}, nil
	}
}

// addrFromWire converts the kernel's little-endian bdaddr_t (what x/sys returns
// from accept/getpeername, unreversed) to display order.
func addrFromWire(w [6]byte) bt.Addr {
	var a bt.Addr
	for i := range w {
		a[i] = w[5-i]
	}
	return a
}

// setIMTU raises the incoming MTU in L2CAP_OPTIONS; accepted sockets inherit it.
func setIMTU(fd, imtu int) error {
	var buf [12]byte
	n := uint32(len(buf))
	if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_L2CAP, l2capOptions,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0); e != 0 {
		return e
	}
	if n < 4 {
		return unix.EINVAL
	}
	binary.NativeEndian.PutUint16(buf[2:], uint16(imtu))
	if _, _, e := unix.Syscall6(unix.SYS_SETSOCKOPT, uintptr(fd), unix.SOL_L2CAP, l2capOptions,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(n), 0); e != 0 {
		return e
	}
	return nil
}

// mtus reads L2CAP_OPTIONS {omtu,imtu u16; flush_to u16; mode,fcs,max_tx u8; txwin u16}.
func mtus(f *os.File) (omtu, imtu int) {
	omtu, imtu = defaultMTU, defaultMTU
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) {
		var buf [12]byte
		n := uint32(len(buf))
		_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_L2CAP, l2capOptions,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0)
		if e != 0 || n < 4 {
			return
		}
		o := int(binary.NativeEndian.Uint16(buf[0:]))
		i := int(binary.NativeEndian.Uint16(buf[2:]))
		if o > 0 {
			omtu = o
		}
		if i > 0 {
			imtu = i
		}
	})
	return
}

type conn struct {
	f          *os.File
	remote     bt.Addr
	psm        uint16
	imtu, omtu int
}

func (c *conn) Read(p []byte) (int, error)         { return c.f.Read(p) }
func (c *conn) Write(p []byte) (int, error)        { return c.f.Write(p) }
func (c *conn) Close() error                       { return c.f.Close() }
func (c *conn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *conn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
func (c *conn) RemoteAddr() bt.Addr                { return c.remote }
func (c *conn) PSM() uint16                        { return c.psm }
func (c *conn) InMTU() int                         { return c.imtu }
func (c *conn) OutMTU() int                        { return c.omtu }
