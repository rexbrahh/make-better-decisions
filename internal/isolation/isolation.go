// Package isolation runs stdio MCP servers and opaque command sessions inside the filesystem isolator of
// the platform, as the signed installation recipe describes them.
//
// MainIsolate and MainShellPrefix are the agent-decisions-isolate and agent-decisions-shell-prefix
// commands. On success they replace the process with execve, as Python's os.execvpe does.
//
// py: agent_decisions/isolation.py
package isolation

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/py/argparse"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Side effects that tests replace.
var (
	// execvpe replaces the process. It returns only when the exec fails.
	execvpe = osExecvpe
	// moduleFile returns the file whose grandparent directory is the code root. Python uses
	// <root>/agent_decisions/isolation.py; the Go binary is <root>/bin/<name>, at the same depth.
	moduleFile = os.Executable
	// platform is Python's sys.platform on the hosts that agent-decisions supports.
	platform = runtime.GOOS
)

// The exception classes of the Python module. ConfigurationError and UnsupportedIsolationError are
// subclasses of IsolationError, which is a subclass of RuntimeError.
const (
	configurationErrorClass = "ConfigurationError"
	unsupportedErrorClass   = "UnsupportedIsolationError"
)

func init() {
	pyerr.Register("IsolationError", "RuntimeError")
	pyerr.Register(configurationErrorClass, "IsolationError")
	pyerr.Register(unsupportedErrorClass, "IsolationError")
}

func configurationError(msg string) error {
	return &pyerr.Error{Class: configurationErrorClass, Msg: msg, Module: "agent_decisions.isolation"}
}

func unsupportedIsolationError(msg string) error {
	return &pyerr.Error{Class: unsupportedErrorClass, Msg: msg, Module: "agent_decisions.isolation"}
}

// WrapArgv returns argv wrapped in the filesystem isolator of this platform. Python's defaults are
// protectCode true and readOnly false. protectedPaths are the protected_paths of _wrap_argv: more paths
// to deny writes to. wrap_argv passes none.
//
// py: agent_decisions/isolation.py wrap_argv, _wrap_argv
func WrapArgv(argv []string, privateRoot string, protectCode, readOnly bool, protectedPaths ...string) ([]string, error) {
	return wrapArgv(argv, privateRoot, protectCode, readOnly, protectedPaths)
}

// LatestInstallationSpec returns the spec of the newest verified installation entry for host.
//
// Every Python caller passes a host, so the host=None form is not ported.
//
// py: agent_decisions/isolation.py latest_installation_spec
func LatestInstallationSpec(host string) (*json.Object, error) {
	entries, err := ledger.Entries("installation", nil)
	if err != nil {
		return nil, err
	}
	for _, entry := range slices.Backward(entries) {
		if entry.Value("gate") != "installation" || entry.Value("action") != "configured" {
			continue
		}
		spec, ok := entry.Value("spec").(*json.Object)
		if !ok {
			continue
		}
		if spec.Value("host") != host {
			continue
		}
		if err := validateSpec(spec); err != nil {
			return nil, err
		}
		return spec, nil
	}
	return nil, configurationError("no signed installation recipe for host " + str.Repr(host))
}

// NormalizeArgv checks that argv is a non-empty list of non-empty strings and resolves its executable
// when that is a path. argv is a decoded JSON list or a []string.
//
// py: agent_decisions/isolation.py normalize_argv
func NormalizeArgv(argv any) ([]string, error) {
	items, ok := argv.([]any)
	if words, isWords := argv.([]string); isWords {
		items, ok = make([]any, len(words)), true
		for i, word := range words {
			items[i] = word
		}
	}
	if !ok || len(items) == 0 {
		return nil, configurationError("argv must be a non-empty list")
	}
	normalized := make([]string, 0, len(items))
	for _, item := range items {
		word, ok := item.(string)
		if !ok || word == "" {
			return nil, configurationError("argv items must be non-empty strings")
		}
		if len(normalized) == 0 {
			executable, err := normalizeExecutable(word)
			if err != nil {
				return nil, err
			}
			word = executable
		}
		normalized = append(normalized, word)
	}
	return normalized, nil
}

// CommandToArgv splits a shell command into its normalized argv. A nil argv is Python's None: the
// command is empty, does not split, or has no valid argv.
//
// py: agent_decisions/isolation.py command_to_argv
func CommandToArgv(command string) ([]string, error) {
	if command == "" {
		return nil, nil
	}
	words, err := shlex.Split(command)
	if err != nil {
		return nil, nil // shlex raises ValueError
	}
	argv, err := NormalizeArgv(words)
	if err != nil && (pyerr.IsInstance(err, "ValueError", configurationErrorClass)) {
		return nil, nil
	}
	return argv, err
}

// MainIsolate is the agent-decisions-isolate command. args excludes the program name. It returns the exit
// status, or does not return when the exec succeeds.
//
// py: agent_decisions/isolation.py main_isolate
func MainIsolate(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	parser := isolateParser(stdout, stderr)
	namespace, err := parser.ParseArgs(args)
	if err != nil {
		return err.(*argparse.Exit).Code
	}
	if namespace["mode"] == "exec" {
		err = execCommand(namespace["host"].(string), namespace["command"].(string), namespace["read_only"].(bool))
	} else {
		server, _ := namespace["server"].(string)
		host, _ := namespace["host"].(string)
		if server == "" || host == "" {
			return parser.Error("use --server NAME --host HOST or exec --host HOST --command COMMAND").Code
		}
		err = execServer(host, server)
	}
	if err == nil {
		return 1
	}
	if pyerr.IsInstance(err, "IsolationError") {
		io.WriteString(stderr, pyerr.EncodeBackslashReplace(fmt.Sprintf("agent-decisions-isolate: %s\n", err)))
		return 78
	}
	return uncaught(stderr, err)
}

// isolateParser builds the parser of main_isolate. The launcher runs python3 -I, which ignores
// PYTHON_COLORS.
//
// py: agent_decisions/isolation.py main_isolate
func isolateParser(stdout, stderr io.Writer) *argparse.Parser {
	hosts := []string{"claude", "codex"}
	run := &argparse.Parser{Prog: "agent-decisions-isolate exec", Actions: []*argparse.Action{
		argparse.HelpAction(),
		{Kind: argparse.Store, Options: []string{"--host"}, Dest: "host", Choices: hosts, Required: true},
		{Kind: argparse.Store, Options: []string{"--command"}, Dest: "command", Required: true},
		{Kind: argparse.StoreTrue, Options: []string{"--read-only"}, Dest: "read_only"},
	}}
	return &argparse.Parser{Prog: "agent-decisions-isolate", Stdout: stdout, Stderr: stderr, IgnoreEnvironment: true,
		Actions: []*argparse.Action{
			argparse.HelpAction(),
			{Kind: argparse.Parsers, Dest: "mode", Choices: []string{"exec"}, Parser: run,
				ChoiceHelp: "run an opaque command from the signed host spec"},
			{Kind: argparse.Store, Options: []string{"--server"}, Dest: "server"},
			{Kind: argparse.Store, Options: []string{"--host"}, Dest: "host", Choices: hosts},
		}}
}

// MainShellPrefix is the agent-decisions-shell-prefix command, which Claude Code runs around every Bash
// command. args excludes the program name. It returns the exit status, or does not return when the exec
// succeeds.
//
// py: agent_decisions/isolation.py main_shell_prefix
func MainShellPrefix(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		io.WriteString(stderr, "agent-decisions-shell-prefix: expected one command string\n")
		return 2
	}
	command := args[0]
	spec, err := LatestInstallationSpec("claude")
	if err != nil {
		if !pyerr.IsInstance(err, "IsolationError") {
			return uncaught(stderr, err)
		}
		return exited(stderr, execShell(command, ""))
	}
	previous, _ := spec.Value("previous_shell_prefix").(string)
	parsed, err := CommandToArgv(command)
	if err != nil {
		return uncaught(stderr, err)
	}
	matchers, ok := spec.Get("claude_matchers")
	if !ok {
		matchers = []any{}
	}
	matched := false
	if parsed != nil {
		if matched, err = matchesAny(parsed, matchers); err != nil {
			return uncaught(stderr, err)
		}
	}
	if !matched {
		return exited(stderr, execShell(command, previous))
	}
	target := prefixedCommandArgv(command, previous)
	protected, err := protectedPathsFromSpec(spec, nil)
	var isolated []string
	if err == nil {
		isolated, err = wrapArgv(target, spec.Value("state_root").(string), true, false, protected)
	}
	if err != nil {
		if !pyerr.IsInstance(err, "IsolationError") {
			return uncaught(stderr, err)
		}
		io.WriteString(stderr, pyerr.EncodeBackslashReplace(fmt.Sprintf("agent-decisions-shell-prefix: %s\n", err)))
		return 78
	}
	return exited(stderr, execvpe(isolated[0], isolated, environCopy()))
}

// exited returns the status of a main function after an exec that returned: 1, after printing the
// exception when the exec failed.
func exited(stderr io.Writer, err error) int {
	if err != nil {
		return uncaught(stderr, err)
	}
	return 1
}

// uncaught prints the last line of the traceback of an exception that escapes main, and returns 1, the
// status of an uncaught exception. The traceback frames are Python file positions and have no Go form.
func uncaught(stderr io.Writer, err error) int {
	pyerr.WriteUncaught(stderr, err)
	return 1
}

// py: agent_decisions/isolation.py _exec_server
func execServer(host, server string) error {
	spec, err := LatestInstallationSpec(host)
	if err != nil {
		return err
	}
	servers, ok := spec.Value("servers").(*json.Object)
	if !ok {
		return configurationError("no signed recipe for server " + str.Repr(server))
	}
	recipe, ok := servers.Get(server)
	if !ok {
		return configurationError("no signed recipe for server " + str.Repr(server))
	}
	argv, err := serverArgv(recipe)
	if err != nil {
		return err
	}
	env, err := serverEnv(recipe)
	if err != nil {
		return err
	}
	// serverArgv checked that recipe is a dict.
	if cwd := recipe.(*json.Object).Value("cwd"); cwd != nil {
		dir, ok := cwd.(string)
		if !ok || dir == "" {
			return configurationError("invalid cwd for server " + str.Repr(server))
		}
		if err := chdir(dir); err != nil {
			return err
		}
	}
	protected, err := protectedPathsFromSpec(spec, recipe.(*json.Object))
	if err != nil {
		return err
	}
	isolated, err := wrapArgv(argv, spec.Value("state_root").(string), true, false, protected)
	if err != nil {
		return err
	}
	return execvpe(isolated[0], isolated, env)
}

// py: agent_decisions/isolation.py _exec_command
func execCommand(host, command string, readOnly bool) error {
	if command == "" {
		return configurationError("command must be non-empty")
	}
	spec, err := LatestInstallationSpec(host)
	if err != nil {
		return err
	}
	shellArgv := []string{shell(), "-lc", command}
	protected, err := protectedPathsFromSpec(spec, nil)
	if err != nil {
		return err
	}
	isolated, err := wrapArgv(shellArgv, spec.Value("state_root").(string), true, readOnly, protected)
	if err != nil {
		return err
	}
	return execvpe(isolated[0], isolated, environCopy())
}

// py: agent_decisions/isolation.py _server_argv
func serverArgv(recipe any) ([]string, error) {
	object, ok := recipe.(*json.Object)
	if !ok {
		return nil, configurationError("server recipe must be an object")
	}
	command, ok := object.Value("command").(string)
	args, present := object.Get("args")
	if !present {
		args = []any{}
	}
	if !ok || command == "" {
		return nil, configurationError("server recipe needs a command")
	}
	argv := []string{command}
	list, ok := args.([]any)
	if !ok {
		return nil, configurationError("server recipe args must be a list of strings")
	}
	for _, item := range list {
		word, ok := item.(string)
		if !ok {
			return nil, configurationError("server recipe args must be a list of strings")
		}
		argv = append(argv, word)
	}
	return argv, nil
}

// serverEnv returns the environment of the server: this process's, updated with the recipe's "env".
//
// py: agent_decisions/isolation.py _server_env
func serverEnv(recipe any) (*json.Object, error) {
	env := environCopy()
	object, _ := recipe.(*json.Object) // serverArgv checked that recipe is a dict
	explicit, present := object.Get("env")
	if !present || explicit == nil {
		explicit = json.NewObject()
	}
	inherited, present := object.Get("env_vars")
	if !present || inherited == nil {
		inherited = []any{}
	}
	explicitMap, ok := explicit.(*json.Object)
	if ok {
		for _, key := range explicitMap.Keys() {
			if _, isString := explicitMap.Value(key).(string); !isString {
				ok = false
				break
			}
		}
	}
	if !ok {
		return nil, configurationError("server recipe env must be a string map")
	}
	names, ok := inherited.([]any)
	if ok {
		for _, name := range names {
			if _, isString := name.(string); !isString {
				ok = false
				break
			}
		}
	}
	if !ok {
		return nil, configurationError("server recipe env_vars must be a list of names")
	}
	for _, key := range explicitMap.Keys() {
		env.Set(key, explicitMap.Value(key))
	}
	return env, nil
}

// py: agent_decisions/isolation.py _wrap_argv
func wrapArgv(argv []string, privateRoot string, protectCode, readOnly bool, protectedPaths []string) ([]string, error) {
	child, err := NormalizeArgv(argv)
	if err != nil {
		return nil, err
	}
	if privateRoot == "" {
		return nil, configurationError("path must be a non-empty string")
	}
	private, err := absolutePath(privateRoot)
	if err != nil {
		return nil, err
	}
	protected, err := protectedWritePaths(protectCode, protectedPaths)
	if err != nil {
		return nil, err
	}
	switch {
	case platform == "darwin":
		const sandbox = "/usr/bin/sandbox-exec"
		if !ospath.Exists(sandbox) {
			return nil, unsupportedIsolationError("macOS sandbox-exec is unavailable")
		}
		return append([]string{sandbox, "-p", macosProfile(private, protected, readOnly)}, child...), nil
	case strings.HasPrefix(platform, "linux"):
		return linuxWrapArgv(child, private, protected, readOnly)
	}
	return nil, unsupportedIsolationError("unsupported platform " + str.Repr(platform))
}

// py: agent_decisions/isolation.py _macos_profile
func macosProfile(private string, protected []string, readOnly bool) string {
	lines := []string{"(version 1)", "(allow default)"}
	if readOnly {
		lines = append(lines, "(deny file-write*)")
	}
	lines = append(lines, fmt.Sprintf(`(deny file-read* file-write* (literal "%s") (subpath "%s"))`,
		scheme(private), scheme(private)))
	for _, path := range protected {
		lines = append(lines, fmt.Sprintf(`(deny file-write* (literal "%s") (subpath "%s"))`,
			scheme(path), scheme(path)))
	}
	return strings.Join(lines, "\n")
}

// py: agent_decisions/isolation.py _linux_wrap_argv
func linuxWrapArgv(argv []string, private string, protected []string, readOnly bool) ([]string, error) {
	bwrap := which("bwrap")
	if bwrap == "" {
		return nil, unsupportedIsolationError("Linux isolation needs bubblewrap or a supported Codex sandbox helper")
	}
	denied, err := mkdtemp("agent-decisions-deny-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(denied, 0); err != nil {
		return nil, pyerr.NewOSError(err, denied)
	}
	bind := "--bind"
	if readOnly {
		bind = "--ro-bind"
	}
	command := []string{bwrap, "--die-with-parent", bind, "/", "/", "--ro-bind", denied, private}
	for _, path := range protected {
		if !ospath.Exists(path) {
			return nil, unsupportedIsolationError("cannot protect missing Linux path " + path)
		}
		command = append(command, "--ro-bind", path, path)
	}
	return append(command, argv...), nil
}

// py: agent_decisions/isolation.py _protected_write_paths
func protectedWritePaths(protectCode bool, extraPaths []string) ([]string, error) {
	var paths []string
	home, err := pathlib.Home()
	if err != nil {
		return nil, err
	}
	codex := pathlib.Join(home, ".codex")
	claude := pathlib.Join(home, ".claude")
	for _, raw := range []string{
		pathlib.Join(codex, "sessions"),
		pathlib.Join(claude, "projects"),
		pathlib.Join(home, ".claude.json"),
		pathlib.Join(claude, "settings.json"),
		pathlib.Join(claude, "settings.local.json"),
		pathlib.Join(codex, "config.toml"),
	} {
		path, err := absolutePath(raw)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	for _, path := range globSuffix(codex, ".config.toml") { // Path.glob("*.config.toml")
		absolute, err := pathlib.Absolute(path)
		if err != nil {
			return nil, err
		}
		resolved, err := absolutePath(path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, absolute, resolved)
	}
	if protectCode {
		root, err := authorityCodeRoot()
		if err != nil {
			return nil, err
		}
		if root != "" {
			paths = append(paths, root)
		}
	}
	for _, path := range extraPaths {
		if path == "" {
			return nil, configurationError("protected_paths must contain strings")
		}
		expanded, err := pathlib.ExpandUser(pathlib.Str(path))
		if err != nil {
			return nil, err
		}
		absolute, err := pathlib.Absolute(expanded)
		if err != nil {
			return nil, err
		}
		resolved, err := absolutePath(path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, absolute, resolved)
	}
	return dedupePaths(paths), nil
}

// protectedPathsFromSpec returns the protected_paths of spec and then of recipe. A nil recipe is
// Python's None.
//
// py: agent_decisions/isolation.py _protected_paths_from_spec
func protectedPathsFromSpec(spec, recipe *json.Object) ([]string, error) {
	var paths []string
	if recipe == nil {
		recipe = json.NewObject()
	}
	for _, source := range []*json.Object{spec, recipe} {
		raw, present := source.Get("protected_paths")
		if !present {
			raw = []any{}
		}
		if raw == nil {
			continue
		}
		list, ok := raw.([]any)
		if !ok {
			return nil, configurationError("protected_paths must be a list of strings")
		}
		for _, item := range list {
			path, ok := item.(string)
			if !ok {
				return nil, configurationError("protected_paths must be a list of strings")
			}
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// authorityCodeRoot returns the installed code root that commands must not write, or "" for a git
// checkout. A plugin cache install protects every cached version.
//
// py: agent_decisions/isolation.py _authority_code_root
func authorityCodeRoot() (string, error) {
	file, err := moduleFile()
	if err != nil {
		return "", err
	}
	resolved, err := ospath.Realpath(pathlib.Str(file), false)
	if err != nil {
		return "", err
	}
	root := pathlib.Parent(pathlib.Parent(pathlib.Str(resolved)))
	parent := pathlib.Parent(root)
	if pathlib.Name(parent) == "agent-decisions" && pathlib.Name(pathlib.Parent(pathlib.Parent(parent))) == "cache" {
		return parent, nil
	}
	if ospath.Exists(pathlib.Join(root, ".git")) {
		return "", nil
	}
	return root, nil
}

// py: agent_decisions/isolation.py _validate_spec
func validateSpec(spec *json.Object) error {
	if _, ok := spec.Value("host").(string); !ok {
		return configurationError("installation spec needs host")
	}
	if root, ok := spec.Value("state_root").(string); !ok || root == "" {
		return configurationError("installation spec needs state_root")
	}
	if servers, present := spec.Get("servers"); present {
		if _, ok := servers.(*json.Object); !ok {
			return configurationError("installation spec servers must be an object")
		}
	}
	if matchers, present := spec.Get("claude_matchers"); present && !validMatchers(matchers) {
		return configurationError("installation spec claude_matchers must be argv lists")
	}
	if previous := spec.Value("previous_shell_prefix"); previous != nil {
		if prefix, ok := previous.(string); !ok || prefix == "" {
			return configurationError("previous_shell_prefix must be a string or null")
		}
	}
	_, err := protectedPathsFromSpec(spec, nil)
	return err
}

// py: agent_decisions/isolation.py _valid_matchers
func validMatchers(matchers any) bool {
	list, ok := matchers.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		parts, ok := item.([]any)
		if !ok || len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if word, ok := part.(string); !ok || word == "" {
				return false
			}
		}
	}
	return true
}

// py: agent_decisions/isolation.py _matches_any
func matchesAny(argv []string, matchers any) (bool, error) {
	list, ok := matchers.([]any)
	if !ok {
		return false, nil
	}
	for _, matcher := range list {
		normalized, err := NormalizeArgv(matcher)
		if err != nil {
			if pyerr.IsInstance(err, configurationErrorClass) {
				continue
			}
			return false, err
		}
		if slices.Equal(argv, normalized) {
			return true, nil
		}
	}
	return false, nil
}

// prefixedCommandArgv returns the argv that runs command through the previous shell prefix, or through
// the login shell when previousShellPrefix is "" (Python's None).
//
// py: agent_decisions/isolation.py _prefixed_command_argv
func prefixedCommandArgv(command, previousShellPrefix string) []string {
	if previousShellPrefix != "" {
		return []string{previousShellPrefix, command}
	}
	return []string{shell(), "-lc", command}
}

// py: agent_decisions/isolation.py _exec_shell
func execShell(command, previousShellPrefix string) error {
	argv := prefixedCommandArgv(command, previousShellPrefix)
	return execvpe(argv[0], argv, environCopy())
}

// py: agent_decisions/isolation.py _shell
func shell() string {
	if ospath.Exists("/bin/zsh") {
		return "/bin/zsh"
	}
	return "/bin/sh"
}

// absolutePath returns str(Path(value).expanduser().resolve()) for a non-empty value. Every Go caller
// checks for the empty string first, which Python reports as ConfigurationError.
//
// py: agent_decisions/isolation.py _absolute_path
func absolutePath(value string) (string, error) {
	expanded, err := pathlib.ExpandUser(pathlib.Str(value))
	if err != nil {
		return "", err
	}
	resolved, err := ospath.Realpath(expanded, false)
	if err != nil {
		return "", err
	}
	return pathlib.Str(resolved), nil
}

// py: agent_decisions/isolation.py _dedupe_paths
func dedupePaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

// py: agent_decisions/isolation.py _normalize_executable
func normalizeExecutable(value string) (string, error) {
	if !strings.Contains(value, "/") {
		return value, nil
	}
	return absolutePath(value)
}

// py: agent_decisions/isolation.py _scheme
func scheme(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`)
}
