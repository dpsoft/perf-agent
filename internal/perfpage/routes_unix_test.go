//go:build unix

package perfpage

import "syscall"

// smallReceiveBuffer is a net.Dialer Control that shrinks the socket's
// receive buffer, so a client that does not read stops the server's
// writes soon.
func smallReceiveBuffer(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
	})
	if err != nil {
		return err
	}
	return serr
}
