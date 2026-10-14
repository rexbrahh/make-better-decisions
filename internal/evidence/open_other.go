//go:build unix && !darwin

package evidence

import (
	"os"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// openBeneath opens root/parts... for reading: root and each directory with O_DIRECTORY|O_NOFOLLOW, each
// relative to the one before, and the file with O_NOFOLLOW, so no part can be a symlink.
//
// py: agent_decisions/evidence.py _open_source_file
func openBeneath(root string, parts []string) (sourceFile, error) {
	const directory = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, err := syscall.Open(root, directory, 0)
	if err != nil {
		return nil, pyerr.NewOSError(err, root)
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := syscall.Openat(fd, part, directory, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, pyerr.NewOSError(err, part)
		}
		fd = next
	}
	name := parts[len(parts)-1]
	file, err := syscall.Openat(fd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	syscall.Close(fd)
	if err != nil {
		return nil, pyerr.NewOSError(err, name)
	}
	return os.NewFile(uintptr(file), strings.TrimSuffix(root, "/")+"/"+strings.Join(parts, "/")), nil
}
