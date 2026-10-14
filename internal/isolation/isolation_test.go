package isolation

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
)

// mainCommand is the first argument that makes the test binary run a main function instead of the tests:
//
//	isolation.test isolation-main CAPTURE PLATFORM MODULE COMMAND ARGS...
//
// CAPTURE "-" execs for real; any other value is a file that receives the exec as JSON instead, and the exec
// then returns. PLATFORM replaces sys.platform and MODULE the module file, unless they are "-". COMMAND is
// "isolate" or "shell-prefix". Arguments, not the environment, carry this, so the environment that the main
// function sees is exactly the one the test gives.
const mainCommand = "isolation-main"

func TestMain(m *testing.M) {
	if len(os.Args) > 5 && os.Args[1] == mainCommand {
		os.Exit(runMain(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6:]))
	}
	os.Exit(m.Run())
}

func runMain(capture, platformName, module, command string, args []string) int {
	if capture != "-" {
		execvpe = func(file string, argv []string, env *json.Object) error {
			items := make([]any, 0, len(env.Keys()))
			for _, key := range env.Keys() {
				items = append(items, []any{key, env.Value(key)})
			}
			cwd, err := syscall.Getwd()
			if err != nil {
				return err
			}
			text, err := json.Dumps(json.NewObject("file", file, "args", argv, "env", items, "cwd", cwd), json.Options{})
			if err != nil {
				return err
			}
			return os.WriteFile(capture, []byte(text), 0o644)
		}
	}
	if platformName != "-" {
		platform = platformName
	}
	if module != "-" {
		moduleFile = func() (string, error) { return module, nil }
	}
	main := MainIsolate
	if command == "shell-prefix" {
		main = MainShellPrefix
	}
	return main(args, os.Stdin, os.Stdout, os.Stderr)
}

type result struct {
	stdout, stderr string
	code           int
}

// spawn runs argv with exactly env, which may repeat a name or hold an entry without "=" (os/exec would
// remove those), and returns its output and exit status.
func spawn(t *testing.T, argv, env []string, dir, stdin string) result {
	t.Helper()
	var pipes [6]*os.File // stdin, stdout and stderr, each as a read end and a write end
	for i := 0; i < len(pipes); i += 2 {
		var err error
		if pipes[i], pipes[i+1], err = os.Pipe(); err != nil {
			t.Fatal(err)
		}
	}
	process, err := os.StartProcess(argv[0], argv, &os.ProcAttr{Dir: dir, Env: env,
		Files: []*os.File{pipes[0], pipes[3], pipes[5]}})
	pipes[0].Close()
	pipes[3].Close()
	pipes[5].Close()
	if err != nil {
		t.Fatalf("start %q: %v", argv, err)
	}
	timer := time.AfterFunc(60*time.Second, func() { process.Kill() })
	defer timer.Stop()
	go func() {
		io.WriteString(pipes[1], stdin)
		pipes[1].Close()
	}()
	stderr := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(pipes[4])
		stderr <- data
	}()
	stdout, _ := io.ReadAll(pipes[2])
	errOutput := <-stderr
	pipes[2].Close()
	pipes[4].Close()
	state, err := process.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return result{string(stdout), string(errOutput), state.ExitCode()}
}

// runCLI runs a main function of this package in a child process, as its command does.
func runCLI(t *testing.T, env []string, stdin, command string, args ...string) result {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{self, mainCommand, "-", "-", "-", command}, args...)
	return spawn(t, argv, env, "", stdin)
}

// tempDir returns a new temporary directory with its symlinks resolved, as StateTestCase's self.dir.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// useState points AGENT_DECISIONS_STATE at <dir>/state, as StateTestCase does, and returns dir.
func useState(t *testing.T) string {
	t.Helper()
	dir := tempDir(t)
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	return dir
}

func stateDir(t *testing.T) string {
	t.Helper()
	state, err := policy.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// environ returns this process's environment with extra, given as alternating names and values, replacing
// any entry of the same name. It is the _env helper of the Python tests.
func environ(extra ...string) []string {
	names := map[string]bool{}
	for i := 0; i < len(extra); i += 2 {
		names[extra[i]] = true
	}
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !names[name] {
			env = append(env, entry)
		}
	}
	for i := 0; i < len(extra); i += 2 {
		env = append(env, extra[i]+"="+extra[i+1])
	}
	return env
}

func loads(t *testing.T, text string) any {
	t.Helper()
	value, err := json.Loads(text)
	if err != nil {
		t.Fatalf("Loads(%q): %v", text, err)
	}
	return value
}

func dumps(t *testing.T, value any) string {
	t.Helper()
	text, err := json.Dumps(value, json.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// writeInstallation appends a configured installation entry whose spec is the JSON text spec.
func writeInstallation(t *testing.T, spec string) {
	t.Helper()
	entry := json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"time", "2026-10-11T00:00:00Z", "spec", loads(t, spec))
	if err := ledger.Append(entry); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// requireSandbox skips a live test where sandbox-exec cannot confine a child. That includes a process that
// already runs inside a sandbox, where sandbox_apply fails with EPERM.
func requireSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || !ospath.Exists("/usr/bin/sandbox-exec") {
		t.Skip("live sandbox proof needs macOS sandbox-exec")
	}
	probe := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)\n(allow default)", "/usr/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("sandbox-exec cannot apply a profile here: %v: %s", err, output)
	}
}

func run(t *testing.T, argv, env []string, stdin string) result {
	t.Helper()
	return spawn(t, argv, env, "", stdin)
}

// py: tests/test_isolation.py::test_stdio_confinement_blocks_profile_symlink_replacement
func TestStdioConfinementBlocksProfileSymlinkReplacement(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	target := filepath.Join(dir, "managed-cli.toml")
	writeFile(t, target, "fixture config", 0o644)
	profile := filepath.Join(dir, "cli.config.toml")
	if err := os.Symlink(target, profile); err != nil {
		t.Fatal(err)
	}
	argv, err := wrapArgv([]string{"/bin/rm", profile}, stateDir(t), true, false, []string{profile})
	if err != nil {
		t.Fatal(err)
	}
	result := run(t, argv, os.Environ(), "")
	if result.code == 0 {
		t.Errorf("rm of the protected profile succeeded: %+v", result)
	}
	if info, err := os.Lstat(profile); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("profile is no longer a symlink: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "fixture config" {
		t.Errorf("target = %q", data)
	}
}

// The Go command has no interpreter startup to poison, so this checks that PYTHONPATH changes nothing and
// that the signed /bin/cat server relays stdio unchanged.
//
// py: tests/test_isolation.py::test_stdio_bootstrap_ignores_pythonpath_before_reading_signed_recipe
func TestStdioBootstrapIgnoresPythonpathBeforeReadingSignedRecipe(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	poison := filepath.Join(dir, "pythonpath")
	mkdir(t, poison)
	marker := filepath.Join(dir, "bootstrap-leak")
	writeFile(t, filepath.Join(poison, "sitecustomize.py"), "import os, pathlib\n"+
		"try:\n"+
		"    pathlib.Path(os.environ['AGENT_DECISIONS_STATE']).joinpath('key').read_bytes()\n"+
		"except OSError:\n"+
		"    pass\n"+
		"else:\n"+
		"    pathlib.Path(os.environ['AD_BOOTSTRAP_MARKER']).write_text('private state read')\n", 0o644)
	state := stateDir(t)
	writeInstallation(t, `{"host": "codex", "state_root": `+dumps(t, state)+
		`, "servers": {"echo": {"command": "/bin/cat", "args": []}}, "claude_matchers": []}`)
	// The sentinel is test-only. It is never a real credential or real signing key.
	writeFile(t, filepath.Join(state, "key"), "test-only-private-state", 0o600)
	result := runCLI(t, environ("AGENT_DECISIONS_STATE", state, "PYTHONPATH", poison, "AD_BOOTSTRAP_MARKER", marker),
		"protocol", "isolate", "--server", "echo", "--host", "codex")
	if result.code != 0 {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if result.stdout != "protocol" {
		t.Errorf("stdout = %q", result.stdout)
	}
	if ospath.Exists(marker) {
		t.Error("startup code read private state before isolation")
	}
}

// py: tests/test_isolation.py::test_wrap_argv_uses_macos_sandbox_profile_without_broad_filesystem_denies
func TestWrapArgvUsesMacosSandboxProfileWithoutBroadFilesystemDenies(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS sandbox-exec profile shape is macOS-only")
	}
	dir := useState(t)
	state := filepath.Join(dir, "state")
	argv, err := WrapArgv([]string{"/bin/echo", "ok"}, state, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/sandbox-exec" || argv[1] != "-p" {
		t.Fatalf("argv = %q", argv)
	}
	profile := argv[2]
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(allow default)", `(subpath "` + state + `")`, "file-read* file-write*", "file-write*"} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, root) {
		t.Errorf("profile names the repository %s:\n%s", root, profile)
	}
	if tail := argv[len(argv)-2:]; tail[0] != "/bin/echo" || tail[1] != "ok" {
		t.Errorf("argv ends with %q", tail)
	}
}

// Python patches shutil.which to return None; an empty PATH directory has the same effect.
//
// py: tests/test_isolation.py::test_linux_without_supported_sandbox_raises_instead_of_running_unconfined
func TestLinuxWithoutSupportedSandboxRaisesInsteadOfRunningUnconfined(t *testing.T) {
	dir := useState(t)
	saved := platform
	platform = "linux"
	t.Cleanup(func() { platform = saved })
	empty := filepath.Join(dir, "empty")
	mkdir(t, empty)
	t.Setenv("PATH", empty)
	_, err := WrapArgv([]string{"/bin/echo", "ok"}, filepath.Join(dir, "state"), true, false)
	if pyerr.Class(err) != unsupportedErrorClass {
		t.Fatalf("err = %v, want UnsupportedIsolationError", err)
	}
}

// py: tests/test_isolation.py::test_live_sandbox_preserves_stdio_and_public_files_but_blocks_computed_private_paths
func TestLiveSandboxPreservesStdioAndPublicFilesButBlocksComputedPrivatePaths(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	private := filepath.Join(dir, "state")
	public := filepath.Join(dir, "public")
	mkdir(t, private)
	mkdir(t, public)
	writeFile(t, filepath.Join(private, "secret.txt"), "private-ledger-secret", 0o644)
	writeFile(t, filepath.Join(public, "public.txt"), "public-ok", 0o644)
	const script = `private="$AD_PART_A/$AD_PART_B"
printf 'STDIN=%s\n' "$(cat)"
printf 'PUBLIC_READ=%s\n' "$(cat "$AD_PUBLIC/public.txt")"
printf public-write-ok > "$AD_PUBLIC/out.txt"
if cat "$private/secret.txt" > /dev/null 2>&1; then echo PRIVATE_READ=allowed; else echo PRIVATE_READ=denied; fi
if (printf no > "$private/new.txt") 2> /dev/null; then echo PRIVATE_WRITE=allowed; else echo PRIVATE_WRITE=denied; fi
`
	argv, err := WrapArgv([]string{"/bin/sh", "-c", script}, private, true, false)
	if err != nil {
		t.Fatal(err)
	}
	env := environ("AD_PART_A", filepath.Dir(private), "AD_PART_B", filepath.Base(private), "AD_PUBLIC", public)
	result := run(t, argv, env, "ping")
	if result.code != 0 {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	for _, want := range []string{"STDIN=ping", "PUBLIC_READ=public-ok", "PRIVATE_READ=denied", "PRIVATE_WRITE=denied"} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, result.stdout)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(public, "out.txt")); string(data) != "public-write-ok" {
		t.Errorf("out.txt = %q", data)
	}
	if strings.Contains(result.stdout+result.stderr, "private-ledger-secret") {
		t.Error("the private secret leaked")
	}
}

// py: tests/test_isolation.py::test_read_only_mode_blocks_public_writes_for_interactive_exec
func TestReadOnlyModeBlocksPublicWritesForInteractiveExec(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	private := filepath.Join(dir, "state")
	public := filepath.Join(dir, "public")
	mkdir(t, private)
	mkdir(t, public)
	argv, err := WrapArgv([]string{"/bin/sh", "-c", `printf x > "$AD_PUBLIC/x"`}, private, true, true)
	if err != nil {
		t.Fatal(err)
	}
	result := run(t, argv, environ("AD_PUBLIC", public), "")
	if result.code == 0 {
		t.Errorf("the read-only write succeeded: %+v", result)
	}
	if ospath.Exists(filepath.Join(public, "x")) {
		t.Error("the read-only sandbox let a public write through")
	}
}

// py: tests/test_isolation.py::test_exec_command_mode_derives_private_root_from_signed_spec
func TestExecCommandModeDerivesPrivateRootFromSignedSpec(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	private := stateDir(t)
	public := filepath.Join(dir, "public")
	mkdir(t, private)
	mkdir(t, public)
	command := shlex.Join([]string{"/bin/sh", "-c", `printf x > "$AD_PUBLIC/x"`})
	writeInstallation(t, `{"host": "codex", "state_root": `+dumps(t, private)+
		`, "servers": {}, "claude_matchers": [], "previous_shell_prefix": null}`)
	result := runCLI(t, environ("AD_PUBLIC", public), "", "isolate",
		"exec", "--host", "codex", "--read-only", "--command", command)
	if result.code == 0 {
		t.Errorf("the read-only command succeeded: %+v", result)
	}
	if ospath.Exists(filepath.Join(public, "x")) {
		t.Error("the read-only exec let a public write through")
	}
}

// py: tests/test_isolation.py::test_latest_installation_spec_uses_only_hmac_valid_rows
func TestLatestInstallationSpecUsesOnlyHmacValidRows(t *testing.T) {
	dir := useState(t)
	want := filepath.Join(dir, "state")
	writeInstallation(t, `{"host": "codex", "state_root": `+dumps(t, want)+
		`, "servers": {}, "claude_matchers": [], "previous_shell_prefix": null}`)
	path := filepath.Join(stateDir(t), "sessions", "installation.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	row := loads(t, first).(*json.Object)
	row.Value("entry").(*json.Object).Value("spec").(*json.Object).Set("state_root", "/attacker/chosen")
	forged, err := json.Dumps(row, json.Options{SortKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString(forged + "\n"); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	spec, err := LatestInstallationSpec("codex")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Value("state_root"); got != want {
		t.Errorf("state_root = %v, want %s", got, want)
	}
}

// py: tests/test_isolation.py::test_stdio_launcher_uses_signed_recipe_env_cwd_and_prints_no_extra_protocol_text
func TestStdioLauncherUsesSignedRecipeEnvCwdAndPrintsNoExtraProtocolText(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	serverDir := filepath.Join(dir, "server")
	mkdir(t, serverDir)
	marker := filepath.Join(serverDir, "cwd-marker")
	const script = `printf ok > cwd-marker; printf '%s|%s' "$VISIBLE" "$(cat)"`
	writeInstallation(t, `{"host": "codex", "state_root": `+dumps(t, stateDir(t))+`,
		"servers": {"echo": {"command": "/bin/sh", "args": ["-c", `+dumps(t, script)+`],
			"env": {"VISIBLE": "ok", "SECRET_RECIPE": "do-not-print"},
			"env_vars": ["INHERITED_OK"], "cwd": `+dumps(t, serverDir)+`, "source": "test"}},
		"claude_matchers": [], "previous_shell_prefix": null}`)
	result := runCLI(t, environ("INHERITED_OK", "yes"), "hello", "isolate", "--server", "echo", "--host", "codex")
	if result.code != 0 {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if result.stdout != "ok|hello" {
		t.Errorf("stdout = %q", result.stdout)
	}
	if data, _ := os.ReadFile(marker); string(data) != "ok" {
		t.Errorf("cwd-marker = %q", data)
	}
	if strings.Contains(result.stdout+result.stderr, "do-not-print") {
		t.Error("the recipe env leaked into the protocol stream")
	}
}

// py: tests/test_isolation.py::test_shell_prefix_isolates_exact_match_and_passthrough_keeps_hook_state_access
func TestShellPrefixIsolatesExactMatchAndPassthroughKeepsHookStateAccess(t *testing.T) {
	requireSandbox(t)
	dir := useState(t)
	private := stateDir(t)
	mkdir(t, private)
	writeFile(t, filepath.Join(private, "secret.txt"), "private-ledger-secret", 0o644)
	priorLog := filepath.Join(dir, "prior.log")
	prior := filepath.Join(dir, "prior-prefix.sh")
	writeFile(t, prior, "#!/bin/sh\nprintf prior >> \"$AD_PRIOR_LOG\"\nexec /bin/zsh -lc \"$1\"\n", 0o700)

	const matchScript = `private="$AD_PART_A/$AD_PART_B"
if cat "$private/secret.txt" > /dev/null 2>&1; then echo PRIVATE_READ=allowed; else echo PRIVATE_READ=denied; fi
if (printf no > "$private/new.txt") 2> /dev/null; then echo PRIVATE_WRITE=allowed; else echo PRIVATE_WRITE=denied; fi
`
	matchArgv := []string{"/bin/sh", "-c", matchScript}
	const passScript = `cat "$AD_PART_A/$AD_PART_B/secret.txt" > /dev/null && echo HOOK_READ_OK`
	writeInstallation(t, `{"host": "claude", "state_root": `+dumps(t, private)+`, "servers": {},
		"claude_matchers": [`+dumps(t, matchArgv)+`], "previous_shell_prefix": `+dumps(t, prior)+`}`)
	env := environ("AD_PART_A", filepath.Dir(private), "AD_PART_B", filepath.Base(private), "AD_PRIOR_LOG", priorLog)

	matched := runCLI(t, env, "", "shell-prefix", shlex.Join(matchArgv))
	passed := runCLI(t, env, "", "shell-prefix", shlex.Join([]string{"/bin/sh", "-c", passScript}))

	if matched.code != 0 {
		t.Fatalf("matched exit %d: %s", matched.code, matched.stderr)
	}
	for _, want := range []string{"PRIVATE_READ=denied", "PRIVATE_WRITE=denied"} {
		if !strings.Contains(matched.stdout, want) {
			t.Errorf("matched stdout lacks %q:\n%s", want, matched.stdout)
		}
	}
	if passed.code != 0 {
		t.Fatalf("passthrough exit %d: %s", passed.code, passed.stderr)
	}
	if !strings.Contains(passed.stdout, "HOOK_READ_OK") {
		t.Errorf("passthrough stdout = %q", passed.stdout)
	}
	if data, _ := os.ReadFile(priorLog); string(data) != "priorprior" {
		t.Errorf("prior.log = %q", data)
	}
	if strings.Contains(matched.stdout+matched.stderr+passed.stdout+passed.stderr, "private-ledger-secret") {
		t.Error("the private secret leaked")
	}
}

// An exec failure during the PATH search names the encoded file as bytes, as os.execvpe does, and a
// permission error wins over a later missing file.
func TestExecvpeSearchErrors(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, filepath.Join(dir, "tool"), "", 0o644) // not executable
	env := json.NewObject("PATH", filepath.Join(dir, "missing")+":"+dir+":"+filepath.Join(dir, "missing2"))
	err := osExecvpe("tool", []string{"tool"}, env)
	var osError interface{ PyType() string }
	if !errors.As(err, &osError) || osError.PyType() != "PermissionError" ||
		err.Error() != "[Errno 13] Permission denied: b'"+dir+"/tool'" {
		t.Errorf("err = %v", err)
	}
	for _, c := range []struct {
		args []string
		env  *json.Object
		want string
	}{
		{[]string{"a\x00"}, json.NewObject(), "embedded null byte"},
		{[]string{""}, json.NewObject(), "execve: argv first element cannot be empty"},
		{[]string{"a"}, json.NewObject("", "x"), "illegal environment variable name"},
		{[]string{"a"}, json.NewObject("A=B", "x"), "illegal environment variable name"},
		{[]string{"a"}, json.NewObject("A", "\x00"), "embedded null byte"},
		{[]string{"a"}, json.NewObject("A", "\xed\xa0\x80"),
			`'utf-8' codec can't encode character '\ud800' in position 0: surrogates not allowed`},
	} {
		if err := osExecvpe("/nonexistent/x", c.args, c.env); err == nil || err.Error() != c.want {
			t.Errorf("execvpe(%q, %v) = %v, want %s", c.args, c.env.Keys(), err, c.want)
		}
	}
	if got := bytesRepr("it's \"\\\t\xff"); got != `b'it\'s "\\\t\xff'` {
		t.Errorf("bytesRepr = %s", got)
	}
}
