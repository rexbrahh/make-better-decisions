package bundle

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/isolation"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
)

// fixture is a plugin root inside a removable cache directory, as in tests/test_runtime.py, plus a fake
// agent-decisions executable that publish copies in place of the running test binary.
type fixture struct {
	home, source, binary string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{home: dir, source: filepath.Join(dir, "plugin-cache", "1.1.8"), binary: filepath.Join(dir, "agent-decisions")}
	for _, relative := range sourceFiles {
		write(t, filepath.Join(f.source, relative), "fixture "+relative+"\n")
	}
	write(t, f.binary, "#!/bin/sh\necho binary\n")
	previous := executable
	executable = func() (string, error) { return f.binary, nil }
	t.Cleanup(func() { executable = previous })
	return f
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func current(t *testing.T, home string) string {
	t.Helper()
	path, err := Current(home)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func wantRuntimeError(t *testing.T, err error, prefix string) {
	t.Helper()
	var pyErr interface{ PyType() string }
	if !errors.As(err, &pyErr) || pyErr.PyType() != "RuntimeError" || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("error = %v, want RuntimeError %q", err, prefix)
	}
}

// Replaces tests/test_runtime.py::test_runtime_hook_and_launcher_survive_cache_removal, which ran the
// bundled Python hook and launcher. The Go bundle has no hook script: it must hold byte-equal copies of
// the executable and plugin files, with private modes, after the plugin cache is gone.
func TestRuntimeBundleSurvivesCacheRemoval(t *testing.T) {
	f := newFixture(t)
	active, err := Publish(f.source, f.home)
	if err != nil {
		t.Fatal(err)
	}
	if active != current(t, f.home) {
		t.Fatalf("Publish returned %q, want %q", active, current(t, f.home))
	}
	want := map[string][]byte{}
	for _, relative := range sourceFiles {
		data, _ := os.ReadFile(filepath.Join(f.source, relative))
		want[relative] = data
	}
	want[Binary], _ = os.ReadFile(f.binary)
	if err := os.RemoveAll(filepath.Dir(f.source)); err != nil {
		t.Fatal(err)
	}
	for relative, data := range want {
		path := filepath.Join(active, relative)
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s = %q, %v; want %q", relative, got, err, data)
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(relative, "bin/") {
			mode = 0o700
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s mode = %v, %v; want %v", relative, info.Mode().Perm(), err, mode)
		}
	}
}

// Replaces tests/test_runtime.py::test_bundle_ships_checked_bytecode_for_every_module and
// test_hook_import_skips_urllib, which checked Python bytecode and imports. The Go bundle holds exactly
// the executable and the plugin files: no Python modules, no hook script, no bytecode.
func TestBundleHoldsExactlyTheExecutableAndPluginFiles(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.source, "hooks/hook.py"), "unused\n")
	write(t, filepath.Join(f.source, "agent_decisions/hook.py"), "unused\n")
	active, err := Publish(f.source, f.home)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = filepath.WalkDir(resolve(t, active), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			relative, _ := filepath.Rel(resolve(t, active), path)
			got = append(got, relative)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{Binary}, sourceFiles...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("bundle files = %q, want %q", got, want)
	}
}

// Replaces the default executable check: a missing executable is rejected like a missing source file.
func TestMissingExecutableIsRejected(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(f.binary); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(f.source, f.home)
	wantRuntimeError(t, err, "runtime source file is missing: bin/agent-decisions")
	if exists(current(t, f.home)) {
		t.Fatal("current exists after a rejected publish")
	}
}

// py: tests/test_runtime.py::test_changed_source_is_rejected_before_activation
func TestChangedSourceIsRejectedBeforeActivation(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { writeBytes = writeFile })
	writeBytes = func(path string, data []byte) error {
		err := writeFile(path, data)
		if filepath.Base(path) == "SKILL.md" {
			write(t, filepath.Join(f.source, ".codex-plugin/plugin.json"), "changed after copy")
		}
		return err
	}
	_, err := Publish(f.source, f.home)
	wantRuntimeError(t, err, "runtime source changed during publication")
	if exists(current(t, f.home)) {
		t.Fatal("current exists after a rejected publish")
	}
}

// The Go bundle has no hooks/hook.py, so the pointer write targets .claude-plugin/plugin.json instead.
//
// py: tests/test_runtime.py::test_native_sandbox_denies_pointer_replacement_and_bundle_writes
func TestNativeSandboxDeniesPointerReplacementAndBundleWrites(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native runtime protection proof requires macOS")
	}
	probe := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)\n(allow default)", "/usr/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("sandbox-exec cannot apply a profile here: %v: %s", err, output)
	}
	t.Setenv("AGENT_DECISIONS_STATE", t.TempDir())
	f := newFixture(t)
	active, err := Publish(f.source, f.home)
	if err != nil {
		t.Fatal(err)
	}
	const script = `pointer=$1
resolved=$(cd -P "$pointer" && pwd -P)
for operation in 'rm -f "$pointer"' 'printf tamper > "$pointer/.claude-plugin/plugin.json"' \
		'printf tamper > "$resolved/bin/ts-decide"'; do
	if (eval "$operation") 2> /dev/null; then echo allowed; else echo denied; fi
done
`
	state, err := policy.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := Namespace(f.home)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := isolation.WrapArgv([]string{"/bin/sh", "-c", script, "sh", active}, state, true, false, namespace)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command := exec.Command(argv[0], argv[1:]...)
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n"); !slices.Equal(got, []string{"denied", "denied", "denied"}) {
		t.Fatalf("operations = %q, want three denials", got)
	}
}

// py: tests/test_runtime.py::test_failed_activation_retains_previous_bundle
func TestFailedActivationRetainsPreviousBundle(t *testing.T) {
	f := newFixture(t)
	active, err := Publish(f.source, f.home)
	if err != nil {
		t.Fatal(err)
	}
	old := resolve(t, active)
	write(t, filepath.Join(f.source, "bin/ts-decide"), "#!/bin/sh\nexit 1\n")
	replace = func(string, string) error { return syscall.EINTR }
	_, err = Publish(f.source, f.home)
	replace = os.Rename
	var pyErr interface{ PyType() string }
	if !errors.As(err, &pyErr) || pyErr.PyType() != "InterruptedError" {
		t.Fatalf("error = %v, want InterruptedError", err)
	}
	if resolve(t, active) != old {
		t.Fatal("failed activation moved the current pointer")
	}
	if !exists(filepath.Join(old, "bin/ts-decide")) {
		t.Fatal("previous bundle lost bin/ts-decide")
	}
	if _, err := Publish(f.source, f.home); err != nil {
		t.Fatal(err)
	}
	if resolve(t, active) == old {
		t.Fatal("second publish did not move the current pointer")
	}
	if info, err := os.Stat(old); err != nil || !info.IsDir() {
		t.Fatal("previous bundle was removed")
	}
	entries, _ := filepath.Glob(filepath.Join(filepath.Dir(active), ".current-*"))
	stages, _ := filepath.Glob(filepath.Join(filepath.Dir(active), "runtimes", ".stage-*"))
	if len(entries)+len(stages) != 0 {
		t.Fatalf("leftover pointers %q and stages %q", entries, stages)
	}
}

// py: tests/test_runtime.py::test_source_symlinks_and_missing_entrypoints_are_rejected
func TestSourceSymlinksAndMissingEntrypointsAreRejected(t *testing.T) {
	cases := []struct{ name, path, message string }{
		{"file", "bin/ts-decide", "runtime source must not contain symlinks: "},
		{"directory", "skills/deciding", "runtime source must not contain symlinks: "},
		{"missing", "bin/ts-decide", "runtime source file is missing: bin/ts-decide"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			path := filepath.Join(f.source, c.path)
			outside := filepath.Join(f.home, "outside")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if c.name != "missing" {
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Publish(f.source, f.home)
			wantRuntimeError(t, err, c.message)
			if exists(current(t, f.home)) {
				t.Fatal("current exists after a rejected publish")
			}
		})
	}
}

// py: tests/test_runtime.py::test_runtime_namespace_cannot_redirect_bundle_writes
func TestRuntimeNamespaceCannotRedirectBundleWrites(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.home, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	namespace, err := Namespace(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(namespace), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, namespace); err != nil {
		t.Fatal(err)
	}
	_, err = Publish(f.source, f.home)
	wantRuntimeError(t, err, "runtime directory must not be a symlink: "+namespace)
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("outside holds %v", entries)
	}
}

func TestNamespaceUsesHomeAndWorkingDirectory(t *testing.T) {
	t.Setenv("HOME", "/home/owner")
	if got, _ := Namespace(""); got != "/home/owner/.local/share/agent-decisions" {
		t.Errorf("Namespace(\"\") = %q", got)
	}
	cwd, _ := syscall.Getwd()
	if got, _ := Current("rel//dir/."); got != cwd+"/rel/dir/.local/share/agent-decisions/current" {
		t.Errorf("Current(relative) = %q", got)
	}
}
