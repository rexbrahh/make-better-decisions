package install

// Ports of the CPython 3.15 library behaviour that install.py relies on: pathlib queries, text file reads,
// subprocess spawn errors, shutil.copy2, uuid4 and the dict helpers of the value model.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// baseDir returns `Path.home() if home is None else home` for the "" spelling of None.
func baseDir(home string) (string, error) {
	if home == "" {
		return pathlib.Home()
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

func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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

// lastCodePoint returns the last code point of s as a string, or "" for an empty s.
func lastCodePoint(s string) string {
	for back := min(4, len(s)); back > 0; back-- {
		if _, size := str.DecodeRune(s[len(s)-back:]); size == back {
			return s[len(s)-back:]
		}
	}
	return ""
}

// orDefault returns `value or fallback`.
func orDefault(value, fallback any) any {
	if json.Truthy(value) {
		return value
	}
	return fallback
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
