package websocket_test

import "syscall"

// setReceiveBuffer sets the receive buffer of the socket fd to n bytes.
func setReceiveBuffer(fd uintptr, n int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
}
