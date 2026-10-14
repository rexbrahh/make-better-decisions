package install

import "syscall"

func statTimes(st *syscall.Stat_t) (atime, mtime syscall.Timespec) {
	return st.Atim, st.Mtim
}

// copyFlags is a no-op: Linux stat results have no st_flags, so shutil.copystat skips chflags.
func copyFlags(string, *syscall.Stat_t) error {
	return nil
}
