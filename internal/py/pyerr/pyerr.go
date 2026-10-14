// Package pyerr holds the Go forms of Python exceptions. Each type returns str(exception) from Error and
// the Python class name from PyType, so a hook can print both as Python does.
//
// Port `except OSError` as errors.As(err, new(*OSError)). Every other class is an *Error.
package pyerr

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Error is a Python exception outside the OSError family, such as ValueError or TypeError.
type Error struct {
	Class string // the Python class name, such as "TypeError"
	Msg   string // str(exception)
}

func (e *Error) Error() string  { return e.Msg }
func (e *Error) PyType() string { return e.Class }

// OSError is a Python OSError or one of its errno subclasses, such as FileNotFoundError.
type OSError struct {
	Class string // "OSError" or the errno subclass name
	Msg   string // str(exception)
	Err   error  // the Go error it came from, or nil
}

func (e *OSError) Error() string  { return e.Msg }
func (e *OSError) PyType() string { return e.Class }
func (e *OSError) Unwrap() error  { return e.Err }

// errnoClasses maps an errno to its OSError subclass. EWOULDBLOCK is EAGAIN on darwin and linux.
// platformErrnoClasses (strerror_*.go) adds the errnos that only some platforms define.
//
// py: Objects/exceptions.c _PyExc_InitState errnomap (Python 3.15)
var errnoClasses = map[syscall.Errno]string{
	syscall.EAGAIN:       "BlockingIOError",
	syscall.EALREADY:     "BlockingIOError",
	syscall.EINPROGRESS:  "BlockingIOError",
	syscall.EPIPE:        "BrokenPipeError",
	syscall.ESHUTDOWN:    "BrokenPipeError",
	syscall.ECHILD:       "ChildProcessError",
	syscall.ECONNABORTED: "ConnectionAbortedError",
	syscall.ECONNREFUSED: "ConnectionRefusedError",
	syscall.ECONNRESET:   "ConnectionResetError",
	syscall.EEXIST:       "FileExistsError",
	syscall.ENOENT:       "FileNotFoundError",
	syscall.EISDIR:       "IsADirectoryError",
	syscall.ENOTDIR:      "NotADirectoryError",
	syscall.EINTR:        "InterruptedError",
	syscall.EACCES:       "PermissionError",
	syscall.EPERM:        "PermissionError",
	syscall.ESRCH:        "ProcessLookupError",
	syscall.ETIMEDOUT:    "TimeoutError",
}

// ErrnoClass returns the class of OSError(errno, ...): an errno subclass, or "OSError".
//
// py: Objects/exceptions.c OSError_new (Python 3.15)
func ErrnoClass(errno syscall.Errno) string {
	if class, ok := errnoClasses[errno]; ok {
		return class
	}
	if class, ok := platformErrnoClasses[errno]; ok {
		return class
	}
	return "OSError"
}

// Strerror returns os.strerror(errno). Go's errno texts are the C library's with a lower-case first letter;
// the C library texts that Go lacks are in strerror_*.go.
//
// py: Modules/posixmodule.c os_strerror_impl (Python 3.15)
func Strerror(errno syscall.Errno) string {
	if text, ok := platformStrerror[errno]; ok {
		return text
	}
	text := errno.Error()
	if text == fmt.Sprintf("errno %d", int(errno)) { // Go has no text for this errno
		return fmt.Sprintf(unknownErrno, int(errno))
	}
	return strings.ToUpper(text[:1]) + text[1:] // Go's errno texts start with an ASCII letter
}

// NewOSError returns the OSError that CPython raises for err from a call on filenames: none, one, or the
// source and target of a rename. The message is "[Errno 2] No such file or directory: 'path'". An err
// without an errno becomes a plain OSError with err's text.
//
// py: Objects/exceptions.c OSError_str (Python 3.15)
func NewOSError(err error, filenames ...string) *OSError {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return &OSError{Class: "OSError", Msg: err.Error(), Err: err}
	}
	msg := fmt.Sprintf("[Errno %d] %s", int(errno), Strerror(errno))
	for i, name := range filenames {
		msg += [...]string{": ", " -> "}[min(i, 1)] + str.Repr(name)
	}
	return &OSError{Class: ErrnoClass(errno), Msg: msg, Err: err}
}

// EncodeUTF8 returns s.encode() with the strict error handler: a lone surrogate is a UnicodeEncodeError.
//
// py: Objects/unicodeobject.c unicode_encode_utf8 (Python 3.15)
func EncodeUTF8(s string) (string, error) {
	return encode(s, false)
}

// Fsencode returns os.fsencode(s): UTF-8 with the surrogateescape error handler. A lone surrogate
// U+DC80-U+DCFF becomes its low byte; any other lone surrogate is a UnicodeEncodeError over the rest of its
// run of surrogates. An undecodable byte stands for U+DC00 plus the byte, so it stays as it is.
//
// py: Objects/unicodeobject.c unicode_encode_utf8 (Python 3.15)
func Fsencode(s string) (string, error) {
	return encode(s, true)
}

func encode(s string, escape bool) (string, error) {
	if utf8.ValidString(s) {
		return s, nil // no surrogate and no raw byte
	}
	var out strings.Builder
	for rest, position := s, 0; rest != ""; position++ {
		r, size := str.DecodeRune(rest)
		switch {
		case escape && r >= 0xDC80 && r <= 0xDCFF:
			out.WriteByte(byte(r)) // a raw byte from DecodeRune stays the same byte
		case r >= 0xD800 && r <= 0xDFFF:
			return "", surrogateError(rest, position)
		default:
			out.WriteString(rest[:size])
		}
		rest = rest[size:]
	}
	return out.String(), nil
}

// surrogateError returns the UnicodeEncodeError for the surrogate that starts rest, at position. CPython
// reports the whole run of surrogates from there, including ones that surrogateescape could encode.
func surrogateError(rest string, position int) error {
	first, _ := str.DecodeRune(rest)
	end := position
	for ; rest != ""; end++ {
		r, size := str.DecodeRune(rest)
		if r < 0xD800 || r > 0xDFFF {
			break
		}
		rest = rest[size:]
	}
	msg := fmt.Sprintf(`'utf-8' codec can't encode character '\u%04x' in position %d: surrogates not allowed`,
		first, position)
	if end-position > 1 {
		msg = fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed",
			position, end-1)
	}
	return &Error{Class: "UnicodeEncodeError", Msg: msg}
}
