// Package os ports the file descriptor calls of CPython 3.15's os module that agent-decisions uses.
// Import it as pyos beside the standard os package.
package os

import (
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// Open ports os.open: the descriptor is close-on-exec and EINTR retries.
//
// py: Modules/posixmodule.c os_open_impl (Python 3.15)
func Open(path string, flags int, mode uint32) (int, error) {
	for {
		fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC, mode)
		if err == nil {
			return fd, nil
		}
		if err != syscall.EINTR {
			return -1, pyerr.NewOSError(err, path)
		}
	}
}

// WriteAll ports the `while view: view = view[os.write(fd, view):]` loop.
func WriteAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := syscall.Write(fd, data)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return pyerr.NewOSError(err)
		}
		data = data[n:]
	}
	return nil
}
