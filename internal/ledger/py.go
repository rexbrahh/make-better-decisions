package ledger

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// typeName returns type(v).__name__ for a value of the JSON value model.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Int, int, int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any, []string:
		return "list"
	case *json.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// hashable returns the TypeError that Python raises when v cannot be used as a set element or dict key;
// use names the role, "a set element" or "a dict key".
func hashable(v any, use string) error {
	switch v.(type) {
	case []any, []string, *json.Object:
		name := typeName(v)
		return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("cannot use '%s' as %s (unhashable type: '%s')", name, use, name)}
	}
	return nil
}

// contains returns `v in members` for a set of hashable members.
func contains(v any, members ...any) (bool, error) {
	if err := hashable(v, "a set element"); err != nil {
		return false, err
	}
	for _, member := range members {
		if same, err := json.Equal(v, member); err != nil || same {
			return same, err
		}
	}
	return false, nil
}

// get returns v.get(key, fallback), with the AttributeError Python raises when v is not a dict.
func get(v any, key string, fallback any) (any, error) {
	object, ok := v.(*json.Object)
	if !ok {
		return nil, &pyerr.Error{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", typeName(v))}
	}
	if value, present := object.Get(key); present {
		return value, nil
	}
	return fallback, nil
}

// item returns d[key], with the KeyError Python raises for a missing key.
func item(d *json.Object, key string) (any, error) {
	if value, present := d.Get(key); present {
		return value, nil
	}
	return nil, &pyerr.Error{Class: "KeyError", Msg: str.Repr(key)}
}

// head returns v[:n] for n >= 0, with the errors Python raises when v is not a str or list.
func head(v any, n int) (any, error) {
	switch v := v.(type) {
	case string:
		return str.SliceTo(v, n), nil
	case []any:
		return v[:min(n, len(v)):min(n, len(v))], nil
	case *json.Object:
		return nil, &pyerr.Error{Class: "KeyError", Msg: fmt.Sprintf("slice(None, %d, None)", n)}
	}
	return nil, &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", typeName(v))}
}

// iterate returns the items that `for x in v` yields for a str (its code points) or a list.
func iterate(v any) []any {
	switch v := v.(type) {
	case string:
		var chars []any
		for v != "" {
			_, width := str.DecodeRune(v)
			chars = append(chars, v[:width])
			v = v[width:]
		}
		return chars
	case []any:
		return v
	}
	return nil
}

// backslashReplace returns s.encode("utf-8", errors="backslashreplace"): a lone surrogate becomes \udXXX.
func backslashReplace(s string) []byte {
	var out []byte
	for s != "" {
		r, width := str.DecodeRune(s)
		if r >= 0xD800 && r <= 0xDFFF {
			out = fmt.Appendf(out, `\u%04x`, r)
		} else {
			out = append(out, s[:width]...)
		}
		s = s[width:]
	}
	return out
}

// openFD ports os.open: the descriptor is close-on-exec and EINTR retries.
func openFD(path string, flags int, mode uint32) (int, error) {
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

// writeAll ports the `while view: view = view[os.write(fd, view):]` loop.
func writeAll(fd int, data []byte) error {
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

// flock ports fcntl.flock, which retries EINTR.
func flock(fd, how int) error {
	for {
		err := syscall.Flock(fd, how)
		if err != syscall.EINTR {
			if err != nil {
				return pyerr.NewOSError(err)
			}
			return nil
		}
	}
}

// fsync ports os.fsync: a plain fsync(2), not the F_FULLFSYNC that os.File.Sync uses on darwin.
func fsync(fd int) error {
	for {
		err := syscall.Fsync(fd)
		if err != syscall.EINTR {
			if err != nil {
				return pyerr.NewOSError(err)
			}
			return nil
		}
	}
}

// closeFD ports os.close.
func closeFD(fd int) error {
	if err := syscall.Close(fd); err != nil {
		return pyerr.NewOSError(err)
	}
	return nil
}

// readError strips the filename from a read error, as CPython reports read errors without one.
func readError(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return pyerr.NewOSError(err)
}
