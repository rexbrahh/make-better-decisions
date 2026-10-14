//go:build !darwin

package pyerr

import "syscall"

// platformStrerror holds the glibc strerror texts that Go's errno table lacks. Not verified against a
// CPython oracle: this repository's oracle runs on darwin.
var platformStrerror = map[syscall.Errno]string{0: "Success"}

// unknownErrno is the glibc strerror text of an errno it does not know.
const unknownErrno = "Unknown error %d"

// platformErrnoClasses is empty: linux defines no errno outside the shared errnomap.
var platformErrnoClasses = map[syscall.Errno]string{}
