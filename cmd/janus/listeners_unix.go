//go:build !windows

package main

// What the edge actually bound, from the kernel: every descriptor this
// process holds, asked with getsockname. The admin API knows only what was
// configured; this is the answer for what is listening, and it needs no
// privilege beyond owning the descriptors.

import (
	"errors"
	"net/netip"
	"os"
	"strconv"
	"syscall"
)

// ownSockets lists this process's bound IP sockets: listening TCP sockets
// and every UDP socket, with the local address of each.
func ownSockets() ([]boundSocket, error) {
	// Names only: stat-ing an entry of /dev/fd fails with EBADF on macOS
	// when the descriptor is a kqueue, which the Go runtime always holds.
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return nil, err
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return nil, err
	}
	var out []boundSocket
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
		if err != nil {
			continue // not a socket, or gone
		}
		var proto string
		switch typ {
		case syscall.SOCK_STREAM:
			if !isListening(fd) {
				continue
			}
			proto = "tcp"
		case syscall.SOCK_DGRAM:
			proto = "udp"
		default:
			continue
		}
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		var ap netip.AddrPort
		switch a := sa.(type) {
		case *syscall.SockaddrInet4:
			ap = netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port))
		case *syscall.SockaddrInet6:
			ap = netip.AddrPortFrom(netip.AddrFrom16(a.Addr), uint16(a.Port))
		default:
			continue // unix sockets are not a front door
		}
		out = append(out, boundSocket{proto: proto, addr: ap})
	}
	return out, nil
}

// isListening tells a listening stream socket from a connection. Linux
// answers SO_ACCEPTCONN; macOS has no such option (ENOPROTOOPT), and there
// a stream socket with no peer is a listener — a connection always has one.
func isListening(fd int) bool {
	accepting, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	if err == nil {
		return accepting != 0
	}
	if !errors.Is(err, syscall.ENOPROTOOPT) {
		return false
	}
	_, err = syscall.Getpeername(fd)
	return errors.Is(err, syscall.ENOTCONN)
}
