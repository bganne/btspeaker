package main

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errCgoBuild: syscall.AllThreadsSyscall is unavailable in a cgo build, so
// the capabilities of the other threads cannot be dropped.
var errCgoBuild = errors.New("built with cgo (build with CGO_ENABLED=0)")

// dropCaps irrevocably drops every capability of every thread of the process.
// Capabilities are only checked when sockets are bound (L2CAP PSMs < 0x1001
// need CAP_NET_BIND_SERVICE; the mgmt socket is trusted if bound with
// CAP_NET_ADMIN), so it is called once they all are. The uid is kept: as root
// (a manual run) the state dir and devices stay accessible, and root without
// capabilities has no privilege over other users' files.
//
// Capability calls only affect the calling thread, hence AllThreadsSyscall.
func dropCaps() error {
	all := func(name string, trap, a1, a2, a3 uintptr) error {
		if _, _, e := syscall.AllThreadsSyscall(trap, a1, a2, a3); e != 0 {
			if e == syscall.ENOTSUP {
				return errCgoBuild
			}
			return fmt.Errorf("%s: %w", name, e)
		}
		return nil
	}
	// No execve can regain anything (file capabilities, setuid).
	if err := all("PR_SET_NO_NEW_PRIVS", unix.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0); err != nil {
		return err
	}
	if err := all("PR_CAP_AMBIENT_CLEAR_ALL", unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0); err != nil {
		return err
	}
	// Shrinking the bounding set needs CAP_SETPCAP (root has it, the systemd
	// unit does not): best effort. With an empty permitted set and
	// no_new_privs, what is left in it can never be raised again anyway.
	for c := uintptr(0); c < 64; c++ {
		err := all("PR_CAPBSET_DROP", unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, c, 0)
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) { // past CAP_LAST_CAP, or no CAP_SETPCAP
			break
		}
		if err != nil {
			return err
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3} // Pid 0: the calling thread
	var data [2]unix.CapUserData                                        // all sets empty
	err := all("capset", unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	runtime.KeepAlive(&hdr)
	runtime.KeepAlive(&data)
	return err
}
