package install

// Ports of the CPython 3.15 library behaviour that install.py relies on: pathlib queries, text file reads,
// subprocess spawn errors, shutil.copy2, uuid4 and the dict helpers of the value model.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// expandUser returns str(Path(p).expanduser()): only a leading "~" part expands.
//
// py: Lib/pathlib/_local.py Path.expanduser (Python 3.15)
func expandUser(p string) (string, error) {
	path := pathlib.Str(p)
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
		return pathlib.Str(home), nil
	}
	return pathlib.Join(pathlib.Str(home), rest), nil
}

// absolute returns str(Path(p).absolute()) for a normalized p: a relative path joins the kernel's working
// directory.
//
// py: Lib/pathlib/_local.py Path.absolute (Python 3.15)
func absolute(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return p, nil
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	if p == "." {
		return cwd, nil
	}
	return pathlib.Str(cwd + "/" + p), nil
}

// homeDir returns str(Path.home()).
//
// py: Lib/pathlib/_local.py Path.home (Python 3.15)
func homeDir() (string, error) {
	home, err := ospath.ExpandUser("~")
	if err != nil {
		return "", err
	}
	if home == "~" {
		return "", &pyerr.Error{Class: "RuntimeError", Msg: "Could not determine home directory."}
	}
	return pathlib.Str(home), nil
}

// baseDir returns `Path.home() if home is None else home` for the "" spelling of None.
func baseDir(home string) (string, error) {
	if home == "" {
		return homeDir()
	}
	return pathlib.Str(home), nil
}

// globClaudeConfigs returns sorted(base.glob(".claude-*/.claude.json")). The wildcard part keeps entries
// that are directories after following symlinks; the literal part only has to exist as a link.
//
// py: Lib/pathlib/_local.py Path.glob (Python 3.15)
func globClaudeConfigs(base string) []string {
	entries, _ := os.ReadDir(base) // a directory that cannot be listed yields nothing
	var paths []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".claude-") {
			continue
		}
		dir := pathlib.Join(base, entry.Name())
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		if config := pathlib.Join(dir, ".claude.json"); lexists(config) {
			paths = append(paths, config)
		}
	}
	sortPaths(paths)
	return paths
}

// globCodexProfiles returns sorted(dir.glob("*.config.toml")): every entry type matches.
//
// py: Lib/pathlib/_local.py Path.glob (Python 3.15)
func globCodexProfiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var paths []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".config.toml") {
			paths = append(paths, pathlib.Join(dir, entry.Name()))
		}
	}
	sortPaths(paths)
	return paths
}

// sortPaths sorts paths as Python sorts PosixPath objects: by their lists of parts. Byte order equals code
// point order for UTF-8.
//
// py: Lib/pathlib/_local.py PurePath.__lt__ (Python 3.15)
func sortPaths(paths []string) {
	slices.SortFunc(paths, func(a, b string) int {
		return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
	})
}

// exists ports Path.exists: any error reads as false.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// isFile ports Path.is_file: it follows symlinks, and any error reads as false.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// readText returns Path(path).read_text(encoding="utf-8"): strict UTF-8 with universal newlines.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", pyerr.NewOSError(err, path)
	}
	text, err := decodeStrict(data)
	if err != nil {
		return "", err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// decodeStrict returns data.decode("utf-8"), or the UnicodeDecodeError of its first invalid sequence.
//
// py: Objects/unicodeobject.c unicode_decode_utf8 (Python 3.15)
func decodeStrict(data []byte) (string, error) {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		reason, length := invalidUTF8(data[i:])
		if length == 1 {
			return "", &pyerr.Error{Class: "UnicodeDecodeError", Msg: fmt.Sprintf(
				"'utf-8' codec can't decode byte 0x%02x in position %d: %s", data[i], i, reason)}
		}
		return "", &pyerr.Error{Class: "UnicodeDecodeError", Msg: fmt.Sprintf(
			"'utf-8' codec can't decode bytes in position %d-%d: %s", i, i+length-1, reason)}
	}
	return string(data), nil
}

// invalidUTF8 returns CPython's reason and length for the invalid sequence at the start of b: the lead byte
// and the continuation bytes that were valid before the error.
//
// py: Objects/stringlib/codecs.h utf8_decode (Python 3.15)
func invalidUTF8(b []byte) (string, int) {
	lead, need := b[0], 0
	low, high := byte(0x80), byte(0xbf) // the range of the second byte
	switch {
	case lead >= 0xc2 && lead <= 0xdf:
		need = 2
	case lead >= 0xe0 && lead <= 0xef:
		need = 3
		if lead == 0xe0 {
			low = 0xa0
		} else if lead == 0xed {
			high = 0x9f // surrogates
		}
	case lead >= 0xf0 && lead <= 0xf4:
		need = 4
		if lead == 0xf0 {
			low = 0x90
		} else if lead == 0xf4 {
			high = 0x8f
		}
	default:
		return "invalid start byte", 1
	}
	for k := 1; k < need; k++ {
		if k == len(b) {
			return "unexpected end of data", k
		}
		if k == 1 && (b[k] < low || b[k] > high) || k > 1 && (b[k] < 0x80 || b[k] > 0xbf) {
			return "invalid continuation byte", k
		}
	}
	panic("install: invalidUTF8 called on valid UTF-8")
}

// spawnError returns the OSError that subprocess raises when binary cannot start. A binary that PATH does
// not hold is FileNotFoundError, as the last execve of Python's PATH search reports.
func spawnError(err error, binary string) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		err = syscall.ENOENT
	}
	return pyerr.NewOSError(err, binary)
}

// isValueError reports whether err is a ValueError or one of its subclasses.
func isValueError(err error) bool {
	switch pyClass(err) {
	case "ValueError", "JSONDecodeError", "UnicodeDecodeError", "UnicodeEncodeError":
		return true
	}
	return false
}

// pyClass returns the Python class name of err, or "" for an error outside the Python model.
func pyClass(err error) string {
	var typed interface{ PyType() string }
	if errors.As(err, &typed) {
		return typed.PyType()
	}
	return ""
}

// pyWrite writes s to a Python text stream with the backslashreplace error handler of sys.stderr: a lone
// surrogate prints as \udXXX.
func pyWrite(w io.Writer, s string) {
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
	io.WriteString(w, out.String())
}

// lastCodePoint returns the last code point of s as a string, or "" for an empty s.
func lastCodePoint(s string) string {
	for back := min(4, len(s)); back > 0; back-- {
		if _, size := str.DecodeRune(s[len(s)-back:]); size == back {
			return s[len(s)-back:]
		}
	}
	return ""
}

// valueOr returns o.get(key, fallback).
func valueOr(o *json.Object, key string, fallback any) any {
	if value, present := o.Get(key); present {
		return value
	}
	return fallback
}

// orDefault returns `value or fallback`.
func orDefault(value, fallback any) any {
	if json.Truthy(value) {
		return value
	}
	return fallback
}

// item returns d[key], with the KeyError Python raises for a missing key.
func item(d *json.Object, key string) (any, error) {
	if value, present := d.Get(key); present {
		return value, nil
	}
	return nil, &pyerr.Error{Class: "KeyError", Msg: str.Repr(key)}
}

// stringList reports whether v is a list of str.
func stringList(v any) ([]any, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return nil, false
		}
	}
	return list, true
}

// stringMap reports whether v is a dict of str values.
func stringMap(v any) (*json.Object, bool) {
	object, ok := v.(*json.Object)
	if !ok {
		return nil, false
	}
	for _, key := range object.Keys() {
		if _, ok := object.Value(key).(string); !ok {
			return nil, false
		}
	}
	return object, true
}

// setDefaultObject ports `x = o.setdefault(key, {})` followed by a reset to {} when x is not a dict. An
// existing key keeps its position.
func setDefaultObject(o *json.Object, key string) *json.Object {
	if value, ok := o.Value(key).(*json.Object); ok {
		return value
	}
	value := json.NewObject()
	o.Set(key, value)
	return value
}

// isFalse ports `v is False`.
func isFalse(v any) bool {
	flag, ok := v.(bool)
	return ok && !flag
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// optional returns the JSON value of an Optional[str]: nil or the string.
func optional(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// deepCopy ports copy.deepcopy for the JSON value model.
func deepCopy(v any) any {
	switch v := v.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = deepCopy(item)
		}
		return out
	case *json.Object:
		out := json.NewObject()
		for _, key := range v.Keys() {
			out.Set(key, deepCopy(v.Value(key)))
		}
		return out
	}
	return v
}

// typeName returns type(v).__name__ for a value of the JSON value model.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *json.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// reprList returns repr(values) for a list of str.
func reprList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = str.Repr(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// uuid4 returns str(uuid.uuid4()).
func uuid4() string {
	var b [16]byte
	rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
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

// copy2 ports shutil.copy2(src, dst) for a regular file: copy the bytes, then the times, the mode and, on
// macOS, the file flags.
// ponytail: shutil's same-file and special-file checks and Linux xattr copy are not ported; the source is
// always a resolved config file and dst a new name beside it.
//
// py: Lib/shutil.py copyfile, copystat (Python 3.15)
func copy2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return pyerr.NewOSError(err, src)
	}
	file, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return pyerr.NewOSError(err, dst)
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return pyerr.NewOSError(err, dst)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(src, &st); err != nil {
		return pyerr.NewOSError(err, src)
	}
	atime, mtime := statTimes(&st)
	if err := syscall.UtimesNano(dst, []syscall.Timespec{atime, mtime}); err != nil {
		return pyerr.NewOSError(err, dst)
	}
	if err := syscall.Chmod(dst, uint32(st.Mode)&0o7777); err != nil {
		return pyerr.NewOSError(err, dst)
	}
	return copyFlags(dst, &st)
}
