// Package bundle publishes complete owner-installed runtime bundles outside the plugin cache.
//
// The Python runtime module copied the Python sources and compiled their bytecode. The Go bundle holds the
// running agent-decisions executable instead, plus the launchers, the skill and the plugin manifests from
// the plugin root. The safety rules are unchanged: no symlinks in the source, a verified copy in a private
// stage directory, a final check that the source did not change, and an atomic pointer swap that never
// removes an older bundle.
//
// py: agent_decisions/runtime.py
package bundle

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// Binary is the bundle path of the agent-decisions executable.
const Binary = "bin/agent-decisions"

// sourceFiles are the bundle files copied from the plugin root, in the order Python sorts their paths.
var sourceFiles = []string{
	".claude-plugin/plugin.json",
	".codex-plugin/plugin.json",
	"bin/agent-decisions-install",
	"bin/agent-decisions-isolate",
	"bin/agent-decisions-shell-prefix",
	"bin/ts-decide",
	"skills/deciding/SKILL.md",
}

// Side effects that tests replace.
var (
	random     io.Reader = rand.Reader
	replace              = os.Rename // os.replace
	writeBytes           = writeFile // Path.write_bytes
	// executable returns the resolved path of the running agent-decisions binary, which becomes
	// bin/agent-decisions in every bundle.
	executable = func() (string, error) {
		path, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(path)
	}
)

func runtimeError(format string, args ...any) error {
	return &pyerr.Error{Class: "RuntimeError", Msg: fmt.Sprintf(format, args...)}
}

// Namespace returns the directory that holds every bundle and the current pointer. An empty home means
// the user's home directory.
//
// py: agent_decisions/runtime.py namespace
func Namespace(home string) (string, error) {
	if home == "" {
		var err error
		if home, err = homeDir(); err != nil {
			return "", err
		}
	}
	base, err := absolute(pathlib.Str(home))
	if err != nil {
		return "", err
	}
	return pathlib.Join(base, ".local/share/agent-decisions"), nil
}

// Current returns the pointer to the active bundle.
//
// py: agent_decisions/runtime.py current
func Current(home string) (string, error) {
	namespace, err := Namespace(home)
	if err != nil {
		return "", err
	}
	return pathlib.Join(namespace, "current"), nil
}

// origin is one bundle file and the file it is copied from.
type origin struct {
	relative string
	path     string
}

// files checks the bundle sources and returns them in bundle order. Every source file and each of its
// parents up to source must not be a symlink. The executable is already resolved.
//
// py: agent_decisions/runtime.py _files
func files(source string) ([]origin, error) {
	binary, err := executable()
	if err != nil {
		return nil, pyerr.NewOSError(err)
	}
	var out []origin
	for _, relative := range sourceFiles {
		path := pathlib.Join(source, relative)
		for candidate := path; ; candidate = pathlib.Parent(candidate) {
			if isSymlink(candidate) {
				return nil, runtimeError("runtime source must not contain symlinks: %s", candidate)
			}
			if candidate == source {
				break
			}
		}
		if !isFile(path) {
			return nil, runtimeError("runtime source file is missing: %s", relative)
		}
		out = append(out, origin{relative, path})
		if relative == ".codex-plugin/plugin.json" { // bin/agent-decisions sorts here
			out = append(out, origin{Binary, binary})
		}
	}
	if !isFile(binary) {
		return nil, runtimeError("runtime source file is missing: %s", Binary)
	}
	return out, nil
}

// Publish verifies a complete copy of the bundle files, then atomically activates it without removing old
// bundles. source is the plugin root. It returns the current pointer.
//
// py: agent_decisions/runtime.py publish
func Publish(source, home string) (current string, err error) {
	if source, err = absolute(pathlib.Str(source)); err != nil {
		return "", err
	}
	sources, err := files(source)
	if err != nil {
		return "", err
	}
	contents := make([][]byte, len(sources))
	for i, file := range sources {
		if contents[i], err = readBytes(file.path); err != nil {
			return "", err
		}
	}
	root, err := Namespace(home)
	if err != nil {
		return "", err
	}
	bundles := pathlib.Join(root, "runtimes")
	for _, directory := range []string{root, bundles} {
		if isSymlink(directory) {
			return "", runtimeError("runtime directory must not be a symlink: %s", directory)
		}
		if err := pathlib.Mkdir(directory, 0o700, true); err != nil {
			return "", err
		}
	}
	stage, err := makeStage(bundles)
	if err != nil {
		return "", err
	}
	pointerID, err := uuid4Hex()
	if err != nil {
		return "", err
	}
	pointer := pathlib.Join(root, ".current-"+pointerID)
	defer func() {
		// A failed cleanup replaces the original error, as an exception raised in a finally block does.
		if cleanupErr := cleanup(pointer, stage); cleanupErr != nil {
			current, err = "", cleanupErr
		}
	}()
	for i, file := range sources {
		target := pathlib.Join(stage, file.relative)
		if err := pathlib.Mkdir(pathlib.Parent(target), 0o700, true); err != nil {
			return "", err
		}
		if err := writeBytes(target, contents[i]); err != nil {
			return "", err
		}
		mode := uint32(0o600)
		if strings.HasPrefix(file.relative, "bin/") {
			mode = 0o700
		}
		if err := syscall.Chmod(target, mode); err != nil {
			return "", pyerr.NewOSError(err, target)
		}
		copied, err := readBytes(target)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(copied, contents[i]) {
			return "", runtimeError("runtime copy verification failed: %s", file.relative)
		}
	}
	again, err := files(source)
	if err != nil {
		return "", err
	}
	changed := len(again) != len(sources)
	for i := 0; !changed && i < len(sources); i++ {
		changed = again[i] != sources[i]
	}
	for i := 0; !changed && i < len(sources); i++ {
		data, err := readBytes(sources[i].path)
		if err != nil {
			return "", err
		}
		changed = !bytes.Equal(data, contents[i])
	}
	if changed {
		return "", runtimeError("runtime source changed during publication")
	}
	bundleID, err := uuid4Hex()
	if err != nil {
		return "", err
	}
	bundle := pathlib.Join(bundles, bundleID)
	if err := syscall.Rename(stage, bundle); err != nil {
		return "", pyerr.NewOSError(err, stage, bundle)
	}
	if err := syscall.Symlink("runtimes/"+bundleID, pointer); err != nil {
		return "", pyerr.NewOSError(err, "runtimes/"+bundleID, pointer)
	}
	if current, err = Current(home); err != nil {
		return "", err
	}
	if err := replace(pointer, current); err != nil {
		return "", pyerr.NewOSError(err, pointer, current)
	}
	return current, nil
}

// cleanup ports the finally block of publish: pointer.unlink(missing_ok=True), then shutil.rmtree(stage)
// when the stage still exists.
func cleanup(pointer, stage string) error {
	if err := syscall.Unlink(pointer); err != nil && err != syscall.ENOENT {
		return pyerr.NewOSError(err, pointer)
	}
	if _, err := os.Stat(stage); err == nil {
		if err := os.RemoveAll(stage); err != nil {
			return pyerr.NewOSError(err, stage)
		}
	}
	return nil
}

// makeStage ports tempfile.mkdtemp(prefix=".stage-", dir=bundles): a new 0o700 directory named
// ".stage-" plus 8 random characters.
//
// py: Lib/tempfile.py mkdtemp (Python 3.15)
func makeStage(dir string) (string, error) {
	const characters = "abcdefghijklmnopqrstuvwxyz0123456789_"
	for range 10000 { // tempfile.TMP_MAX
		var raw [8]byte
		if _, err := io.ReadFull(random, raw[:]); err != nil {
			return "", err
		}
		name := []byte(".stage-")
		for _, b := range raw {
			name = append(name, characters[int(b)%len(characters)])
		}
		path := pathlib.Join(dir, string(name))
		err := syscall.Mkdir(path, 0o700)
		if err == nil {
			return path, nil
		}
		if err != syscall.EEXIST {
			return "", pyerr.NewOSError(err, path)
		}
	}
	return "", &pyerr.OSError{Class: "FileExistsError", Msg: fmt.Sprintf(
		"[Errno %d] No usable temporary directory name found", int(syscall.EEXIST))}
}

// uuid4Hex returns uuid.uuid4().hex.
func uuid4Hex() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:]), nil
}

// writeFile ports Path.write_bytes: open with O_TRUNC and mode 0o666 before umask, then write all of data.
func writeFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return pyerr.NewOSError(err, path)
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return pyerr.NewOSError(err)
	}
	return nil
}

func readBytes(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, pyerr.NewOSError(err, path)
	}
	return data, nil
}

// isSymlink ports Path.is_symlink: any lstat error reads as false.
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// isFile ports Path.is_file: it follows symlinks, and any stat error reads as false.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// homeDir ports Path.home.
//
// py: Lib/pathlib/__init__.py Path.home (Python 3.15)
func homeDir() (string, error) {
	dir, err := ospath.ExpandUser("~")
	if err != nil {
		return "", err
	}
	if dir == "~" {
		return "", &pyerr.Error{Class: "RuntimeError", Msg: "Could not determine home directory."}
	}
	return pathlib.Str(dir), nil
}

// absolute ports Path.absolute for a normalized path: a relative path joins the kernel's working
// directory.
//
// py: Lib/pathlib/__init__.py Path.absolute (Python 3.15)
func absolute(path string) (string, error) {
	if strings.HasPrefix(path, "/") {
		return path, nil
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	if path == "." {
		return cwd, nil
	}
	return pathlib.Str(cwd + "/" + path), nil
}
