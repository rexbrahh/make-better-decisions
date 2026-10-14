package install

import (
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

func statTimes(st *syscall.Stat_t) (atime, mtime syscall.Timespec) {
	return st.Atimespec, st.Mtimespec
}

// copyFlags ports the chflags step of shutil.copystat, which ignores a file system without flags.
//
// py: Lib/shutil.py copystat (Python 3.15)
func copyFlags(dst string, st *syscall.Stat_t) error {
	err := syscall.Chflags(dst, int(st.Flags))
	if err != nil && err != syscall.EOPNOTSUPP && err != syscall.ENOTSUP {
		return pyerr.NewOSError(err, dst)
	}
	return nil
}
