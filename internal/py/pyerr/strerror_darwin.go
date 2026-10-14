package pyerr

import "syscall"

// enotcapable is ENOTCAPABLE, which package syscall does not define for darwin.
const enotcapable = syscall.Errno(107)

// platformStrerror holds the darwin strerror texts that Go's errno table lacks.
var platformStrerror = map[syscall.Errno]string{
	0:           "Undefined error: 0",
	enotcapable: "Capabilities insufficient",
}

// unknownErrno is the darwin strerror text of an errno it does not know.
const unknownErrno = "Unknown error: %d"

// py: Objects/exceptions.c _PyExc_InitState errnomap ENOTCAPABLE (Python 3.15)
var platformErrnoClasses = map[syscall.Errno]string{enotcapable: "PermissionError"}
