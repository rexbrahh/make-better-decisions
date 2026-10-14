package evidence

import (
	"os"
	"strings"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// oNofollowAny is darwin's O_NOFOLLOW_ANY: open fails with ELOOP when any part of the path is a symlink.
// The syscall package does not define it.
const oNofollowAny = 0x20000000

// openBeneath opens root/parts... for reading and refuses a symlink in any part, as _open_source_file's
// O_NOFOLLOW openat chain does. darwin has no openat in the syscall package, and root, which realpath
// returned, holds no symlink, so one open with O_NOFOLLOW_ANY checks the same parts. O_NOFOLLOW_ANY covers
// the last part too, and darwin rejects it combined with O_NOFOLLOW (EINVAL, measured on Darwin 27.2).
//
// py: agent_decisions/evidence.py _open_source_file
func openBeneath(root string, parts []string) (sourceFile, error) {
	path := strings.TrimSuffix(root, "/") + "/" + strings.Join(parts, "/")
	f, err := os.OpenFile(path, os.O_RDONLY|oNofollowAny, 0)
	if err != nil {
		return nil, pyerr.NewOSError(err, parts[len(parts)-1])
	}
	return f, nil
}
