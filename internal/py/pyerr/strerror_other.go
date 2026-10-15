//go:build !darwin

package pyerr

import "syscall"

// platformStrerror holds the glibc strerror texts that Go's errno table lacks. Errnos 0 to 140 match
// os.strerror of CPython 3.12 with glibc on linux-amd64 (Ubuntu 24.04, 2026-10-15).
var platformStrerror = map[syscall.Errno]string{
	0:   "Success",
	133: "Memory page has hardware error", // EHWPOISON
}

// unknownErrno is the glibc strerror text of an errno it does not know.
const unknownErrno = "Unknown error %d"

// platformErrnoClasses is empty: linux defines no errno outside the shared errnomap.
var platformErrnoClasses = map[syscall.Errno]string{}
