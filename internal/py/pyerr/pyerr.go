// Package pyerr holds the Go forms of Python exceptions. Each type returns str(exception) from Error and
// the Python class name from PyType, so a hook can print both as Python does.
//
// Every OSError class is an *OSError; every other class is an *Error. Port `except C` as
// IsInstance(err, "C"), which also catches the subclasses of C.
package pyerr

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Error is a Python exception outside the OSError family, such as ValueError or TypeError.
type Error struct {
	Class  string // the Python class name, such as "TypeError"
	Msg    string // str(exception)
	Module string // the module that defines Class, such as "agent_decisions.install"; "" for a builtin
}

func (e *Error) Error() string    { return e.Msg }
func (e *Error) PyType() string   { return e.Class }
func (e *Error) PyModule() string { return e.Module }

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

// Class returns the Python class name of err, or "" for an error outside the Python model.
func Class(err error) string {
	var typed interface{ PyType() string }
	if errors.As(err, &typed) {
		return typed.PyType()
	}
	return ""
}

// bases maps each Python exception class that the port raises, and each class between it and
// BaseException, to its base class. A ported module adds the classes it defines with Register.
//
// runtime.py defines its own RuntimeError(Exception). It shares the name and the base of the builtin, so
// this one entry stands for both. No Python module has `except RuntimeError`, so the two never differ here.
//
// py: Objects/exceptions.c, Lib/json/decoder.py JSONDecodeError, Lib/subprocess.py TimeoutExpired (Python 3.15)
var bases = map[string]string{
	"BaseException": "",
	"Exception":     "BaseException",

	"ArithmeticError": "Exception",
	"OverflowError":   "ArithmeticError",

	"AttributeError": "Exception",

	"LookupError": "Exception",
	"KeyError":    "LookupError",

	"OSError":                "Exception",
	"BlockingIOError":        "OSError",
	"ChildProcessError":      "OSError",
	"ConnectionError":        "OSError",
	"BrokenPipeError":        "ConnectionError",
	"ConnectionAbortedError": "ConnectionError",
	"ConnectionRefusedError": "ConnectionError",
	"ConnectionResetError":   "ConnectionError",
	"FileExistsError":        "OSError",
	"FileNotFoundError":      "OSError",
	"InterruptedError":       "OSError",
	"IsADirectoryError":      "OSError",
	"NotADirectoryError":     "OSError",
	"PermissionError":        "OSError",
	"ProcessLookupError":     "OSError",
	"TimeoutError":           "OSError",

	"RuntimeError":   "Exception",
	"RecursionError": "RuntimeError",

	"TypeError": "Exception",

	"ValueError":         "Exception",
	"UnicodeError":       "ValueError",
	"UnicodeDecodeError": "UnicodeError",
	"UnicodeEncodeError": "UnicodeError",
	"JSONDecodeError":    "ValueError",

	"SubprocessError": "Exception",
	"TimeoutExpired":  "SubprocessError",
}

// Register declares class, an exception class that a ported module defines, as a direct subclass of base.
// Call it from an init function. It panics on an unknown base or on a second declaration with another
// base.
func Register(class, base string) {
	if _, ok := bases[base]; !ok {
		panic("pyerr.Register: unknown base class " + base)
	}
	if old, ok := bases[class]; ok && old != base {
		panic("pyerr.Register: " + class + " already derives from " + old)
	}
	bases[class] = base
}

// IsInstance reports whether Python's `except (class, ...)` catches err: whether the class of err is one
// of classes or a subclass of one. An error outside the Python model is an instance of no class. It panics
// on a class that is neither in the builtin table nor registered, because that is a gap in the port.
func IsInstance(err error, classes ...string) bool {
	for _, class := range classes {
		if _, ok := bases[class]; !ok {
			panic("pyerr.IsInstance: unknown class " + class)
		}
	}
	for c := Class(err); c != ""; c = bases[c] {
		if _, ok := bases[c]; !ok {
			panic("pyerr.IsInstance: unknown class " + c)
		}
		if slices.Contains(classes, c) {
			return true
		}
	}
	return false
}

// WriteUncaught writes the last line of the traceback that CPython prints for an uncaught err:
// "module.Class: message", with the module left out for a builtin class and ": message" left out for an
// empty message. It encodes the line as sys.stderr does, with backslashreplace. A Go error without a
// Python class prints as Exception. The module comes from a PyModule method beside PyType.
//
// py: Lib/traceback.py TracebackException.format_exception_only, through sys.excepthook (Python 3.15)
func WriteUncaught(w io.Writer, err error) {
	line := "Exception"
	var typed interface{ PyType() string }
	if errors.As(err, &typed) {
		line = typed.PyType()
		if module, ok := typed.(interface{ PyModule() string }); ok && module.PyModule() != "" {
			line = module.PyModule() + "." + line
		}
	}
	if msg := err.Error(); msg != "" {
		line += ": " + msg
	}
	io.WriteString(w, EncodeBackslashReplace(line+"\n"))
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

// EncodeBackslashReplace returns s.encode(errors="backslashreplace"), the encoding of sys.stderr: a lone
// surrogate becomes its \udXXX escape. An undecodable byte stands for U+DC00 plus the byte, so it becomes
// \udcXX.
//
// py: Python/codecs.c PyCodec_BackslashReplaceErrors (Python 3.15)
func EncodeBackslashReplace(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var out strings.Builder
	for rest := s; rest != ""; {
		r, size := str.DecodeRune(rest)
		if r >= 0xD800 && r <= 0xDFFF {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.WriteString(rest[:size])
		}
		rest = rest[size:]
	}
	return out.String()
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
