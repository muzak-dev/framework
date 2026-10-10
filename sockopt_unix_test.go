//go:build unix

package muzak

import "syscall"

// setReceiveBuffer sets the receive buffer of the socket fd names.
func setReceiveBuffer(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}
