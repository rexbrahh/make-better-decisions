// Package install ports the installer: it plans and applies host-level agent-decisions isolation for Codex
// and Claude Code.
//
// A plan is the *json.Object that Python builds, key for key, so the dry-run output and the settings it
// writes match byte for byte. An empty home argument is Python's home=None (the user's home directory).
//
// py: agent_decisions/install.py
package install

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/bundle"
	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/isolation"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/argparse"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	pyos "github.com/rexbrahh/make-better-decisions/internal/py/os"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

const (
	ProfileName          = "agent-decisions-isolation"
	CodexBinary          = "/opt/homebrew/bin/codex"
	ClaudeSettingsSource = "claude:settings.mcpServers"
)

func defaultCodexBinary() string {
	if _, err := exec.LookPath(CodexBinary); err == nil {
		return CodexBinary
	}
	return "codex"
}

// NativeServers are the Codex servers whose own launchers stay unwrapped.
var NativeServers = map[string]bool{
	"codex_app": true, "adrafinil": true, "node_repl": true, "cua_repl": true, "computer-history": true,
	"computer-use": true, "event-stream": true, "mcp-search": true, "messages": true,
}

var (
	secretKeyParts = []string{"api_key", "apikey", "access_token", "auth_token", "bearer_token", "credential",
		"password", "private_key", "secret", "token"}
	secretCLIFlags = map[string]bool{"api-key": true, "api_key": true, "token": true, "password": true,
		"secret": true, "bearer-token": true, "bearer_token": true, "access-token": true, "access_token": true}
	writeTools        = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}
	runtimeServerKeys = map[string]bool{"auth_status": true, "disabled_reason": true, "name": true, "source": true}
)

const redacted = "<redacted>"

// cliSecretFlag is the flag alternation of _CLI_SECRET_EQUALS and _CLI_SECRET_NEXT. Python's IGNORECASE
// also matches "i" with "ı" (U+0131) and "İ" (U+0130), which Go's (?i) does not, so the "i" of "api" lists
// them. Go's (?i) already matches "k" with the Kelvin sign and "s" with "ſ", as Python does.
const cliSecretFlag = `--(?:ap[iI\x{131}\x{130}][-_]?key|access[-_]?token|bearer[-_]?token|token|password|secret)`

var (
	// py: agent_decisions/install.py _CLI_SECRET_EQUALS (?i)(--(...)=)([^\s]+)
	cliSecretEquals = regexp.MustCompile(`(?i)(` + cliSecretFlag + `=)(` + re.NotSpace + `+)`)
	// py: agent_decisions/install.py _CLI_SECRET_NEXT (?i)(--(...)\s+)([^\s]+)
	cliSecretNext = regexp.MustCompile(`(?i)(` + cliSecretFlag + re.Space + `+)(` + re.NotSpace + `+)`)
	// urlToken is _URL_TOKEN without its leading \b, which extractRedactedURLs checks by hand because RE2's
	// \b is ASCII. The class splices the body of re.Space into [^\s'"<>].
	//
	// py: agent_decisions/install.py _URL_TOKEN (?i)\bhttps?://[^\s'"<>]+
	urlToken = regexp.MustCompile(`(?i)https?://[^` + re.Space[1:len(re.Space)-1] + `'"<>]+`)
)

// Side effects that tests replace.
var (
	now   = time.Now
	newID = uuid4 // str(uuid.uuid4())
	// executable returns the resolved path of the running binary, as Path(__file__).resolve() did.
	executable = func() (string, error) {
		path, err := os.Executable()
		if err != nil {
			return "", err
		}
		return ospath.Realpath(path, false)
	}
	// defaultAppServerSocket ports _default_app_server_socket; tests patch it as the Python tests do.
	defaultAppServerSocket = appServerSocket
)

func init() { pyerr.Register("InstallError", "RuntimeError") }

// installError is install.InstallError, a RuntimeError subclass.
//
// py: agent_decisions/install.py InstallError
func installError(msg string) error {
	return &pyerr.Error{Class: "InstallError", Msg: msg, Module: "agent_decisions.install"}
}

// DefaultInstallRoot returns the plugin root. The bootstrap launcher sets AGENT_DECISIONS_SOURCE to it;
// without that variable it is the directory two levels above the resolved executable
// (<root>/bin/agent-decisions), as Python used the directory above its own package.
//
// py: agent_decisions/install.py default_install_root
func DefaultInstallRoot() (string, error) {
	if source := os.Getenv("AGENT_DECISIONS_SOURCE"); source != "" {
		return pathlib.Str(source), nil
	}
	path, err := executable()
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	return pathlib.Parent(pathlib.Parent(pathlib.Str(path))), nil
}

// CanonicalPath returns the user-expanded, symlink-resolved form of a path.
//
// py: agent_decisions/install.py canonical_path
func CanonicalPath(value string) (string, error) {
	if value == "" {
		return "", installError("path must be a non-empty string")
	}
	expanded, err := pathlib.ExpandUser(value)
	if err != nil {
		return "", err
	}
	resolved, err := ospath.Realpath(expanded, false)
	if err != nil {
		return "", err
	}
	return pathlib.Str(resolved), nil
}

// py: agent_decisions/install.py shell_prefix_path
func ShellPrefixPath(installRoot string) (string, error) {
	return launcherPath(installRoot, "bin/agent-decisions-shell-prefix")
}

// py: agent_decisions/install.py isolate_path
func IsolatePath(installRoot string) (string, error) {
	return launcherPath(installRoot, "bin/agent-decisions-isolate")
}

func launcherPath(installRoot, relative string) (string, error) {
	root, err := pathlib.ExpandUser(installRoot)
	if err != nil {
		return "", err
	}
	return pathlib.Absolute(pathlib.Join(root, relative))
}

// ClaudeConfigRoots returns each Claude config dir with its global .claude.json.
//
// CLAUDE_CONFIG_DIR selects a dir per launch, so the installer also covers every ~/.claude-* dir that holds
// a .claude.json, not only the one set in its own shell.
//
// py: agent_decisions/install.py claude_config_roots
func ClaudeConfigRoots(home string) ([][2]string, error) {
	base, err := baseDir(home)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, config := range globClaudeConfigs(base) {
		dirs = append(dirs, pathlib.Parent(config))
	}
	if selected := os.Getenv("CLAUDE_CONFIG_DIR"); selected != "" {
		dir, err := pathlib.ExpandUser(selected)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
	roots := [][2]string{{pathlib.Join(base, ".claude"), pathlib.Join(base, ".claude.json")}}
	for _, root := range dirs {
		roots = append(roots, [2]string{root, pathlib.Join(root, ".claude.json")})
	}
	return roots, nil
}

// py: agent_decisions/install.py host_config_paths
func HostConfigPaths(host, home string) ([]string, error) {
	base, err := baseDir(home)
	if err != nil {
		return nil, err
	}
	switch host {
	case "codex":
		config, err := CanonicalPath(pathlib.Join(base, ".codex/config.toml"))
		if err != nil {
			return nil, err
		}
		paths := []string{config}
		for _, path := range globCodexProfiles(pathlib.Join(base, ".codex")) {
			absolutePath, err := pathlib.Absolute(path)
			if err != nil {
				return nil, err
			}
			paths = append(paths, absolutePath)
		}
		return paths, nil
	case "claude":
		roots, err := ClaudeConfigRoots(home)
		if err != nil {
			return nil, err
		}
		var paths []string
		for _, pair := range roots {
			root, config := pair[0], pair[1]
			for _, path := range []string{pathlib.Join(root, "settings.json"), pathlib.Join(root, "settings.local.json"),
				config, pathlib.Join(root, "plugins/installed_plugins.json")} {
				canonical, err := CanonicalPath(path)
				if err != nil {
					return nil, err
				}
				paths = append(paths, canonical)
			}
		}
		return paths, nil
	}
	return nil, installError("host must be codex or claude")
}

// py: agent_decisions/install.py transcript_roots
func TranscriptRoots(host, home string) ([]string, error) {
	base, err := baseDir(home)
	if err != nil {
		return nil, err
	}
	switch host {
	case "codex":
		sessions, err := CanonicalPath(pathlib.Join(base, ".codex/sessions"))
		if err != nil {
			return nil, err
		}
		return []string{sessions}, nil
	case "claude":
		roots, err := ClaudeConfigRoots(home)
		if err != nil {
			return nil, err
		}
		var paths []string
		for _, pair := range roots {
			projects, err := CanonicalPath(pathlib.Join(pair[0], "projects"))
			if err != nil {
				return nil, err
			}
			paths = append(paths, projects)
		}
		return paths, nil
	}
	return nil, installError("host must be codex or claude")
}

// AuthorityCodeRoot returns the code that must stay read-only, or "" (Python's None) for a git checkout.
//
// py: agent_decisions/install.py authority_code_root
func AuthorityCodeRoot(installRoot string) (string, error) {
	root, err := CanonicalPath(installRoot)
	if err != nil {
		return "", err
	}
	parent := pathlib.Parent(root)
	if pathlib.Name(parent) == "agent-decisions" && pathlib.Name(pathlib.Parent(pathlib.Parent(parent))) == "cache" {
		return parent, nil // running chats can still invoke earlier cached versions
	}
	if ospath.Exists(pathlib.Join(root, ".git")) {
		return "", nil
	}
	return root, nil
}

// py: agent_decisions/install.py protected_write_paths
func ProtectedWritePaths(host, installRoot, home string) ([]string, error) {
	namespace, err := bundle.Namespace(home)
	if err != nil {
		return nil, err
	}
	paths := []string{namespace}
	codeRoot, err := AuthorityCodeRoot(installRoot)
	if err != nil {
		return nil, err
	}
	if codeRoot != "" {
		paths = append(paths, codeRoot)
	}
	transcripts, err := TranscriptRoots(host, home)
	if err != nil {
		return nil, err
	}
	configs, err := HostConfigPaths(host, home)
	if err != nil {
		return nil, err
	}
	return dedupe(slices.Concat(paths, transcripts, configs))
}

// py: agent_decisions/install.py protected_path_reasons
func ProtectedPathReasons(host, stateRoot, installRoot, home string) (*json.Object, error) {
	state, err := CanonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	reasons := json.NewObject(state, "private_state_deny_read_write")
	namespace, err := bundle.Namespace(home)
	if err != nil {
		return nil, err
	}
	runtimePaths, err := dedupe([]string{namespace})
	if err != nil {
		return nil, err
	}
	for _, path := range runtimePaths {
		reasons.Set(path, "installed_runtime_deny_write")
	}
	codeRoot, err := AuthorityCodeRoot(installRoot)
	if err != nil {
		return nil, err
	}
	if codeRoot != "" {
		reasons.Set(codeRoot, "installed_authority_code_deny_write")
	}
	transcripts, err := TranscriptRoots(host, home)
	if err != nil {
		return nil, err
	}
	for _, path := range transcripts {
		reasons.Set(path, "host_transcripts_deny_write")
	}
	configs, err := HostConfigPaths(host, home)
	if err != nil {
		return nil, err
	}
	for _, path := range configs {
		protected, err := dedupe([]string{path})
		if err != nil {
			return nil, err
		}
		for _, path := range protected {
			reasons.Set(path, "host_authority_config_deny_write")
		}
	}
	return reasons, nil
}

// ParseCodexConfigSummary reads the few top-level Codex settings that the permission profile builds on.
//
// py: agent_decisions/install.py parse_codex_config_summary
func ParseCodexConfigSummary(text string) *json.Object {
	profileExtends := json.NewObject()
	summary := json.NewObject("default_permissions", nil, "profile_extends", profileExtends,
		"sandbox_mode", nil, "workspace_network", nil)
	section := ""
	for _, rawLine := range str.SplitLines(text) {
		line := str.Strip(stripTomlComment(rawLine))
		if line == "" {
			continue
		}
		// ASCII brackets: byte slicing equals code point slicing here.
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = parseSection(str.Strip(str.Slice(line, 1, -1)))
			continue
		}
		key, value, ok := parseAssignment(line)
		if !ok {
			continue
		}
		parsed := parseScalar(value)
		scalar, isText := parsed.(string)
		flag, isBool := parsed.(bool)
		switch {
		case section == "" && key == "default_permissions" && isText:
			summary.Set("default_permissions", scalar)
		case section == "" && key == "sandbox_mode" && isText:
			summary.Set("sandbox_mode", scalar)
		case section == "sandbox_workspace_write" && key == "network_access" && isBool:
			summary.Set("workspace_network", flag)
		case strings.HasPrefix(section, "permissions.") && key == "extends" && isText:
			_, profile, _ := strings.Cut(section, ".") // "." is ASCII
			profileExtends.Set(profile, scalar)
		}
	}
	return summary
}

// ReadCodexConfigSummary reads ~/.codex/config.toml, or path when it is not nil. A missing file reads as
// empty. Python reads it with universal newlines, which split into the same lines.
//
// py: agent_decisions/install.py read_codex_config_summary
func ReadCodexConfigSummary(path *string) (*json.Object, error) {
	var target string
	if path == nil {
		home, err := pathlib.Home()
		if err != nil {
			return nil, err
		}
		target = pathlib.Join(home, ".codex/config.toml")
	} else {
		target = pathlib.Str(*path)
	}
	data, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, syscall.ENOENT) {
		return nil, pyerr.NewOSError(err, target)
	}
	return ParseCodexConfigSummary(json.DecodeUTF8(data)), nil
}

// py: agent_decisions/install.py codex_permission_profile_value
func CodexPermissionProfileValue(stateRoot, installRoot string, currentConfig *json.Object, home string) (*json.Object, error) {
	current := json.NewObject()
	if currentConfig != nil {
		current = currentConfig.Copy()
	}
	profileExtends, ok := current.Value("profile_extends").(*json.Object)
	if !ok {
		profileExtends = json.NewObject()
	}
	var baseProfile string
	currentDefault := current.Value("default_permissions")
	if currentDefault == ProfileName {
		preservedBase, ok := profileExtends.Value(ProfileName).(string)
		if ok && preservedBase != "" && preservedBase != ProfileName {
			baseProfile = preservedBase
		} else {
			baseProfile = legacySandboxProfile(current.Value("sandbox_mode"))
		}
	} else if text, ok := currentDefault.(string); ok && text != "" {
		baseProfile = text
	} else {
		baseProfile = legacySandboxProfile(current.Value("sandbox_mode"))
	}

	state, err := CanonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	filesystem := json.NewObject(state, "deny")
	protected, err := ProtectedWritePaths("codex", installRoot, home)
	if err != nil {
		return nil, err
	}
	for _, path := range protected {
		filesystem.Set(path, "read")
	}

	value := json.NewObject(
		"description", "Deny agent access to the agent-decisions private ledger. Keep protected host files read-only.",
		"extends", baseProfile,
		"filesystem", filesystem,
	)
	if network, ok := current.Value("workspace_network").(bool); ok && baseProfile == ":workspace" {
		value.Set("network", json.NewObject("enabled", network))
	}
	return value, nil
}

// CodexConfigEdits returns the config/batchWrite edits: the permission profile, then one wrapper per
// stdio server.
//
// py: agent_decisions/install.py codex_config_edits
func CodexConfigEdits(servers []*json.Object, stateRoot, installRoot string, currentConfig *json.Object,
	home string, previousSpec *json.Object) ([]any, error) {
	profileKey, err := configKeyPath("permissions", ProfileName)
	if err != nil {
		return nil, err
	}
	profile, err := CodexPermissionProfileValue(stateRoot, installRoot, currentConfig, home)
	if err != nil {
		return nil, err
	}
	edits := []any{json.NewObject("keyPath", profileKey, "mergeStrategy", "upsert", "value", profile)}
	current, err := bundle.Current(home)
	if err != nil {
		return nil, err
	}
	for _, server := range servers {
		name, value, err := CodexMcpWrapperConfig(server, current, previousSpec)
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		key, err := configKeyPath("mcp_servers", name)
		if err != nil {
			return nil, err
		}
		edits = append(edits, json.NewObject("keyPath", key, "mergeStrategy", "upsert", "value", dropNone(value)))
	}
	return edits, nil
}

// CodexConfigRpc wraps edits in a config/batchWrite request. A nil filePath writes the default config.
//
// py: agent_decisions/install.py codex_config_rpc
func CodexConfigRpc(edits []any, filePath *string) (*json.Object, error) {
	params := json.NewObject("edits", edits, "reloadUserConfig", true)
	if filePath != nil {
		path, err := CanonicalPath(*filePath)
		if err != nil {
			return nil, err
		}
		params.Set("filePath", path)
	}
	return json.NewObject("method", "config/batchWrite", "params", params), nil
}

// ReadCodexServers runs `codex mcp list --json` with the installer's stdin. Python decodes both pipes as
// strict UTF-8 before it checks the exit status; its universal newlines do not change what json.loads
// accepts.
//
// py: agent_decisions/install.py read_codex_servers
func ReadCodexServers(codexBinary string) ([]*json.Object, error) {
	command := exec.Command(codexBinary, "mcp", "list", "--json")
	var stdout, stderr bytes.Buffer
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, &stdout, &stderr
	err := command.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, spawnError(err, codexBinary)
	}
	text, decodeErr := json.DecodeUTF8Strict(stdout.Bytes())
	if decodeErr != nil {
		return nil, decodeErr
	}
	if _, decodeErr := json.DecodeUTF8Strict(stderr.Bytes()); decodeErr != nil {
		return nil, decodeErr
	}
	if exitErr != nil {
		return nil, installError("codex mcp list --json failed")
	}
	if text == "" {
		text = "[]"
	}
	parsed, err := json.Loads(text)
	if err != nil {
		if pyerr.IsInstance(err, "ValueError") {
			return nil, installError("codex mcp list --json returned invalid JSON")
		}
		return nil, err
	}
	return serverList(parsed)
}

// py: agent_decisions/install.py read_codex_servers_file
func ReadCodexServersFile(path string) ([]*json.Object, error) {
	text, err := pathlib.ReadText(pathlib.Str(path))
	if err != nil {
		return nil, err
	}
	parsed, err := json.Loads(text)
	if err != nil {
		return nil, err
	}
	return serverList(parsed)
}

// PreviousInstallationSpec returns the newest signed installation spec of host, or nil when there is
// none or it cannot be read.
//
// py: agent_decisions/install.py previous_installation_spec
func PreviousInstallationSpec(host string) *json.Object {
	spec, err := isolation.LatestInstallationSpec(host)
	if err != nil {
		return nil
	}
	return spec
}

// py: agent_decisions/install.py codex_mcp_recipes
func CodexMcpRecipes(servers []*json.Object, installRoot string, previousSpec *json.Object) (*json.Object, error) {
	recipes := json.NewObject()
	for _, server := range servers {
		recipe, err := ServerRecipe(server, installRoot, previousSpec)
		if err != nil {
			return nil, err
		}
		if recipe == nil {
			continue
		}
		name, err := serverName(server)
		if err != nil {
			return nil, err
		}
		recipes.Set(name, recipe)
	}
	return recipes, nil
}

// ServerRecipe returns the signed launch recipe of a stdio server, or nil for any other transport.
//
// py: agent_decisions/install.py server_recipe
func ServerRecipe(server *json.Object, installRoot string, previousSpec *json.Object) (*json.Object, error) {
	name, err := serverName(server)
	if err != nil {
		return nil, err
	}
	transport := transport(server)
	if isCodexWrapperTransport(transport, name) {
		previous := previousServerRecipe(previousSpec, name)
		if previous == nil {
			return nil, installError(fmt.Sprintf("server %s is already wrapped but no signed recipe exists", str.Repr(name)))
		}
		return previous, nil
	}
	if transport.Value("type") != "stdio" {
		return nil, nil
	}
	command, ok := transport.Value("command").(string)
	if !ok || command == "" {
		return nil, nil
	}
	args := transport.ValueOr("args", []any{})
	env := transport.ValueOr("env", json.NewObject())
	envVars := transport.ValueOr("env_vars", []any{})
	cwd := transport.Value("cwd")
	if args == nil {
		args = []any{}
	}
	if env == nil {
		env = json.NewObject()
	}
	if envVars == nil {
		envVars = []any{}
	}
	argList, ok := stringList(args)
	if !ok {
		return nil, installError(fmt.Sprintf("server %s args must be a list of strings", str.Repr(name)))
	}
	envMap, ok := stringMap(env)
	if !ok {
		return nil, installError(fmt.Sprintf("server %s env must be a string map", str.Repr(name)))
	}
	envVarList, ok := stringList(envVars)
	if !ok {
		return nil, installError(fmt.Sprintf("server %s env_vars must be a list of names", str.Repr(name)))
	}
	if _, ok := cwd.(string); cwd != nil && !ok {
		return nil, installError(fmt.Sprintf("server %s cwd must be a string", str.Repr(name)))
	}
	source, err := serverSource(server)
	if err != nil {
		return nil, err
	}
	return json.NewObject("command", command, "args", slices.Clone(argList), "env", envMap.Copy(),
		"env_vars", slices.Clone(envVarList), "cwd", cwd, "source", source), nil
}

// CodexMcpWrapperConfig returns the Codex config of a stdio server routed through the isolate launcher, or
// a nil value for a transport it leaves alone. A native server keeps its own launcher, restored from the
// signed recipe when an older install wrapped it.
//
// py: agent_decisions/install.py codex_mcp_wrapper_config
func CodexMcpWrapperConfig(server *json.Object, installRoot string, previousSpec *json.Object) (string, *json.Object, error) {
	transport := transport(server)
	if transport.Value("type") != "stdio" {
		return "", nil, nil
	}
	name, err := serverName(server)
	if err != nil {
		return "", nil, err
	}
	native := NativeServers[name]
	if native && !isCodexWrapperTransport(transport, name) {
		return "", nil, nil
	}
	value := json.NewObject()
	for _, key := range server.Keys() {
		if item := server.Value(key); !runtimeServerKeys[key] && key != "transport" && item != nil {
			value.Set(key, deepCopy(item))
		}
	}
	for _, key := range transport.Keys() {
		if item := transport.Value(key); key != "type" && item != nil {
			value.Set(key, deepCopy(item))
		}
	}
	if native {
		recipe, err := ServerRecipe(server, installRoot, previousSpec)
		if err != nil {
			return "", nil, err
		}
		for _, key := range []string{"command", "args", "env", "env_vars", "cwd"} {
			value.Set(key, deepCopy(recipe.Value(key)))
		}
	} else {
		isolate, err := IsolatePath(installRoot)
		if err != nil {
			return "", nil, err
		}
		value.Set("command", isolate)
		value.Set("args", []any{"--server", name, "--host", "codex"})
	}
	return name, value, nil
}

// BuildCodexPlan returns the Codex plan. A nil configPath is Python's config_path=None.
//
// py: agent_decisions/install.py build_codex_plan
func BuildCodexPlan(servers []*json.Object, stateRoot, installRoot string, currentConfig *json.Object,
	configPath *string, home string, previousSpec *json.Object) (*json.Object, error) {
	state, err := CanonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	root, err := CanonicalPath(installRoot)
	if err != nil {
		return nil, err
	}
	recipes, err := CodexMcpRecipes(servers, root, previousSpec)
	if err != nil {
		return nil, err
	}
	protected, err := ProtectedWritePaths("codex", root, home)
	if err != nil {
		return nil, err
	}
	edits, err := CodexConfigEdits(servers, state, root, currentConfig, home, previousSpec)
	if err != nil {
		return nil, err
	}
	entry, err := InstallationEntry("codex", state, recipes, []any{}, nil, protected)
	if err != nil {
		return nil, err
	}
	if err := ConfigureSessionExceptions(entry, previousSpec, nil, nil); err != nil {
		return nil, err
	}
	plan, err := hostPlan("codex", state, root, home, protected, recipes)
	if err != nil {
		return nil, err
	}
	rpc, err := CodexConfigRpc(edits, configPath)
	if err != nil {
		return nil, err
	}
	plan.Set("config_rpc", rpc)
	plan.Set("installation_entry", entry)
	if configPath != nil {
		source, err := CanonicalPath(*configPath)
		if err != nil {
			return nil, err
		}
		plan.Set("backup_source", source)
	}
	return plan, nil
}

// hostPlan returns the keys that both plans start with, evaluated in Python's order.
func hostPlan(host, state, root, home string, protected []string, servers *json.Object) (*json.Object, error) {
	current, err := bundle.Current(home)
	if err != nil {
		return nil, err
	}
	reasons, err := ProtectedPathReasons(host, state, root, home)
	if err != nil {
		return nil, err
	}
	var runtimeHome any
	if home != "" {
		runtimeHome = pathlib.Str(home)
	}
	return json.NewObject("host", host, "state_root", state, "install_root", root, "runtime_root", current,
		"runtime_home", runtimeHome, "protected_paths", anyList(protected),
		"protected_path_reasons", reasons, "servers", servers), nil
}

// py: agent_decisions/install.py claude_servers_from_settings
func ClaudeServersFromSettings(settings *json.Object, source string) (*json.Object, error) {
	servers := json.NewObject()
	raw, ok := settings.ValueOr("mcpServers", json.NewObject()).(*json.Object)
	if !ok {
		return servers, nil
	}
	for _, name := range raw.Keys() {
		config, ok := raw.Value(name).(*json.Object)
		if !ok {
			continue
		}
		command, ok := config.Value("command").(string)
		if !ok || command == "" {
			continue
		}
		args := orDefault(config.ValueOr("args", []any{}), []any{})
		env := orDefault(config.ValueOr("env", json.NewObject()), json.NewObject())
		envVars := orDefault(config.ValueOr("env_vars", config.ValueOr("envVars", []any{})), []any{})
		cwd := config.Value("cwd")
		argList, ok := stringList(args)
		if !ok {
			return nil, installError(fmt.Sprintf("Claude server %s args must be a list of strings", str.Repr(name)))
		}
		envMap, ok := stringMap(env)
		if !ok {
			return nil, installError(fmt.Sprintf("Claude server %s env must be a string map", str.Repr(name)))
		}
		envVarList, ok := stringList(envVars)
		if !ok {
			return nil, installError(fmt.Sprintf("Claude server %s env_vars must be a list of names", str.Repr(name)))
		}
		if _, ok := cwd.(string); cwd != nil && !ok {
			return nil, installError(fmt.Sprintf("Claude server %s cwd must be a string", str.Repr(name)))
		}
		servers.Set(name, json.NewObject("command", command, "args", slices.Clone(argList), "env", envMap.Copy(),
			"env_vars", slices.Clone(envVarList), "cwd", cwd, "source", source))
	}
	return servers, nil
}

// Sourced is one (source, mapping) pair: a Claude config file or plugin manifest and its parsed JSON.
type Sourced struct {
	Source string
	Value  *json.Object
}

// py: agent_decisions/install.py claude_servers_from_sources
func ClaudeServersFromSources(settings *json.Object, claudeConfigs, pluginManifests []Sourced) (*json.Object, error) {
	servers, err := ClaudeServersFromSettings(settings, ClaudeSettingsSource)
	if err != nil {
		return nil, err
	}
	update := func(settings *json.Object, source string) error {
		found, err := ClaudeServersFromSettings(settings, source)
		if err != nil {
			return err
		}
		for _, name := range found.Keys() {
			servers.Set(name, found.Value(name))
		}
		return nil
	}
	for _, config := range claudeConfigs {
		if err := update(config.Value, config.Source); err != nil {
			return nil, err
		}
	}
	for _, manifest := range pluginManifests {
		if err := update(manifest.Value, manifest.Source); err != nil {
			return nil, err
		}
		if raw, ok := manifest.Value.Value("servers").(*json.Object); ok {
			if err := update(json.NewObject("mcpServers", raw), manifest.Source); err != nil {
				return nil, err
			}
		}
		_, hasMCP := manifest.Value.Get("mcpServers")
		_, hasServers := manifest.Value.Get("servers")
		if !hasMCP && !hasServers {
			if err := update(json.NewObject("mcpServers", manifest.Value), manifest.Source); err != nil {
				return nil, err
			}
		}
	}
	return servers, nil
}

// DiscoverClaudePluginManifests returns sorted, unique .mcp.json paths from supported registry entries.
//
// py: agent_decisions/install.py discover_claude_plugin_manifests
func DiscoverClaudePluginManifests(home string) ([]string, error) {
	base, err := baseDir(home)
	if err != nil {
		return nil, err
	}
	registry, err := readClaudePluginRegistry(base)
	if err != nil {
		return nil, err
	}
	plugins, ok := registry.Value("plugins").(*json.Object)
	if !ok {
		return []string{}, nil
	}
	manifests := []string{}
	for _, key := range plugins.Keys() {
		entries, ok := plugins.Value(key).([]any)
		if !ok {
			entries = []any{plugins.Value(key)}
		}
		for _, entry := range entries {
			installPath, err := claudePluginInstallPath(entry)
			if err != nil {
				return nil, err
			}
			if installPath == "" || claudePluginDisabled(entry) {
				continue
			}
			if manifest := pathlib.Join(installPath, ".mcp.json"); ospath.IsFile(manifest) {
				manifests = append(manifests, manifest)
			}
		}
	}
	sortPaths(manifests)
	return slices.Compact(manifests), nil
}

// py: agent_decisions/install.py read_claude_plugin_manifests
func ReadClaudePluginManifests(paths []string) ([]Sourced, error) {
	manifests := []Sourced{}
	for _, path := range paths {
		target := pathlib.Str(path)
		data, err := ReadJSON(target)
		if err != nil {
			return nil, err
		}
		root, err := CanonicalPath(pathlib.Parent(target))
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, Sourced{target, expandPluginRootMacros(data, root).(*json.Object)})
	}
	return manifests, nil
}

func selectedClaudeConfigRoot(home string) (string, error) {
	if selected := os.Getenv("CLAUDE_CONFIG_DIR"); selected != "" {
		return pathlib.ExpandUser(selected)
	}
	return pathlib.Join(home, ".claude"), nil
}

// readClaudePluginRegistry reads installed_plugins.json; a missing or unparsable registry is empty.
//
// py: agent_decisions/install.py _read_claude_plugin_registry
func readClaudePluginRegistry(home string) (*json.Object, error) {
	root, err := selectedClaudeConfigRoot(home)
	if err != nil {
		return nil, err
	}
	registry := pathlib.Join(root, "plugins/installed_plugins.json")
	text, err := pathlib.ReadText(registry)
	var data any
	if err == nil {
		data, err = json.Loads(text)
	}
	if err != nil {
		if pyerr.IsInstance(err, "FileNotFoundError", "ValueError") {
			return json.NewObject(), nil
		}
		return nil, err
	}
	if object, ok := data.(*json.Object); ok {
		return object, nil
	}
	return json.NewObject(), nil
}

// claudePluginInstallPath returns the expanded installPath of a registry entry, or "" for None.
//
// py: agent_decisions/install.py _claude_plugin_install_path
func claudePluginInstallPath(entry any) (string, error) {
	object, ok := entry.(*json.Object)
	if !ok {
		return "", nil
	}
	value, ok := object.Value("installPath").(string)
	if !ok || value == "" {
		return "", nil
	}
	return pathlib.ExpandUser(value)
}

// py: agent_decisions/install.py _claude_plugin_disabled
func claudePluginDisabled(entry any) bool {
	object, ok := entry.(*json.Object)
	if !ok {
		return false
	}
	if settings, ok := object.Value("settings").(*json.Object); ok && isFalse(settings.Value("enabledPlugins")) {
		return true
	}
	return isFalse(object.Value("enabledPlugins"))
}

// py: agent_decisions/install.py _expand_plugin_root_macros
func expandPluginRootMacros(value any, pluginRoot string) any {
	switch value := value.(type) {
	case string:
		// The macros are ASCII, so byte replacement is exact on WTF-8 text.
		value = strings.ReplaceAll(value, "${CLAUDE_PLUGIN_ROOT}", pluginRoot)
		return strings.ReplaceAll(value, "${PLUGIN_ROOT}", pluginRoot)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = expandPluginRootMacros(item, pluginRoot)
		}
		return out
	case *json.Object:
		out := json.NewObject()
		for _, key := range value.Keys() {
			out.Set(key, expandPluginRootMacros(value.Value(key), pluginRoot))
		}
		return out
	}
	return value
}

// ClaudeMatchersFromServers returns the argv of each server the Claude shell prefix must isolate.
//
// py: agent_decisions/install.py claude_matchers_from_servers
func ClaudeMatchersFromServers(servers *json.Object, ownerSources []string) ([]any, error) {
	matchers := []any{}
	for _, name := range servers.Keys() {
		recipe, ok := servers.Value(name).(*json.Object)
		if !ok {
			return nil, &pyerr.Error{Class: "AttributeError",
				Msg: fmt.Sprintf("'%s' object has no attribute 'get'", json.TypeName(servers.Value(name)))}
		}
		// A native name exempts only the owner's own config; a plugin manifest could claim any name.
		if source, ok := recipe.Value("source").(string); ok && NativeServers[name] && slices.Contains(ownerSources, source) {
			continue
		}
		command, ok := recipe.Value("command").(string)
		args, isList := recipe.ValueOr("args", []any{}).([]any)
		if !ok || !isList {
			continue
		}
		matcher := []any{command}
		for _, item := range args {
			// ponytail: recipes from ClaudeServersFromSettings hold only string args; Python's str() of other
			// JSON values is not ported.
			text, ok := item.(string)
			if !ok {
				return nil, &pyerr.Error{Class: "TypeError", Msg: "matcher args must be strings"}
			}
			matcher = append(matcher, text)
		}
		matchers = append(matchers, matcher)
	}
	return matchers, nil
}

// MergeClaudeSettings adds the shell prefix, sandbox denies and tool denies to a copy of settings. It also
// returns the shell prefix that was there before, or nil.
//
// py: agent_decisions/install.py merge_claude_settings
func MergeClaudeSettings(settings *json.Object, shellPrefix, stateRoot string, protectedPaths []string,
	previousSpec *json.Object, transcripts []string) (*json.Object, *string, error) {
	merged := deepCopy(settings).(*json.Object)
	env, ok := merged.Value("env").(*json.Object)
	if !ok {
		env = json.NewObject()
		merged.Set("env", env)
	}
	var previous *string
	if text, ok := env.Value("CLAUDE_CODE_SHELL_PREFIX").(string); ok {
		previous = &text
	}
	if previous != nil && (*previous == shellPrefix || pathlib.Name(pathlib.Str(*previous)) == "agent-decisions-shell-prefix") {
		previous = nil
		if previousSpec != nil {
			if signed, ok := previousSpec.Value("previous_shell_prefix").(string); ok && signed != shellPrefix {
				previous = &signed
			}
		}
	}
	env.Set("CLAUDE_CODE_SHELL_PREFIX", shellPrefix)

	sandbox := setDefaultObject(merged, "sandbox")
	filesystem := setDefaultObject(sandbox, "filesystem")

	state, err := CanonicalPath(stateRoot)
	if err != nil {
		return nil, nil, err
	}
	appendUnique(filesystem, "denyRead", state)
	appendUnique(filesystem, "denyWrite", state)
	canonicalProtected := make([]string, len(protectedPaths))
	for i, path := range protectedPaths {
		if canonicalProtected[i], err = CanonicalPath(path); err != nil {
			return nil, nil, err
		}
		appendUnique(filesystem, "denyWrite", canonicalProtected[i])
	}

	permissions := setDefaultObject(merged, "permissions")
	deny, ok := permissions.ValueOr("deny", nil).([]any)
	if !ok {
		deny = []any{}
	}
	deny, err = appendToolDenies(deny, state, true, true)
	if err != nil {
		return nil, nil, err
	}
	transcriptPaths := map[string]bool{}
	for _, path := range transcripts {
		canonical, err := CanonicalPath(path)
		if err != nil {
			return nil, nil, err
		}
		transcriptPaths[canonical] = true
	}
	for _, path := range canonicalProtected {
		if transcriptPaths[path] {
			deny, err = appendTranscriptDenies(deny, path) // the Bash sandbox above still denies the whole root
		} else {
			deny, err = appendToolDenies(deny, path, false, true)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	permissions.Set("deny", deny)
	return merged, previous, nil
}

// py: agent_decisions/install.py build_claude_plan
func BuildClaudePlan(settings *json.Object, stateRoot, installRoot, home string, claudeConfigs, pluginManifests []Sourced,
	previousSpec *json.Object) (*json.Object, error) {
	state, err := CanonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	root, err := CanonicalPath(installRoot)
	if err != nil {
		return nil, err
	}
	protected, err := ProtectedWritePaths("claude", root, home)
	if err != nil {
		return nil, err
	}
	current, err := bundle.Current(home)
	if err != nil {
		return nil, err
	}
	prefix, err := ShellPrefixPath(current)
	if err != nil {
		return nil, err
	}
	servers, err := ClaudeServersFromSources(settings, claudeConfigs, pluginManifests)
	if err != nil {
		return nil, err
	}
	transcripts, err := TranscriptRoots("claude", home)
	if err != nil {
		return nil, err
	}
	merged, previous, err := MergeClaudeSettings(settings, prefix, state, protected, previousSpec, transcripts)
	if err != nil {
		return nil, err
	}
	owners := []string{ClaudeSettingsSource}
	for _, config := range claudeConfigs {
		owners = append(owners, config.Source)
	}
	matchers, err := ClaudeMatchersFromServers(servers, owners)
	if err != nil {
		return nil, err
	}
	entry, err := InstallationEntry("claude", state, servers, matchers, previous, protected)
	if err != nil {
		return nil, err
	}
	if err := ConfigureSessionExceptions(entry, previousSpec, nil, nil); err != nil {
		return nil, err
	}
	plan, err := hostPlan("claude", state, root, home, protected, servers)
	if err != nil {
		return nil, err
	}
	plan.Set("claude_matchers", matchers)
	plan.Set("previous_shell_prefix", optional(previous))
	plan.Set("merged_settings", merged)
	plan.Set("installation_entry", entry)
	return plan, nil
}

// InstallationEntry returns the unsigned ledger entry that records an installation spec.
//
// py: agent_decisions/install.py installation_entry
func InstallationEntry(host, stateRoot string, servers *json.Object, claudeMatchers []any, previousShellPrefix *string,
	protectedPaths []string) (*json.Object, error) {
	state, err := CanonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	stamp := now().UTC().Format("2006-01-02T15:04:05Z")
	protected := make([]any, len(protectedPaths))
	for i, path := range protectedPaths {
		if protected[i], err = CanonicalPath(path); err != nil {
			return nil, err
		}
	}
	spec := json.NewObject("host", host, "state_root", state, "review_mode", "required",
		"servers", deepCopy(servers), "claude_matchers", deepCopy(claudeMatchers),
		"previous_shell_prefix", optional(previousShellPrefix), "protected_paths", protected)
	return json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"time", stamp, "spec", spec), nil
}

// py: agent_decisions/install.py sign_installation
func SignInstallation(entry *json.Object) error {
	return ledger.Append(deepCopy(entry).(*json.Object))
}

// ConfigureSessionExceptions sets the spec's session review exceptions: the previous ones plus add, minus
// remove, without duplicates.
//
// py: agent_decisions/install.py configure_session_exceptions
func ConfigureSessionExceptions(entry, previousSpec *json.Object, add, remove []string) error {
	var sessions any = []any{}
	if previousSpec != nil {
		sessions = previousSpec.ValueOr("session_review_exceptions", []any{})
	}
	list, ok := sessions.([]any)
	valid := ok
	for _, item := range slices.Concat(list, anyList(add), anyList(remove)) {
		text, isText := item.(string)
		valid = valid && isText && text != "" && str.Len(text) <= 128
	}
	if !valid {
		return installError("session review exceptions must contain exact session IDs")
	}
	kept := []any{}
	for _, item := range slices.Concat(list, anyList(add)) {
		if !slices.Contains(kept, item) && !slices.Contains(remove, item.(string)) {
			kept = append(kept, item)
		}
	}
	spec, ok := entry.Value("spec").(*json.Object)
	if !ok {
		return &pyerr.Error{Class: "KeyError", Msg: "'spec'"}
	}
	spec.Set("session_review_exceptions", kept)
	return nil
}

// BackupFile copies path beside itself with a private mode, and returns the copy or nil when path does not
// exist.
//
// py: agent_decisions/install.py backup_file
func BackupFile(path string) (*string, error) {
	source, err := CanonicalPath(path)
	if err != nil {
		return nil, err
	}
	if !ospath.Exists(source) {
		return nil, nil
	}
	suffix := now().UTC().Format("20060102T150405Z") + "." + strconv.FormatInt(now().UnixNano(), 10)
	backup := pathlib.Join(pathlib.Parent(source), pathlib.Name(source)+".agent-decisions.bak."+suffix)
	if err := copy2(source, backup); err != nil {
		return nil, err
	}
	if err := syscall.Chmod(backup, 0o600); err != nil {
		return nil, pyerr.NewOSError(err, backup)
	}
	return &backup, nil
}

// Writer applies a Codex config RPC. Tests pass fakes, as the Python tests pass a mock writer.
type Writer interface {
	Call(method string, params *json.Object) (*json.Object, error)
}

// CodexRpcWriter uses the control socket, with local stdio recovery when automatic socket initialization
// fails. Explicit sockets and failures after initialization are never retried.
//
// py: agent_decisions/install.py CodexRpcWriter
type CodexRpcWriter struct {
	CodexBinary     string
	SocketPath      *string // nil: no control socket
	TimeoutS        float64
	automaticSocket bool
}

// NewCodexRpcWriter ports CodexRpcWriter.__init__. A nil socketPath looks for the default control socket.
// Python's defaults are CodexBinary and a 10 second timeout.
//
// py: agent_decisions/install.py CodexRpcWriter.__init__
func NewCodexRpcWriter(codexBinary string, socketPath *string, timeoutS float64) (*CodexRpcWriter, error) {
	writer := &CodexRpcWriter{CodexBinary: codexBinary, TimeoutS: timeoutS, automaticSocket: socketPath == nil}
	if socketPath != nil {
		path, err := CanonicalPath(*socketPath)
		if err != nil {
			return nil, err
		}
		writer.SocketPath = &path
		return writer, nil
	}
	path, err := defaultAppServerSocket()
	if err != nil {
		return nil, err
	}
	writer.SocketPath = path
	return writer, nil
}

// py: agent_decisions/install.py CodexRpcWriter.call
func (w *CodexRpcWriter) Call(method string, params *json.Object) (*json.Object, error) {
	request := json.NewObject("id", newID(), "method", method, "params", params.Copy())
	command := []string{w.CodexBinary, "app-server", "--stdio"}
	if w.SocketPath != nil {
		command = []string{w.CodexBinary, "app-server", "proxy", "--sock", *w.SocketPath}
	}
	response, err := callJSONRPC(command, request, w.TimeoutS, true)
	var initialization *rpcInitializationError
	if w.automaticSocket && w.SocketPath != nil && errors.As(err, &initialization) {
		return callJSONRPC([]string{w.CodexBinary, "app-server", "--stdio"}, request, w.TimeoutS, true)
	}
	return response, err
}

// Only an initialization failure proves the requested operation was not sent.
type rpcInitializationError struct{ cause error }

func (e *rpcInitializationError) Error() string { return e.cause.Error() }
func (e *rpcInitializationError) Unwrap() error { return e.cause }

// py: agent_decisions/install.py _default_app_server_socket
func appServerSocket() (*string, error) {
	home, err := pathlib.Home()
	if err != nil {
		return nil, err
	}
	path := pathlib.Join(home, ".codex/app-server-control/app-server-control.sock")
	if !ospath.Exists(path) {
		return nil, nil
	}
	return &path, nil
}

// callJSONRPC starts command, sends request (after an initialize request when initialize is set) and
// returns the response with the same id. Request writes and response reads share one deadline per
// exchange. Child cleanup remains bounded separately.
//
// py: agent_decisions/install.py _call_json_rpc
func callJSONRPC(command []string, request *json.Object, timeoutS float64, initialize bool) (response *json.Object, err error) {
	process, err := startProcess(command)
	if err != nil {
		return nil, err
	}
	defer func() {
		// A failed termination replaces the original error, as an exception raised in a finally block does.
		if terminateErr := terminateProcess(process, command); terminateErr != nil {
			response, err = nil, terminateErr
		}
	}()
	timeout := time.Duration(timeoutS * float64(time.Second))

	sendAndWait := func(payload *json.Object) (*json.Object, error) {
		method := payload.Value("method").(string)
		deadline := time.Now().Add(timeout)
		if err := process.stdin.SetWriteDeadline(deadline); err != nil {
			return nil, pyerr.NewOSError(err)
		}
		line, err := json.Dumps(payload, json.Options{})
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(process.stdin, line+"\n"); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, installError(fmt.Sprintf("codex app-server timed out for %s: %s", method, drainFirst(process.stderr)))
			}
			return nil, pyerr.NewOSError(err)
		}
		var response *json.Object
		for time.Now().Before(deadline) {
			line, ok := process.stdout.get(50 * time.Millisecond)
			if !ok {
				if process.poll() {
					break
				}
				continue
			}
			if line == nil {
				break
			}
			message, err := json.Loads(*line)
			if err != nil {
				if pyerr.IsInstance(err, "ValueError") {
					continue
				}
				return nil, err
			}
			// The payload id is a str, so == never compares two lists.
			if object, ok := message.(*json.Object); ok && object.Value("id") == payload.Value("id") {
				response = object
				break
			}
		}
		if response == nil {
			return nil, installError(fmt.Sprintf("codex app-server timed out for %s: %s", method, drainFirst(process.stderr)))
		}
		if json.Truthy(response.Value("error")) {
			return nil, installError(fmt.Sprintf("codex app-server returned an error for %s", method))
		}
		return response, nil
	}

	if initialize {
		client := json.NewObject("name", "agent-decisions-install", "version", "1.1.0")
		_, err := sendAndWait(json.NewObject("id", newID(), "method", "initialize",
			"params", json.NewObject("clientInfo", client, "capabilities", json.NewObject())))
		if err != nil {
			return nil, &rpcInitializationError{cause: err}
		}
	}
	return sendAndWait(request)
}

// process is a child with text pipes, as subprocess.Popen(text=True, stdin=PIPE, stdout=PIPE, stderr=PIPE)
// makes it. Its pipes are *os.File, so exec copies nothing through goroutines and Wait closes none of them.
type process struct {
	cmd            *exec.Cmd
	stdin          *os.File
	stdout, stderr *lineQueue
	readers        []*os.File
	done           chan struct{} // closed when Wait returns
}

// startProcess starts command with three pipes and a reader goroutine per output pipe.
func startProcess(command []string) (*process, error) {
	cmd := exec.Command(command[0], command[1:]...)
	var parentEnds, childEnds []*os.File
	closeAll := func() {
		for _, file := range slices.Concat(parentEnds, childEnds) {
			file.Close()
		}
	}
	for range 3 {
		reader, writer, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, pyerr.NewOSError(err)
		}
		if len(parentEnds) == 0 { // stdin: the child reads
			parentEnds, childEnds = append(parentEnds, writer), append(childEnds, reader)
		} else {
			parentEnds, childEnds = append(parentEnds, reader), append(childEnds, writer)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childEnds[0], childEnds[1], childEnds[2]
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, spawnError(err, command[0])
	}
	for _, file := range childEnds {
		file.Close()
	}
	p := &process{cmd: cmd, stdin: parentEnds[0], stdout: newLineQueue(), stderr: newLineQueue(),
		readers: parentEnds[1:], done: make(chan struct{})}
	startReader(parentEnds[1], p.stdout)
	startReader(parentEnds[2], p.stderr)
	go func() {
		cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// poll reports whether the process has exited, like Popen.poll() is not None.
func (p *process) poll() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// wait ports Popen.wait(timeout): it reports whether the process exited in time.
func (p *process) wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// lineQueue ports queue.Queue[Optional[str]]: an unbounded FIFO whose producer never blocks. A nil item
// marks the end of the stream.
type lineQueue struct {
	mu     sync.Mutex
	items  []*string
	signal chan struct{}
}

func newLineQueue() *lineQueue {
	return &lineQueue{signal: make(chan struct{}, 1)}
}

func (q *lineQueue) put(item *string) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// get returns the next item, waiting up to timeout; ok is false when none arrived (queue.Empty). A zero
// timeout is get_nowait.
func (q *lineQueue) get(timeout time.Duration) (item *string, ok bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item, q.items = q.items[0], q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		q.mu.Unlock()
		if timeout <= 0 {
			return nil, false
		}
		select {
		case <-q.signal:
		case <-timer.C:
			return nil, false
		}
	}
}

// startReader queues each line of stream, then nil. Lines split and end as a text-mode Python file reads
// them: "\n", "\r" and "\r\n" each end a line and read as "\n". Python's reader thread dies on invalid
// UTF-8, which ends the stream here too.
//
// py: agent_decisions/install.py _start_reader
func startReader(stream io.Reader, queue *lineQueue) {
	go func() {
		defer queue.put(nil)
		reader := bufio.NewReader(stream)
		for {
			line, err := reader.ReadBytes('\n')
			for len(line) > 0 {
				end := bytes.IndexByte(line, '\r')
				if end < 0 {
					break
				}
				if end == len(line)-1 && err == nil {
					// A "\r" at the end of a read may start a "\r\n": wait for the next byte, as Python does.
					if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == '\n' {
						reader.Discard(1)
					}
				}
				if end+1 < len(line) && line[end+1] == '\n' {
					line = slices.Delete(line, end+1, end+2)
				}
				if !emit(queue, append(line[:end:end], '\n')) {
					return
				}
				line = line[end+1:]
			}
			if len(line) > 0 && !emit(queue, line) {
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

// emit queues one line and reports whether it was valid UTF-8.
func emit(queue *lineQueue, line []byte) bool {
	text, err := json.DecodeUTF8Strict(line)
	if err != nil {
		return false
	}
	queue.put(&text)
	return true
}

// terminateProcess stops a running process: SIGTERM, then SIGKILL after 2 seconds, then gives up with
// TimeoutExpired after 2 more. It always closes the pipes.
//
// py: agent_decisions/install.py _terminate_process
func terminateProcess(p *process, command []string) error {
	var err error
	if !p.poll() {
		p.cmd.Process.Signal(syscall.SIGTERM)
		if !p.wait(2 * time.Second) {
			p.cmd.Process.Kill()
			if !p.wait(2 * time.Second) {
				err = &pyerr.Error{Class: "TimeoutExpired", Module: "subprocess",
					Msg: fmt.Sprintf("Command '%s' timed out after 2 seconds", reprList(command))}
			}
		}
	}
	p.stdin.Close()
	for _, file := range p.readers {
		file.Close()
	}
	return err
}

// drainFirst returns the first non-empty queued stderr line, stripped and cut to 500 code points.
//
// py: agent_decisions/install.py _drain_first
func drainFirst(items *lineQueue) string {
	for {
		item, ok := items.get(0)
		if !ok {
			return ""
		}
		if item != nil && *item != "" {
			return str.SliceTo(str.Strip(*item), 500)
		}
	}
}

// ApplyCodexPlan backs up the Codex config, publishes the runtime bundle, signs the installation and
// applies the config RPC. A nil writer uses the preferred launcher or Codex on PATH.
//
// py: agent_decisions/install.py apply_codex_plan
func ApplyCodexPlan(plan *json.Object, writer Writer) (*json.Object, error) {
	rpcValue, err := plan.Item("config_rpc")
	if err != nil {
		return nil, err
	}
	rpc, ok := rpcValue.(*json.Object)
	if !ok {
		return nil, installError("codex plan needs config_rpc")
	}
	var backup *string
	params, isObject := rpc.ValueOr("params", json.NewObject()).(*json.Object)
	if isObject && json.Truthy(params.Value("filePath")) {
		path, ok := params.Value("filePath").(string)
		if !ok {
			return nil, installError("path must be a non-empty string")
		}
		backup, err = BackupFile(path)
	} else {
		var home string
		if home, err = pathlib.Home(); err == nil {
			backup, err = BackupFile(pathlib.Join(home, ".codex/config.toml"))
		}
	}
	if err != nil {
		return nil, err
	}
	if writer == nil {
		if writer, err = NewCodexRpcWriter(defaultCodexBinary(), nil, 10); err != nil {
			return nil, err
		}
	}
	if err := publish(plan); err != nil {
		return nil, err
	}
	entry, err := plan.Item("installation_entry")
	if err != nil {
		return nil, err
	}
	if err := signEntry(entry); err != nil {
		return nil, err
	}
	methodValue, err := rpc.Item("method")
	if err != nil {
		return nil, err
	}
	paramsValue, err := rpc.Item("params")
	if err != nil {
		return nil, err
	}
	// ponytail: a plan from BuildCodexPlan always holds a str method and dict params.
	method, isText := methodValue.(string)
	callParams, isObject := paramsValue.(*json.Object)
	if !isText || !isObject {
		return nil, &pyerr.Error{Class: "TypeError", Msg: "config_rpc needs a str method and dict params"}
	}
	response, err := writer.Call(method, callParams)
	if err != nil {
		return nil, err
	}
	return json.NewObject("backup", optional(backup), "response", response), nil
}

// py: agent_decisions/install.py apply_claude_plan
func ApplyClaudePlan(plan *json.Object, settingsPath string) (*json.Object, error) {
	backup, err := BackupFile(settingsPath)
	if err != nil {
		return nil, err
	}
	if err := publish(plan); err != nil {
		return nil, err
	}
	entry, err := plan.Item("installation_entry")
	if err != nil {
		return nil, err
	}
	if err := signEntry(entry); err != nil {
		return nil, err
	}
	merged, err := plan.Item("merged_settings")
	if err != nil {
		return nil, err
	}
	settings, ok := merged.(*json.Object)
	if !ok {
		return nil, &pyerr.Error{Class: "TypeError", Msg: "merged_settings must be an object"}
	}
	if err := WriteJSON(settingsPath, settings); err != nil {
		return nil, err
	}
	return json.NewObject("backup", optional(backup)), nil
}

// publish ports runtime.publish(Path(plan["install_root"]), home=...) in the apply functions.
func publish(plan *json.Object) error {
	rootValue, err := plan.Item("install_root")
	if err != nil {
		return err
	}
	root, ok := rootValue.(string)
	if !ok {
		return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf(
			"argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", json.TypeName(rootValue))}
	}
	home, _ := plan.Value("runtime_home").(string)
	_, err = bundle.Publish(root, home)
	return err
}

func signEntry(entry any) error {
	object, ok := entry.(*json.Object)
	if !ok {
		return &pyerr.Error{Class: "TypeError", Msg: "installation_entry must be an object"}
	}
	return SignInstallation(object)
}

// ReadJSON reads a JSON object; a missing file reads as {}.
//
// py: agent_decisions/install.py read_json
func ReadJSON(path string) (*json.Object, error) {
	target := pathlib.Str(path)
	text, err := pathlib.ReadText(target)
	if errors.Is(err, syscall.ENOENT) {
		return json.NewObject(), nil
	}
	if err != nil {
		return nil, err
	}
	data, err := json.Loads(text)
	if err != nil {
		return nil, err
	}
	object, ok := data.(*json.Object)
	if !ok {
		return nil, installError(target + " must contain a JSON object")
	}
	return object, nil
}

// WriteJSON atomically updates the resolved target with private JSON and preserves settings symlinks.
//
// py: agent_decisions/install.py write_json
func WriteJSON(path string, value *json.Object) (err error) {
	target, err := CanonicalPath(path)
	if err != nil {
		return err
	}
	if err := pathlib.Mkdir(pathlib.Parent(target), 0o700, true); err != nil {
		return err
	}
	temporary := pathlib.Join(pathlib.Parent(target),
		fmt.Sprintf(".%s.%d.%d.tmp", pathlib.Name(target), os.Getpid(), now().UnixNano()))
	text, err := json.Dumps(value, json.Options{Indent: 2, SortKeys: true})
	if err != nil {
		return err
	}
	data := []byte(text + "\n") // ensure_ascii output: already UTF-8
	defer func() {
		if err != nil {
			if unlinkErr := syscall.Unlink(temporary); unlinkErr != nil && unlinkErr != syscall.ENOENT {
				err = pyerr.NewOSError(unlinkErr, temporary)
			}
		}
	}()
	fd, err := pyos.Open(temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = pyos.WriteAll(fd, data)
	if err == nil {
		if syncErr := syscall.Fsync(fd); syncErr != nil {
			err = pyerr.NewOSError(syncErr)
		}
	}
	if closeErr := syscall.Close(fd); closeErr != nil {
		err = pyerr.NewOSError(closeErr) // an error in Python's finally block replaces the first
	}
	if err != nil {
		return err
	}
	if err := syscall.Rename(temporary, target); err != nil {
		return pyerr.NewOSError(err, temporary, target)
	}
	if err := syscall.Chmod(target, 0o600); err != nil {
		return pyerr.NewOSError(err, target)
	}
	return nil
}

// Sanitize redacts env maps, secret keys, secret CLI arguments and URL credentials before a plan prints.
//
// py: agent_decisions/install.py sanitize
func Sanitize(value any) (any, error) {
	switch value := value.(type) {
	case *json.Object:
		sanitized := json.NewObject()
		for _, key := range value.Keys() {
			keyText := str.Lower(key)
			if keyText == "env" || isSecretKey(keyText) {
				sanitized.Set(key, redacted)
				continue
			}
			item, err := Sanitize(value.Value(key))
			if err != nil {
				return nil, err
			}
			sanitized.Set(key, item)
		}
		return sanitized, nil
	case []any:
		return sanitizeList(value)
	case string:
		return sanitizeString(value)
	}
	return value, nil
}

// py: agent_decisions/install.py _sanitize_list
func sanitizeList(values []any) ([]any, error) {
	sanitized := []any{}
	redactNext := false
	for _, value := range values {
		if redactNext {
			sanitized = append(sanitized, redacted)
			redactNext = false
			continue
		}
		text, ok := value.(string)
		if ok {
			if flag, _, inline := strings.Cut(text, "="); inline && isSecretCLIFlag(flag) {
				sanitized = append(sanitized, flag+"="+redacted)
				continue
			}
		}
		item, err := Sanitize(value)
		if err != nil {
			return nil, err
		}
		sanitized = append(sanitized, item)
		redactNext = ok && isSecretCLIFlag(text)
	}
	return sanitized, nil
}

// py: agent_decisions/install.py _sanitize_string
func sanitizeString(value string) (string, error) {
	text, urls, err := extractRedactedURLs(value)
	if err != nil {
		return "", err
	}
	text = evidence.Redact(text, nil)
	text = cliSecretEquals.ReplaceAllString(text, "${1}"+redacted)
	text = cliSecretNext.ReplaceAllString(text, "${1}"+redacted)
	replacements := make([]string, 0, 2*len(urls))
	for _, url := range urls {
		replacements = append(replacements, url[0], url[1])
	}
	return strings.NewReplacer(replacements...).Replace(text), nil
}

// extractRedactedURLs replaces each URL with a placeholder, and returns the placeholders with the redacted
// URLs. A match counts only where Python's Unicode \b holds: no word character before "http". Python then
// tries the next position, so a failed boundary resumes the search one code point later.
//
// py: agent_decisions/install.py _extract_redacted_urls
func extractRedactedURLs(value string) (string, [][2]string, error) {
	var urls [][2]string
	var out strings.Builder
	nextID := 0
	done := 0 // value[:done] is in out
	for start := 0; start < len(value); {
		match := urlToken.FindStringIndex(value[start:])
		if match == nil {
			break
		}
		begin, end := start+match[0], start+match[1]
		if previous := lastCodePoint(value[:begin]); str.IsAlnum(previous) || previous == "_" {
			_, width := str.DecodeRune(value[begin:])
			start = begin + width
			continue
		}
		url, err := redactURL(value[begin:end])
		if err != nil {
			return "", nil, err
		}
		var placeholder string
		for {
			placeholder = fmt.Sprintf("__AD_URL_%d__", nextID)
			nextID++
			if !strings.Contains(value, placeholder) {
				break
			}
		}
		urls = append(urls, [2]string{placeholder, url})
		out.WriteString(value[done:begin])
		out.WriteString(placeholder)
		done, start = end, end
	}
	out.WriteString(value[done:])
	return out.String(), urls, nil
}

// py: agent_decisions/install.py _is_secret_cli_flag
func isSecretCLIFlag(value string) bool {
	if !strings.HasPrefix(value, "--") || strings.Contains(value, "=") {
		return false
	}
	return secretCLIFlags[str.Lower(value[2:])]
}

// py: agent_decisions/install.py _redact_url
func redactURL(value string) (string, error) {
	parts, err := urlsplit(value)
	if err != nil {
		return "", err
	}
	if at := strings.LastIndex(parts.netloc, "@"); at >= 0 {
		parts.netloc = redacted + "@" + parts.netloc[at+1:]
	}
	if parts.query, err = redactURLQuery(parts.query); err != nil {
		return "", err
	}
	return urlunsplit(parts), nil
}

// py: agent_decisions/install.py _redact_url_query
func redactURLQuery(query string) (string, error) {
	if query == "" {
		return query, nil
	}
	pairs := parseQSL(query)
	for i, pair := range pairs {
		if isSecretKey(str.Lower(pair[0])) {
			pairs[i][1] = redacted
		}
	}
	return urlencode(pairs)
}

// py: agent_decisions/install.py _is_secret_key
func isSecretKey(keyText string) bool {
	normalized := strings.ReplaceAll(keyText, "-", "_")
	switch normalized {
	case "key", "key_file", "keyfile", "ledger_key":
		return true
	case "keypath", "key_path":
		return false
	}
	for _, part := range secretKeyParts {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	for _, suffix := range []string{"_key", "_token", "_secret", "_password"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// cliArgs is the argparse namespace of the installer.
type cliArgs struct {
	host, reviewMode, stateRoot, installRoot, codexBinary, codexConfig, claudeSettings    string
	apply                                                                                 bool
	sessionReviewException, clearSessionReviewException, claudeJSON, claudePluginManifest []string
	codexSock, codexMCPJSON                                                               *string
}

// newParser builds the parser of cli. Python evaluates the defaults as it adds the arguments, so their
// errors escape even --help. The launcher runs python3 without -I, so PYTHON_COLORS applies.
//
// py: agent_decisions/install.py cli
func newParser(stdout, stderr io.Writer) (*argparse.Parser, error) {
	stateDir, err := policy.StateDir()
	if err != nil {
		return nil, err
	}
	installRoot, err := DefaultInstallRoot()
	if err != nil {
		return nil, err
	}
	home, err := pathlib.Home()
	if err != nil {
		return nil, err
	}
	claudeRoot, err := selectedClaudeConfigRoot(home)
	if err != nil {
		return nil, err
	}
	store := func(option string, fallback any) *argparse.Action {
		return &argparse.Action{Kind: argparse.Store, Options: []string{option}, Dest: dest(option), Default: fallback}
	}
	appendTo := func(option, metavar, help string) *argparse.Action {
		return &argparse.Action{Kind: argparse.Append, Options: []string{option}, Dest: dest(option), Metavar: metavar,
			Help: help, Default: []string{}}
	}
	return &argparse.Parser{Prog: "agent-decisions-install", Stdout: stdout, Stderr: stderr, Actions: []*argparse.Action{
		argparse.HelpAction(),
		{Kind: argparse.Store, Options: []string{"--host"}, Dest: "host", Choices: []string{"codex", "claude", "both"},
			Default: "both"},
		{Kind: argparse.StoreTrue, Options: []string{"--apply"}, Dest: "apply"},
		{Kind: argparse.Store, Options: []string{"--review-mode"}, Dest: "review_mode",
			Choices: []string{"required", "availability"}, Default: "required",
			Help: "Required blocks unchecked actions; availability permits them with a warning."},
		appendTo("--session-review-exception", "SESSION_ID",
			"Owner-authorized review exception for one canonical session and its native workers."),
		appendTo("--clear-session-review-exception", "SESSION_ID", "Revoke an owner-authorized session review exception."),
		store("--state-root", stateDir),
		store("--install-root", installRoot),
		store("--codex-binary", defaultCodexBinary()),
		store("--codex-sock", nil),
		store("--codex-config", pathlib.Join(home, ".codex/config.toml")),
		store("--codex-mcp-json", nil),
		store("--claude-settings", pathlib.Join(claudeRoot, "settings.json")),
		appendTo("--claude-json", "", ""),
		appendTo("--claude-plugin-manifest", "", ""),
	}}, nil
}

// dest returns the dest argparse derives from a long option: "--state-root" is "state_root".
func dest(option string) string {
	return strings.ReplaceAll(option[2:], "-", "_") // ASCII option strings
}

// parseArgs returns the parsed arguments, or the exit status after argparse printed help or an error.
func parseArgs(parser *argparse.Parser, argv []string) (*cliArgs, int, bool) {
	ns, err := parser.ParseArgs(argv)
	if err != nil {
		return nil, err.(*argparse.Exit).Code, true
	}
	text := func(key string) string { return ns[key].(string) }
	optional := func(key string) *string {
		if value, ok := ns[key].(string); ok {
			return &value
		}
		return nil
	}
	return &cliArgs{
		host: text("host"), reviewMode: text("review_mode"), stateRoot: text("state_root"),
		installRoot: text("install_root"), codexBinary: text("codex_binary"), codexConfig: text("codex_config"),
		claudeSettings: text("claude_settings"), apply: ns["apply"].(bool),
		sessionReviewException:      ns["session_review_exception"].([]string),
		clearSessionReviewException: ns["clear_session_review_exception"].([]string),
		claudeJSON:                  ns["claude_json"].([]string),
		claudePluginManifest:        ns["claude_plugin_manifest"].([]string),
		codexSock:                   optional("codex_sock"), codexMCPJSON: optional("codex_mcp_json"),
	}, 0, false
}

// Main runs the installer CLI with args (sys.argv[1:]) and returns its exit status. An exception that
// escapes Python's cli prints a traceback; Main prints its last line, "Class: message", and returns 1.
//
// py: agent_decisions/install.py cli
func Main(args []string, stdout, stderr io.Writer) int {
	status, err := cli(args, stdout, stderr)
	if err != nil {
		pyerr.WriteUncaught(stderr, err)
		return 1
	}
	return status
}

func cli(argv []string, stdout, stderr io.Writer) (int, error) {
	parser, err := newParser(stdout, stderr)
	if err != nil {
		return 0, err
	}
	args, status, exited := parseArgs(parser, argv)
	if exited {
		return status, nil
	}

	if (len(args.sessionReviewException) > 0 || len(args.clearSessionReviewException) > 0) && args.host != "codex" {
		return parser.Error("session review exceptions require --host codex").Code, nil
	}

	if args.apply {
		state, err := CanonicalPath(args.stateRoot)
		if err != nil {
			return 0, err
		}
		policyState, err := policy.StateDir()
		if err != nil {
			return 0, err
		}
		if want, err := CanonicalPath(policyState); err != nil {
			return 0, err
		} else if state != want {
			return parser.Error("--state-root must match AGENT_DECISIONS_STATE when --apply is used").Code, nil
		}
	}

	plans := []any{}
	if args.host == "codex" || args.host == "both" {
		var servers []*json.Object
		if args.codexMCPJSON != nil && *args.codexMCPJSON != "" {
			servers, err = ReadCodexServersFile(*args.codexMCPJSON)
		} else {
			servers, err = ReadCodexServers(args.codexBinary)
		}
		if err != nil {
			return 0, err
		}
		config := pathlib.Str(args.codexConfig)
		summary, err := ReadCodexConfigSummary(&config)
		if err != nil {
			return 0, err
		}
		plan, err := BuildCodexPlan(servers, args.stateRoot, args.installRoot, summary, &args.codexConfig, "",
			PreviousInstallationSpec("codex"))
		if err != nil {
			return 0, err
		}
		plans = append(plans, plan)
	}
	if args.host == "claude" || args.host == "both" {
		manifestPaths := make([]string, len(args.claudePluginManifest))
		for i, path := range args.claudePluginManifest {
			manifestPaths[i] = pathlib.Str(path)
		}
		if len(manifestPaths) == 0 {
			if manifestPaths, err = DiscoverClaudePluginManifests(""); err != nil {
				return 0, err
			}
		}
		configPaths := args.claudeJSON
		if len(configPaths) == 0 {
			roots, err := ClaudeConfigRoots("")
			if err != nil {
				return 0, err
			}
			for _, pair := range roots {
				canonical, err := CanonicalPath(pair[1])
				if err != nil {
					return 0, err
				}
				if !slices.Contains(configPaths, canonical) {
					configPaths = append(configPaths, canonical)
				}
			}
		}
		settings, err := ReadJSON(args.claudeSettings)
		if err != nil {
			return 0, err
		}
		configs := []Sourced{}
		for _, path := range configPaths {
			config, err := ReadJSON(path)
			if err != nil {
				return 0, err
			}
			configs = append(configs, Sourced{path, config})
		}
		manifests, err := ReadClaudePluginManifests(manifestPaths)
		if err != nil {
			return 0, err
		}
		plan, err := BuildClaudePlan(settings, args.stateRoot, args.installRoot, "", configs, manifests,
			PreviousInstallationSpec("claude"))
		if err != nil {
			return 0, err
		}
		plans = append(plans, plan)
	}

	for _, plan := range plans {
		entry := plan.(*json.Object).Value("installation_entry").(*json.Object)
		spec := entry.Value("spec").(*json.Object)
		spec.Set("review_mode", args.reviewMode)
		if err := ConfigureSessionExceptions(entry, spec, args.sessionReviewException, args.clearSessionReviewException); err != nil {
			return 0, err
		}
	}
	result := json.NewObject("apply", args.apply, "plans", plans)
	if args.apply {
		applied := []any{}
		for _, value := range plans {
			plan := value.(*json.Object)
			var outcome *json.Object
			switch plan.Value("host") {
			case "codex":
				writer, err := NewCodexRpcWriter(args.codexBinary, args.codexSock, 10)
				if err != nil {
					return 0, err
				}
				if outcome, err = ApplyCodexPlan(plan, writer); err != nil {
					return 0, err
				}
			case "claude":
				if outcome, err = ApplyClaudePlan(plan, args.claudeSettings); err != nil {
					return 0, err
				}
			default:
				continue
			}
			row := json.NewObject("host", plan.Value("host"))
			for _, key := range outcome.Keys() {
				row.Set(key, outcome.Value(key))
			}
			applied = append(applied, row)
		}
		result.Set("applied", applied)
	}
	sanitized, err := Sanitize(result)
	if err != nil {
		return 0, err
	}
	text, err := json.Dumps(sanitized, json.Options{Indent: 2, SortKeys: true})
	if err != nil {
		return 0, err
	}
	io.WriteString(stdout, text+"\n")
	return 0, nil
}

// py: agent_decisions/install.py _server_list
func serverList(parsed any) ([]*json.Object, error) {
	var values []any
	switch parsed := parsed.(type) {
	case *json.Object:
		if servers, ok := parsed.Value("servers").([]any); ok {
			values = servers
			break
		}
		values = []any{}
		for _, name := range parsed.Keys() {
			if value, ok := parsed.Value(name).(*json.Object); ok {
				item := deepCopy(value).(*json.Object)
				if _, present := item.Get("name"); !present {
					item.Set("name", name)
				}
				values = append(values, item)
			}
		}
	case []any:
		values = parsed
	default:
		return nil, installError("MCP JSON must be an object or list")
	}
	servers := make([]*json.Object, len(values))
	for i, value := range values {
		server, ok := value.(*json.Object)
		if !ok {
			return nil, installError("MCP JSON servers must be objects")
		}
		servers[i] = server
	}
	for i, server := range servers {
		servers[i] = deepCopy(server).(*json.Object)
	}
	return servers, nil
}

// py: agent_decisions/install.py _server_name
func serverName(server *json.Object) (string, error) {
	name, ok := server.Value("name").(string)
	if !ok || name == "" {
		return "", installError("server needs a name")
	}
	return name, nil
}

// py: agent_decisions/install.py _server_source
func serverSource(server *json.Object) (string, error) {
	source := server.Value("source")
	for _, key := range []string{"source_path", "config_source"} {
		if !json.Truthy(source) {
			source = server.Value(key)
		}
	}
	switch source := source.(type) {
	case string:
		if source != "" {
			return source, nil
		}
	case *json.Object:
		for _, key := range []string{"manifest_path", "path", "file_path", "config_path", "source_path"} {
			if value, ok := source.Value(key).(string); ok && value != "" {
				return value, nil
			}
		}
		plugin := source.Value("plugin")
		for _, key := range []string{"plugin_id", "id"} {
			if !json.Truthy(plugin) {
				plugin = source.Value(key)
			}
		}
		if plugin, ok := plugin.(string); ok && plugin != "" {
			return "codex:plugin:" + plugin, nil
		}
		text, err := json.Dumps(dropNone(source), json.Options{SortKeys: true, Compact: true})
		if err != nil {
			return "", err
		}
		return "codex:source:" + text, nil
	}
	return "codex:mcp-list", nil
}

// py: agent_decisions/install.py _transport
func transport(server *json.Object) *json.Object {
	if raw, ok := server.Value("transport").(*json.Object); ok {
		return deepCopy(raw).(*json.Object)
	}
	transport := json.NewObject("type", server.ValueOr("type", "stdio"))
	for _, key := range []string{"command", "args", "cwd", "env", "env_vars", "url", "headers", "http_headers",
		"env_http_headers", "http_headers_helper", "bearer_token_env_var"} {
		if value, present := server.Get(key); present {
			transport.Set(key, deepCopy(value))
		}
	}
	return transport
}

// py: agent_decisions/install.py _is_codex_wrapper_transport
func isCodexWrapperTransport(transport *json.Object, name string) bool {
	command, ok := transport.Value("command").(string)
	args, isList := transport.ValueOr("args", []any{}).([]any)
	if !ok || !isList {
		return false
	}
	if pathlib.Name(pathlib.Str(command)) != "agent-decisions-isolate" {
		return false
	}
	// The wanted items are strings, so == never compares two lists.
	return len(args) >= 4 && slices.Equal(args[:4], []any{"--server", name, "--host", "codex"})
}

// py: agent_decisions/install.py _previous_server_recipe
func previousServerRecipe(previousSpec *json.Object, name string) *json.Object {
	if previousSpec == nil {
		return nil
	}
	servers, ok := previousSpec.Value("servers").(*json.Object)
	if !ok {
		return nil
	}
	recipe, ok := servers.Value(name).(*json.Object)
	if !ok {
		return nil
	}
	return deepCopy(recipe).(*json.Object)
}

// py: agent_decisions/install.py _config_key_path
func configKeyPath(prefix, value string) (string, error) {
	segment, err := safeKeyPathSegment(value)
	if err != nil {
		return "", err
	}
	return prefix + "." + segment, nil
}

// py: agent_decisions/install.py _safe_key_path_segment
func safeKeyPathSegment(value string) (string, error) {
	if value == "" {
		return "", installError("unsupported empty config key path segment: " + str.Repr(value))
	}
	for rest := value; rest != ""; {
		_, width := str.DecodeRune(rest)
		if char := rest[:width]; !str.IsAlnum(char) && char != "_" && char != "-" {
			return json.Dumps(value, json.Options{})
		}
		rest = rest[width:]
	}
	return value, nil
}

// parseAssignment splits "key = value"; ok is false for a line without "=".
//
// py: agent_decisions/install.py _parse_assignment
func parseAssignment(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(line, "=")
	return str.Strip(key), str.Strip(value), ok
}

// stripTomlComment cuts a "#" comment outside quotes. Every byte it compares is ASCII, and no UTF-8 or
// WTF-8 multi-byte sequence holds an ASCII byte, so walking bytes equals walking code points; a byte after
// a backslash is skipped like the code point it starts.
//
// py: agent_decisions/install.py _strip_toml_comment
func stripTomlComment(line string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		char := line[i]
		switch {
		case escaped:
			escaped = false
		case quote != 0 && char == '\\':
			escaped = true
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '\'' || char == '"':
			quote = char
		case char == '#':
			return line[:i]
		}
	}
	return line
}

// parseSection returns a TOML table header as dotted, unquoted parts. It walks bytes for the reason
// stripTomlComment gives.
//
// py: agent_decisions/install.py _parse_section
func parseSection(raw string) string {
	var parts []string
	var current []byte
	var quote byte
	escaped := false
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		switch {
		case escaped:
			current = append(current, char)
			escaped = false
		case quote != 0 && char == '\\':
			current = append(current, char)
			escaped = true
		case quote != 0:
			current = append(current, char)
			if char == quote {
				quote = 0
			}
		case char == '\'' || char == '"':
			current = append(current, char)
			quote = char
		case char == '.':
			parts = append(parts, parseSectionPart(str.Strip(string(current))))
			current = nil
		default:
			current = append(current, char)
		}
	}
	parts = append(parts, parseSectionPart(str.Strip(string(current))))
	return strings.Join(slices.DeleteFunc(parts, func(part string) bool { return part == "" }), ".")
}

// py: agent_decisions/install.py _parse_section_part
func parseSectionPart(part string) string {
	if quoted(part, '"') {
		if value, err := json.Loads(part); err == nil {
			return value.(string) // a JSON document that starts with a quote is a string
		}
		return part[1 : len(part)-1]
	}
	if quoted(part, '\'') {
		return part[1 : len(part)-1]
	}
	return part
}

// py: agent_decisions/install.py _parse_scalar
func parseScalar(value string) any {
	if value == "true" || value == "false" {
		return value == "true"
	}
	if quoted(value, '"') {
		if parsed, err := json.Loads(value); err == nil {
			return parsed
		}
		return value[1 : len(value)-1]
	}
	return value
}

// quoted reports len(s) >= 2 and s[0] == s[-1] == quote. The quote is ASCII, so bytes decide it.
func quoted(s string, quote byte) bool {
	return len(s) >= 2 && s[0] == quote && s[len(s)-1] == quote
}

// py: agent_decisions/install.py _legacy_sandbox_profile
func legacySandboxProfile(value any) string {
	switch value {
	case "read-only":
		return ":read-only"
	case "danger-full-access":
		return ":danger-full-access"
	}
	return ":workspace"
}

// py: agent_decisions/install.py _drop_none
func dropNone(value any) any {
	switch value := value.(type) {
	case *json.Object:
		out := json.NewObject()
		for _, key := range value.Keys() {
			if item := value.Value(key); item != nil {
				out.Set(key, dropNone(item))
			}
		}
		return out
	case []any:
		out := []any{}
		for _, item := range value {
			if item != nil {
				out = append(out, dropNone(item))
			}
		}
		return out
	}
	return value
}

// py: agent_decisions/install.py _append_unique
func appendUnique(container *json.Object, key, value string) {
	current, ok := container.Value(key).([]any)
	if !ok {
		current = []any{}
	}
	if !slices.Contains(current, any(value)) {
		current = append(current, value)
	}
	container.Set(key, current)
}

// py: agent_decisions/install.py _append_tool_denies
func appendToolDenies(deny []any, path string, read, write bool) ([]any, error) {
	var tools []string
	if read {
		tools = append(tools, "Read")
	}
	if write {
		tools = append(tools, writeTools...)
	}
	toolPath, err := claudeToolPath(path)
	if err != nil {
		return nil, err
	}
	for _, tool := range tools {
		for _, pattern := range []string{toolPath, toolPath + "/**"} {
			if rule := tool + "(" + pattern + ")"; !slices.Contains(deny, any(rule)) {
				deny = append(deny, rule)
			}
		}
	}
	return deny, nil
}

// appendTranscriptDenies denies file-tool writes to transcripts, leaving the memory notes beside them
// writable.
//
// py: agent_decisions/install.py _append_transcript_denies
func appendTranscriptDenies(deny []any, path string) ([]any, error) {
	toolPath, err := claudeToolPath(path)
	if err != nil {
		return nil, err
	}
	// Installs before 1.2.3 denied the whole root, and Claude Code cannot carve a //-anchored deny back open.
	var stale []any
	for _, tool := range writeTools {
		stale = append(stale, tool+"("+toolPath+")", tool+"("+toolPath+"/**)")
	}
	deny = slices.DeleteFunc(slices.Clone(deny), func(rule any) bool { return slices.Contains(stale, rule) })
	for _, tool := range writeTools {
		if rule := tool + "(" + toolPath + "/**/*.jsonl)"; !slices.Contains(deny, any(rule)) {
			deny = append(deny, rule)
		}
	}
	return deny, nil
}

// py: agent_decisions/install.py _claude_tool_path
func claudeToolPath(path string) (string, error) {
	canonical, err := CanonicalPath(path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(canonical, "/") {
		return "/" + canonical, nil
	}
	return canonical, nil
}

// dedupe returns each path in its lexical absolute form and its canonical form, without repeats.
//
// py: agent_decisions/install.py _dedupe
func dedupe(paths []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, path := range paths {
		expanded, err := pathlib.ExpandUser(path)
		if err != nil {
			return nil, err
		}
		lexical, err := pathlib.Absolute(expanded)
		if err != nil {
			return nil, err
		}
		canonical, err := CanonicalPath(path)
		if err != nil {
			return nil, err
		}
		for _, protected := range []string{lexical, canonical} {
			if !seen[protected] {
				seen[protected] = true
				out = append(out, protected)
			}
		}
	}
	return out, nil
}
