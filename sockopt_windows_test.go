//go:build windows

package muzak

import "syscall"

// setReceiveBuffer sets the receive buffer of the socket fd names, which on
// Windows is a handle. Set before the socket connects, it also stops Windows
// tuning the connection's receive window past it.
func setReceiveBuffer(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}
