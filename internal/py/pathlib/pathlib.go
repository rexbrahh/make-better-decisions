// Package pathlib ports the parts of Python's pathlib that agent-decisions uses on POSIX paths. Every
// separator is the ASCII "/", so the strings package is exact on WTF-8 path text here.
//
// Join, Parent and Name take a path that Str already normalized.
package pathlib

import (
	"os"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// Str returns str(PurePosixPath(p)): it drops empty and "." parts, keeps ".." and a leading "//", and
// spells the empty path ".".
//
// py: Lib/pathlib/_local.py PurePath.__str__ (Python 3.15)
func Str(p string) string {
	root := ""
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		root = "//"
	} else if strings.HasPrefix(p, "/") {
		root = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if s := root + strings.Join(parts, "/"); s != "" {
		return s
	}
	return "."
}

// Join returns str(Path(base) / rest) for a normalized base and a rest made of plain names.
//
// py: Lib/pathlib/_local.py PurePath.__truediv__ (Python 3.15)
func Join(base, rest string) string {
	switch {
	case base == ".":
		return rest
	case strings.HasSuffix(base, "/"):
		return base + rest
	}
	return base + "/" + rest
}

// Parent returns str(PurePosixPath(p).parent) for a normalized p.
//
// py: Lib/pathlib/_local.py PurePath.parent (Python 3.15)
func Parent(p string) string {
	i := strings.LastIndexByte(p, '/')
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	case p[:i] == "/":
		return "//"
	}
	return p[:i]
}

// Name returns PurePosixPath(p).name for a normalized p: its last part, or "" for a bare root or ".".
//
// py: Lib/pathlib/_local.py PurePath.name (Python 3.15)
func Name(p string) string {
	if p == "." {
		return ""
	}
	return p[strings.LastIndexByte(p, '/')+1:]
}

// Mkdir ports Path(p).mkdir(mode, parents=parents, exist_ok=True) for a normalized p: missing parents get
// the default mode 0o777, and only p itself gets mode.
//
// py: Lib/pathlib/_local.py Path.mkdir (Python 3.15)
func Mkdir(p string, mode uint32, parents bool) error {
	err := syscall.Mkdir(p, mode)
	if err == nil {
		return nil
	}
	if err == syscall.ENOENT {
		up := Parent(p)
		if !parents || up == p {
			return pyerr.NewOSError(err, p)
		}
		if err := Mkdir(up, 0o777, true); err != nil {
			return err
		}
		return Mkdir(p, mode, false)
	}
	if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
		return nil
	}
	return pyerr.NewOSError(err, p)
}

// Home returns str(Path.home()).
//
// py: Lib/pathlib/__init__.py Path.home (Python 3.15)
func Home() (string, error) {
	home, err := ospath.ExpandUser("~")
	if err != nil {
		return "", err
	}
	if home == "~" {
		return "", &pyerr.Error{Class: "RuntimeError", Msg: "Could not determine home directory."}
	}
	return Str(home), nil
}

// ExpandUser returns str(Path(p).expanduser()): only a leading "~" part expands.
//
// py: Lib/pathlib/__init__.py Path.expanduser (Python 3.15)
func ExpandUser(p string) (string, error) {
	path := Str(p)
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	first, rest, _ := strings.Cut(path, "/")
	home, err := ospath.ExpandUser(first)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(home, "~") {
		return "", &pyerr.Error{Class: "RuntimeError", Msg: "Could not determine home directory."}
	}
	if rest == "" {
		return Str(home), nil
	}
	return Join(Str(home), rest), nil
}
