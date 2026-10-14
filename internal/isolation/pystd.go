package isolation

// The parts of CPython's os, pathlib, shutil and tempfile modules, and of its interpreter startup, that
// isolation.py depends on. They are here rather than in internal/py because only this module uses them.

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// isValueError reports whether Python's `except ValueError` catches err.
func isValueError(err error) bool {
	class := pyerr.Class(err)
	return class == "ValueError" || class == "UnicodeEncodeError"
}

// pathAbsolute returns str(Path(p).absolute()) for a p that pathlib.Str normalized.
//
// py: Lib/pathlib/__init__.py Path.absolute (Python 3.15)
func pathAbsolute(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return p, nil
	}
	cwd, err := syscall.Getwd() // os.getcwd() is the kernel's path, not $PWD
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	if p == "." {
		return cwd, nil
	}
	return pathlib.Join(cwd, p), nil
}

// globSuffix returns Path(dir).glob("*" + suffix) for a dir that pathlib.Str normalized and an ASCII
// suffix without "/" or glob characters. Names come in directory order, as os.scandir lists them.
//
// py: Lib/glob.py _StringGlobber.wildcard_selector (Python 3.15)
func globSuffix(dir, suffix string) []string {
	handle, err := os.Open(dir + "/")
	if err != nil {
		return nil
	}
	defer handle.Close()
	names, err := handle.Readdirnames(-1)
	if err != nil {
		return nil // list(os.scandir(...)) raised, and the selector skips the whole directory
	}
	var paths []string
	for _, name := range names {
		if strings.HasSuffix(name, suffix) {
			paths = append(paths, pathlib.Join(dir, name))
		}
	}
	return paths
}

// chdir ports os.chdir(path).
func chdir(path string) error {
	encoded, err := pathArg("chdir", path)
	if err != nil {
		return err
	}
	if err := os.Chdir(encoded); err != nil {
		return pyerr.NewOSError(err, path)
	}
	return nil
}

// environCopy returns os.environ.copy() as a dict of str. CPython builds os.environ from the C environ:
// it skips entries without "=", keeps the first of duplicate names, and sees the LC_CTYPE that its
// startup coerced.
//
// py: Modules/posixmodule.c convertenviron (Python 3.15)
func environCopy() *json.Object {
	env := json.NewObject()
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if _, seen := env.Get(key); found && !seen {
			env.Set(key, value)
		}
	}
	coerceCLocale(env)
	return env
}

// coerceCLocale applies PEP 538 as CPython's startup does: when LC_ALL is unset or empty and the LC_CTYPE
// locale is C, POSIX or unknown to the C library, CPython sets LC_CTYPE=C.UTF-8 in its environment.
// An existing LC_CTYPE keeps its position.
//
// py: Python/pylifecycle.c _Py_LegacyLocaleDetected, _coerce_default_locale_settings (Python 3.15)
func coerceCLocale(env *json.Object) {
	if all, _ := env.Value("LC_ALL").(string); all != "" {
		return
	}
	name := "C"
	for _, key := range []string{"LC_CTYPE", "LANG"} {
		if value, _ := env.Value(key).(string); value != "" {
			name = value
			break
		}
	}
	if name == "C" || name == "POSIX" || !knownLocale(env, name) {
		env.Set("LC_CTYPE", "C.UTF-8")
	}
}

// knownLocale reports whether setlocale(LC_CTYPE, name) succeeds. On macOS the C library loads
// $PATH_LOCALE/<name>/LC_CTYPE, by default from /usr/share/locale, and rejects names with "/".
// ponytail: glibc's locale archive is not modelled, so on Linux every other name counts as installed;
// read the archive if a Linux host runs with an uninstalled LANG.
func knownLocale(env *json.Object, name string) bool {
	if runtime.GOOS != "darwin" { // the C library of this host, not sys.platform
		return true
	}
	if strings.Contains(name, "/") || name == "." || name == ".." {
		return false
	}
	dir, _ := env.Value("PATH_LOCALE").(string)
	if dir == "" {
		dir = "/usr/share/locale"
	}
	return ospath.Exists(dir + "/" + name + "/LC_CTYPE")
}

// osExecvpe ports os.execvpe(file, args, env): a file without "/" is searched in the PATH of env.
//
// py: Lib/os.py _execvpe (Python 3.15)
func osExecvpe(file string, args []string, env *json.Object) error {
	if strings.Contains(file, "/") {
		return execve(file, args, env)
	}
	file, err := pyerr.Fsencode(file)
	if err != nil {
		return err
	}
	path, ok := env.Value("PATH").(string)
	if !ok {
		path = "/bin:/usr/bin" // os.defpath
	}
	var saved, last error
	for _, dir := range strings.Split(path, ":") {
		fullname := file
		switch {
		case strings.HasSuffix(dir, "/"):
			fullname = dir + file
		case dir != "":
			fullname = dir + "/" + file
		}
		err := execve(fullname, args, env)
		var osError *pyerr.OSError
		if !errors.As(err, &osError) {
			return err
		}
		// Python execs the fsencoded name, so the exception names a bytes filename.
		err = &pyerr.OSError{Class: osError.Class, Msg: pyerr.NewOSError(osError.Err).Msg + ": " + bytesRepr(fullname),
			Err: osError.Err}
		last = err
		var errno syscall.Errno
		if errors.As(err, &errno) && errno != syscall.ENOENT && errno != syscall.ENOTDIR && saved == nil {
			saved = err
		}
	}
	if saved != nil {
		return saved
	}
	return last
}

// bytesRepr returns repr(b) for the bytes b.
//
// py: Objects/bytesobject.c PyBytes_Repr (Python 3.15)
func bytesRepr(b string) string {
	quote := byte('\'')
	if strings.Contains(b, "'") && !strings.Contains(b, `"`) {
		quote = '"'
	}
	out := []byte{'b', quote}
	for i := range len(b) {
		switch c := b[i]; {
		case c == quote || c == '\\':
			out = append(out, '\\', c)
		case c == '\t':
			out = append(out, `\t`...)
		case c == '\n':
			out = append(out, `\n`...)
		case c == '\r':
			out = append(out, `\r`...)
		case c < ' ' || c >= 0x7f:
			out = fmt.Appendf(out, `\x%02x`, c)
		default:
			out = append(out, c)
		}
	}
	return string(append(out, quote))
}

// execve ports os.execve(path, args, env). It checks the arguments in CPython's order, then ignores
// SIGPIPE and SIGXFSZ, which CPython ignores at startup and which the new program inherits.
//
// py: Modules/posixmodule.c os_execve_impl (Python 3.15)
func execve(path string, args []string, env *json.Object) error {
	encodedPath, err := pathArg("execve", path)
	if err != nil {
		return err
	}
	argv := make([]string, len(args))
	for i, arg := range args {
		if argv[i], err = fsConverter(arg); err != nil {
			return err
		}
	}
	if len(argv) == 0 {
		return &pyerr.Error{Class: "ValueError", Msg: "execve: argv must not be empty"}
	}
	if argv[0] == "" {
		return &pyerr.Error{Class: "ValueError", Msg: "execve: argv first element cannot be empty"}
	}
	var envp []string
	for _, key := range env.Keys() {
		name, err := fsConverter(key)
		if err != nil {
			return err
		}
		if name == "" || strings.Contains(name[1:], "=") {
			return &pyerr.Error{Class: "ValueError", Msg: "illegal environment variable name"}
		}
		value, err := fsConverter(env.Value(key).(string))
		if err != nil {
			return err
		}
		envp = append(envp, name+"="+value)
	}
	signal.Ignore(syscall.SIGPIPE, syscall.SIGXFSZ)
	return pyerr.NewOSError(syscall.Exec(encodedPath, argv, envp), path)
}

// pathArg ports the path_t converter of a posix function: the path, encoded, without a NUL.
func pathArg(function, path string) (string, error) {
	encoded, err := pyerr.Fsencode(path)
	if err != nil {
		return "", err
	}
	if strings.Contains(encoded, "\x00") {
		return "", &pyerr.Error{Class: "ValueError", Msg: function + ": embedded null character in path"}
	}
	return encoded, nil
}

// fsConverter ports PyUnicode_FSConverter: the encoded str, without a NUL.
func fsConverter(s string) (string, error) {
	encoded, err := pyerr.Fsencode(s)
	if err == nil && strings.Contains(encoded, "\x00") {
		err = &pyerr.Error{Class: "ValueError", Msg: "embedded null byte"}
	}
	return encoded, err
}

// which ports shutil.which(cmd) for a cmd without "/".
//
// py: Lib/shutil.py which (Python 3.15)
func which(cmd string) string {
	path, ok := os.LookupEnv("PATH")
	if !ok {
		path = "/bin:/usr/bin" // os.confstr("CS_PATH") of glibc
		if runtime.GOOS == "darwin" {
			path = "/usr/bin:/bin:/usr/sbin:/sbin"
		}
	}
	if path == "" {
		return ""
	}
	seen := map[string]bool{}
	for _, dir := range strings.Split(path, ":") {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		name := cmd
		switch {
		case strings.HasSuffix(dir, "/"):
			name = dir + cmd
		case dir != "":
			name = dir + "/" + cmd
		}
		if info, err := os.Stat(name); err == nil && !info.IsDir() && syscall.Access(name, 1) == nil {
			return name // exists, os.access(X_OK) and not a directory
		}
	}
	return ""
}

// mkdtemp ports tempfile.mkdtemp(prefix=prefix): a new 0o700 directory in the first usable candidate
// temporary directory. The random part of the name is not Python's, as it is random in both.
// ponytail: CPython probes each candidate with a test file and caches the result; this tries the mkdir in
// each candidate instead, which picks the same directory unless one allows files but not directories.
//
// py: Lib/tempfile.py mkdtemp, _candidate_tempdir_list (Python 3.15)
func mkdtemp(prefix string) (string, error) {
	var candidates []string
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		if dir := os.Getenv(name); dir != "" {
			candidates = append(candidates, dir)
		}
	}
	candidates = append(candidates, "/tmp", "/var/tmp", "/usr/tmp")
	cwd, err := syscall.Getwd()
	if err != nil {
		cwd = "."
	}
	candidates = append(candidates, cwd)
	const characters = "abcdefghijklmnopqrstuvwxyz0123456789_"
	for _, dir := range candidates {
		if dir != "." {
			if dir, err = abspath(dir); err != nil {
				return "", err
			}
		}
		for range 100 {
			name := make([]byte, 8)
			for i := range name {
				name[i] = characters[rand.IntN(len(characters))]
			}
			path := pathlib.Join(dir, prefix+string(name))
			err := syscall.Mkdir(path, 0o700)
			if err == nil {
				return abspath(path)
			}
			if err != syscall.EEXIST {
				break
			}
		}
	}
	quoted := make([]string, len(candidates))
	for i, dir := range candidates {
		quoted[i] = str.Repr(dir)
	}
	return "", &pyerr.OSError{Class: "FileNotFoundError",
		Msg: "[Errno 2] No usable temporary directory found in [" + strings.Join(quoted, ", ") + "]"}
}

// abspath ports os.path.abspath(p).
//
// py: Lib/posixpath.py abspath (Python 3.15)
func abspath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", pyerr.NewOSError(err)
		}
		p = cwd + "/" + p
	}
	return normpath(p), nil
}

// normpath ports os.path.normpath(p) for an absolute p.
//
// py: Lib/posixpath.py normpath (Python 3.15)
func normpath(p string) string {
	root := "/"
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		root = "//"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, part)
		}
	}
	return root + strings.Join(parts, "/")
}
