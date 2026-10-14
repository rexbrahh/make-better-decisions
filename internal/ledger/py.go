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

// contains returns `v in members` for a set of hashable members.
func contains(v any, members ...any) (bool, error) {
	if err := json.Unhashable(v, "set element"); err != nil {
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
		return nil, &pyerr.Error{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", json.TypeName(v))}
	}
	if value, present := object.Get(key); present {
		return value, nil
	}
	return fallback, nil
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
	return nil, &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", json.TypeName(v))}
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
