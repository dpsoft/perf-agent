//go:build !unix

package perfpage

import "syscall"

// smallReceiveBuffer leaves the buffer as it is where shrinking it is not
// portable; the tests that use it still hold, with more data in flight.
func smallReceiveBuffer(_, _ string, _ syscall.RawConn) error { return nil }
