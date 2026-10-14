package ospath

import (
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// ExpandUser ports posixpath.expanduser. "~" uses HOME, or the password database entry of the current user
// when HOME is unset; "~name" uses the entry for name. A path without a usable entry is returned unchanged.
// A name with a NUL byte is an error, as pwd.getpwnam raises ValueError for it.
//
// py: Lib/posixpath.py expanduser (Python 3.15)
func ExpandUser(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	i := strings.IndexByte(path[1:], '/') + 1
	if i == 0 {
		i = len(path)
	}
	var home string
	if i == 1 {
		var ok bool
		if home, ok = os.LookupEnv("HOME"); !ok {
			entry, err := user.LookupId(strconv.Itoa(os.Getuid()))
			if err != nil {
				return path, nil
			}
			home = entry.HomeDir
		}
	} else {
		name := path[1:i]
		if strings.IndexByte(name, 0) >= 0 {
			return "", &pyerr.Error{Class: "ValueError", Msg: "embedded null byte"}
		}
		entry, err := user.Lookup(name)
		if err != nil {
			return path, nil
		}
		home = entry.HomeDir
	}
	if expanded := strings.TrimRight(home, "/") + path[i:]; expanded != "" {
		return expanded, nil
	}
	return "/", nil
}

// part is one entry of realpath's stack. A marker entry records that the symlink at name is now resolved.
type part struct {
	name   string
	marker bool
}

// Realpath ports posixpath.realpath(filename, strict=strict): it resolves the same symlinks and, when
// strict is false, ignores the same OSErrors. A relative filename starts from os.getcwd(), which is the
// kernel's path (syscall.Getwd, not $PWD). Errors are the Python ones:
//   - an OSError from os.getcwd(), in both modes;
//   - UnicodeEncodeError for a path that os.fsencode cannot encode, in both modes;
//   - ValueError "lstat: embedded null character in path" for a part with a NUL, in both modes, since lstat
//     raises it and realpath catches only OSError;
//   - with strict, the OSError of a failed lstat or readlink, ENOTDIR for a non-directory with parts
//     after it (or a trailing "/"), and ELOOP for a symlink loop.
//
// py: Lib/posixpath.py realpath (Python 3.15)
func Realpath(filename string, strict bool) (string, error) {
	names := strings.Split(strings.TrimRight(filename, "/"), "/")
	rest := make([]part, 0, len(names))
	for i := len(names) - 1; i >= 0; i-- {
		rest = append(rest, part{name: names[i]})
	}
	partCount := len(names)
	path := "/"
	if !strings.HasPrefix(filename, "/") {
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", pyerr.NewOSError(err)
		}
		path = cwd
	}
	trailingSep := strings.HasSuffix(filename, "/")
	seen := map[string]*string{} // a nil target marks a symlink that is not resolved yet
	for partCount > 0 {
		top := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		if top.marker {
			resolved := path
			seen[top.name] = &resolved
			continue
		}
		partCount--
		switch top.name {
		case "", ".":
			continue
		case "..":
			if path = path[:strings.LastIndexByte(path, '/')]; path == "" {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + top.name
		if path == "/" {
			newpath = "/" + top.name
		}
		encoded, err := pyerr.Fsencode(newpath) // lstat encodes the path before it checks for a NUL
		if err != nil {
			return "", err
		}
		if strings.IndexByte(top.name, 0) >= 0 { // the resolved path before it holds no NUL
			return "", &pyerr.Error{Class: "ValueError", Msg: "lstat: embedded null character in path"}
		}
		info, err := os.Lstat(encoded)
		if err != nil {
			if strict {
				return "", pyerr.NewOSError(err, newpath)
			}
			path = newpath
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if strict && (partCount > 0 || trailingSep) && !info.IsDir() {
				return "", pyerr.NewOSError(syscall.ENOTDIR, newpath)
			}
			path = newpath
			continue
		}
		if target, ok := seen[newpath]; ok {
			if target != nil {
				path = *target
				continue
			}
			if strict {
				return "", pyerr.NewOSError(syscall.ELOOP, newpath)
			}
			path = newpath // non-strict realpath leaves a symlink loop unresolved
			continue
		}
		target, err := os.Readlink(encoded)
		if err != nil {
			if strict {
				return "", pyerr.NewOSError(err, newpath)
			}
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		rest = append(rest, part{name: newpath, marker: true})
		targetNames := strings.Split(target, "/")
		for i := len(targetNames) - 1; i >= 0; i-- {
			rest = append(rest, part{name: targetNames[i]})
		}
		partCount += len(targetNames)
	}
	return path, nil
}
