package install

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/bundle"
	"github.com/rexbrahh/make-better-decisions/internal/isolation"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// fakeCodexArgs names the file where the fake Codex records its arguments. When it is set, the test
// binary is the fake Codex: it answers two JSON-RPC requests on stdin with {"method": <method>}.
const fakeCodexArgs = "INSTALL_TEST_FAKE_CODEX_ARGS"

func TestMain(m *testing.M) {
	if path := os.Getenv(fakeCodexArgs); path != "" {
		fakeCodex(path)
		return
	}
	os.Exit(m.Run())
}

// fakeCodex ports the fake codex script of test_codex_writer_uses_stdio_fallback_when_no_socket_exists.
func fakeCodex(argsPath string) {
	args, _ := json.Dumps(toAnyList(os.Args[1:]), json.Options{})
	os.WriteFile(argsPath, []byte(args), 0o600)
	methods := []any{}
	stdin := bufio.NewReader(os.Stdin)
	for range 2 {
		line, err := stdin.ReadString('\n')
		if err != nil {
			os.Exit(3)
		}
		parsed, err := json.Loads(line)
		if err != nil {
			os.Exit(4)
		}
		request := parsed.(*json.Object)
		methods = append(methods, request.Value("method"))
		text, _ := json.Dumps(methods, json.Options{})
		os.WriteFile(argsPath+".methods", []byte(text), 0o600)
		reply, _ := json.Dumps(json.NewObject("id", request.Value("id"),
			"result", json.NewObject("method", request.Value("method"))), json.Options{})
		os.Stdout.WriteString(reply + "\n")
	}
}

// setUp ports StateTestCase.setUp, and also points HOME at a temporary directory so that no test reads
// or writes the real home. It returns the resolved test directory.
func setUp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(dir, "user-home"))
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	t.Setenv("TYPESAFE_API_KEY", "test-key-0123456789abcdef")
	t.Setenv("AGENT_DECISIONS_SOURCE", repoRoot)
	for _, key := range []string{"AGENT_DECISIONS_OFF", "CLAUDE_CONFIG_DIR", "NO_COLOR", "FORCE_COLOR", "PYTHON_COLORS"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	return dir
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, text string) any {
	t.Helper()
	value, err := json.Loads(text)
	check(t, err)
	return value
}

func loadObject(t *testing.T, text string) *json.Object {
	t.Helper()
	return load(t, text).(*json.Object)
}

func loadObjects(t *testing.T, text string) []*json.Object {
	t.Helper()
	return objects(load(t, text))
}

// dig follows object keys and list indexes.
func dig(v any, path ...any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := v.(*json.Object)
			if !ok {
				return nil
			}
			v = object.Value(key)
		case int:
			list, ok := v.([]any)
			if !ok || key >= len(list) {
				return nil
			}
			v = list[key]
		}
	}
	return v
}

func wantEqual(t *testing.T, got, want any) {
	t.Helper()
	if same, err := json.Equal(got, want); err != nil || !same {
		gotText, _ := json.Dumps(got, json.Options{SortKeys: true})
		wantText, _ := json.Dumps(want, json.Options{SortKeys: true})
		t.Fatalf("got %s, want %s", gotText, wantText)
	}
}

func contains(list any, value any) bool {
	items, _ := list.([]any)
	return slices.Contains(items, value)
}

func edit(edits any, keyPath string) *json.Object {
	for _, item := range edits.([]any) {
		if dig(item, "keyPath") == keyPath {
			return item.(*json.Object)
		}
	}
	return nil
}

func has(object *json.Object, key string) bool {
	_, ok := object.Get(key)
	return ok
}

func keys(v any) []string {
	names := slices.Clone(v.(*json.Object).Keys())
	slices.Sort(names)
	return names
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	value, err := CanonicalPath(path)
	check(t, err)
	return value
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	check(t, os.MkdirAll(filepath.Dir(path), 0o755))
	check(t, os.WriteFile(path, []byte(text), 0o644))
}

func runtimeCurrent(t *testing.T, home string) string {
	t.Helper()
	current, err := bundle.Current(home)
	check(t, err)
	return current
}

// py: tests/test_install.py::test_native_launchers_are_preserved_and_legacy_wrappers_restore_signed_transport
func TestNativeLaunchersArePreservedAndLegacyWrappersRestoreSignedTransport(t *testing.T) {
	dir := setUp(t)
	for name := range NativeServers {
		original := loadObject(t, `{"type": "stdio", "command": "/native/launcher", "args": ["native"],
			"cwd": "/native", "env": {"NATIVE": "1"}, "env_vars": ["NATIVE_PIPE"]}`)
		server := json.NewObject("name", name, "enabled", true, "startup_timeout_sec", json.Int("10"),
			"tool_timeout_sec", json.Int("3600"), "transport", original)
		native, err := BuildCodexPlan([]*json.Object{server}, dir+"/state", dir+"/package", nil, nil, dir, nil)
		check(t, err)
		for _, item := range dig(native, "config_rpc", "params", "edits").([]any) {
			if strings.HasPrefix(dig(item, "keyPath").(string), "mcp_servers.") {
				t.Fatalf("%s: native server was wrapped", name)
			}
		}
		wrappedTransport := original.Copy()
		wrappedTransport.Set("command", "/old/bin/agent-decisions-isolate")
		wrappedTransport.Set("args", []any{"--server", name, "--host", "codex"})
		wrapped := server.Copy()
		wrapped.Set("transport", wrappedTransport)
		recipe := original.Copy()
		recipe.Delete("type")
		previous := json.NewObject("servers", json.NewObject(name, recipe))
		migrated, err := BuildCodexPlan([]*json.Object{wrapped}, dir+"/state", dir+"/package", nil, nil, dir, previous)
		check(t, err)
		restored := edit(dig(migrated, "config_rpc", "params", "edits"), "mcp_servers."+name)
		for _, key := range recipe.Keys() {
			wantEqual(t, dig(restored, "value", key), recipe.Value(key))
		}
		wantEqual(t, dig(restored, "value", "tool_timeout_sec"), json.Int("3600"))
		wantEqual(t, dig(migrated, "servers", name, "command"), "/native/launcher")
		wantEqual(t, dig(migrated, "installation_entry", "spec", "review_mode"), "required")
	}
}

// py: tests/test_install.py::test_codex_install_preserves_default_sandbox_and_approval_settings
func TestCodexInstallPreservesDefaultSandboxAndApprovalSettings(t *testing.T) {
	dir := setUp(t)
	edits, err := CodexConfigEdits(nil, dir+"/state", dir+"/installed",
		loadObject(t, `{"sandbox_mode": "workspace-write"}`), "", nil)
	check(t, err)
	wantEqual(t, []any{dig(edits, 0, "keyPath")}, []any{"permissions.agent-decisions-isolation"})
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
}

// py: tests/test_install.py::test_existing_cli_profile_is_write_protected
func TestExistingCliProfileIsWriteProtected(t *testing.T) {
	dir := setUp(t)
	profile := dir + "/.codex/cli.config.toml"
	writeFile(t, profile, "model = \"fixture-model\"\n")
	value, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed", nil, dir)
	check(t, err)
	wantEqual(t, dig(value, "filesystem", canonical(t, profile)), "read")
	data, err := os.ReadFile(profile)
	check(t, err)
	wantEqual(t, string(data), "model = \"fixture-model\"\n")
}

// py: tests/test_install.py::test_cli_profile_symlink_and_target_are_both_write_protected
func TestCliProfileSymlinkAndTargetAreBothWriteProtected(t *testing.T) {
	dir := setUp(t)
	target := dir + "/managed-cli.toml"
	writeFile(t, target, "model = \"fixture-model\"\n")
	profile := dir + "/.codex/cli.config.toml"
	check(t, os.Mkdir(dir+"/.codex", 0o755))
	check(t, os.Symlink(target, profile))
	value, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed", nil, dir)
	check(t, err)
	wantEqual(t, dig(value, "filesystem", profile), "read")
	wantEqual(t, dig(value, "filesystem", target), "read")
}

// py: tests/test_install.py::test_installed_version_protects_other_active_versions_of_this_plugin
func TestInstalledVersionProtectsOtherActiveVersionsOfThisPlugin(t *testing.T) {
	dir := setUp(t)
	namespace := dir + "/.codex/plugins/cache/market/agent-decisions"
	root := namespace + "/1.0.2"
	check(t, os.MkdirAll(root, 0o755))
	value, err := CodexPermissionProfileValue(dir+"/state", root, nil, dir)
	check(t, err)
	wantEqual(t, dig(value, "filesystem", namespace), "read")
	if has(dig(value, "filesystem").(*json.Object), filepath.Dir(namespace)) {
		t.Fatal("the market directory is protected")
	}
}

// py: tests/test_install.py::test_installed_git_checkout_is_still_authority_code
func TestInstalledGitCheckoutIsStillAuthorityCode(t *testing.T) {
	dir := setUp(t)
	namespace := dir + "/.codex/plugins/cache/market/agent-decisions"
	root := namespace + "/1.0.3"
	check(t, os.MkdirAll(root+"/.git", 0o755))
	got, err := AuthorityCodeRoot(root)
	check(t, err)
	wantEqual(t, got, namespace)
}

// py: tests/test_install.py::test_codex_wraps_only_stdio_servers_and_preserves_fields_in_recipe_and_config
func TestCodexWrapsOnlyStdioServersAndPreservesFieldsInRecipeAndConfig(t *testing.T) {
	dir := setUp(t)
	root := dir + "/installed"
	check(t, os.Mkdir(root, 0o755))
	servers := loadObjects(t, `[
		{"name": "local", "enabled": true, "startup_timeout_sec": 3, "tool_timeout_sec": 4, "source": "/plugin/source.toml",
		 "transport": {"type": "stdio", "command": "/bin/echo", "args": ["ok"], "cwd": "`+dir+`",
		               "env": {"TOKEN": "secret"}, "env_vars": ["INHERITED"]}},
		{"name": "remote", "enabled": true, "transport": {"type": "streamable_http", "url": "https://example.invalid/mcp"}}]`)
	plan, err := BuildCodexPlan(servers, dir+"/state", root,
		loadObject(t, `{"default_permissions": ":workspace", "workspace_network": true}`), nil, "", nil)
	check(t, err)
	wantEqual(t, toAnyList(keys(dig(plan, "servers"))), []any{"local"})
	wantEqual(t, dig(plan, "servers", "local", "command"), "/bin/echo")
	wantEqual(t, dig(plan, "servers", "local", "env"), load(t, `{"TOKEN": "secret"}`))
	wantEqual(t, dig(plan, "servers", "local", "source"), "/plugin/source.toml")
	edits := dig(plan, "config_rpc", "params", "edits")
	local := edit(edits, "mcp_servers.local")
	wantEqual(t, dig(local, "mergeStrategy"), "upsert")
	wantEqual(t, dig(local, "value", "command"), runtimeCurrent(t, "")+"/bin/agent-decisions-isolate")
	wantEqual(t, dig(local, "value", "args"), []any{"--server", "local", "--host", "codex"})
	wantEqual(t, dig(local, "value", "startup_timeout_sec"), json.Int("3"))
	wantEqual(t, dig(local, "value", "tool_timeout_sec"), json.Int("4"))
	if has(local.Value("value").(*json.Object), "auth_status") {
		t.Fatal("auth_status reached the config")
	}
	if edit(edits, "mcp_servers.remote") != nil {
		t.Fatal("the remote server was wrapped")
	}
}

// py: tests/test_install.py::test_codex_permission_profile_extends_prior_default_and_uses_read_for_write_protects
func TestCodexPermissionProfileExtendsPriorDefaultAndUsesReadForWriteProtects(t *testing.T) {
	dir := setUp(t)
	root := dir + "/installed"
	check(t, os.Mkdir(root, 0o755))
	current := loadObject(t, `{"default_permissions": "trusted-profile", "workspace_network": false}`)
	value, err := CodexPermissionProfileValue(dir+"/state", root, current, dir+"/home")
	check(t, err)
	wantEqual(t, dig(value, "extends"), "trusted-profile")
	wantEqual(t, dig(value, "filesystem", dir+"/state"), "deny")
	wantEqual(t, dig(value, "filesystem", root), "read")
	wantEqual(t, dig(value, "filesystem", dir+"/home/.codex/sessions"), "read")
	wantEqual(t, dig(value, "filesystem", dir+"/home/.codex/config.toml"), "read")
	if has(value, "network") {
		t.Fatal("network was set")
	}
}

// py: tests/test_install.py::test_codex_permission_profile_preserves_workspace_network_when_no_prior_profile
func TestCodexPermissionProfilePreservesWorkspaceNetworkWhenNoPriorProfile(t *testing.T) {
	dir := setUp(t)
	value, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed",
		loadObject(t, `{"workspace_network": true}`), dir+"/home")
	check(t, err)
	wantEqual(t, dig(value, "extends"), ":workspace")
	wantEqual(t, dig(value, "network"), load(t, `{"enabled": true}`))
}

// py: tests/test_install.py::test_codex_permission_profile_idempotently_preserves_original_base
func TestCodexPermissionProfileIdempotentlyPreservesOriginalBase(t *testing.T) {
	dir := setUp(t)
	current := loadObject(t, `{"default_permissions": "agent-decisions-isolation",
		"profile_extends": {"agent-decisions-isolation": "original-base"}, "workspace_network": false}`)
	value, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed", current, "")
	check(t, err)
	wantEqual(t, dig(value, "extends"), "original-base")
}

// py: tests/test_install.py::test_codex_permission_profile_does_not_extend_itself_after_partial_config
func TestCodexPermissionProfileDoesNotExtendItselfAfterPartialConfig(t *testing.T) {
	dir := setUp(t)
	missing := loadObject(t, `{"default_permissions": "agent-decisions-isolation", "profile_extends": {},
		"sandbox_mode": "read-only"}`)
	self := loadObject(t, `{"default_permissions": "agent-decisions-isolation",
		"profile_extends": {"agent-decisions-isolation": "agent-decisions-isolation"}}`)
	missingValue, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed", missing, "")
	check(t, err)
	selfValue, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed", self, "")
	check(t, err)
	wantEqual(t, dig(missingValue, "extends"), ":read-only")
	wantEqual(t, dig(selfValue, "extends"), ":workspace")
}

// py: tests/test_install.py::test_codex_reinstall_keeps_prior_recipe_for_already_wrapped_server
func TestCodexReinstallKeepsPriorRecipeForAlreadyWrappedServer(t *testing.T) {
	dir := setUp(t)
	root := dir + "/new-install"
	check(t, os.Mkdir(root, 0o755))
	previous := loadObject(t, `{"servers": {"local": {"command": "/original/server", "args": ["--ok"],
		"env": {"SECRET": "value"}, "env_vars": [], "cwd": null, "source": "prior"}}}`)
	servers := loadObjects(t, `[{"name": "local", "transport": {"type": "stdio",
		"command": "/old/bin/agent-decisions-isolate", "args": ["--server", "local", "--host", "codex"]}}]`)
	plan, err := BuildCodexPlan(servers, dir+"/state", root, nil, nil, "", previous)
	check(t, err)
	wantEqual(t, dig(plan, "servers", "local", "command"), "/original/server")
	local := edit(dig(plan, "config_rpc", "params", "edits"), "mcp_servers.local")
	wantEqual(t, dig(local, "value", "command"), runtimeCurrent(t, "")+"/bin/agent-decisions-isolate")
}

// py: tests/test_install.py::test_codex_mcp_edit_omits_nullable_options_and_quotes_dotted_names
func TestCodexMcpEditOmitsNullableOptionsAndQuotesDottedNames(t *testing.T) {
	dir := setUp(t)
	root := dir + "/installed"
	check(t, os.Mkdir(root, 0o755))
	servers := loadObjects(t, `[{"name": "plugin.server:one", "enabled": true, "startup_timeout_sec": null,
		"tool_timeout_sec": 30, "disabled_reason": null, "transport": {"type": "stdio", "command": "/bin/echo",
		"args": [], "cwd": null, "env": null, "env_vars": [], "http_headers": null}}]`)
	edits, err := CodexConfigEdits(servers, dir+"/state", root, json.NewObject(), "", nil)
	check(t, err)
	server := edits[1].(*json.Object)
	wantEqual(t, server.Value("keyPath"), `mcp_servers."plugin.server:one"`)
	value := server.Value("value").(*json.Object)
	for _, key := range []string{"startup_timeout_sec", "cwd", "env", "http_headers"} {
		if has(value, key) {
			t.Fatalf("%s reached the config", key)
		}
	}
	wantEqual(t, value.Value("tool_timeout_sec"), json.Int("30"))
}

// py: tests/test_install.py::test_codex_source_dict_prefers_manifest_path_identity
func TestCodexSourceDictPrefersManifestPathIdentity(t *testing.T) {
	dir := setUp(t)
	server := loadObject(t, `{"name": "local", "source": {"manifest_path": "/plugin/.mcp.json", "other": null},
		"transport": {"type": "stdio", "command": "/bin/echo"}}`)
	recipe, err := ServerRecipe(server, dir+"/installed", nil)
	check(t, err)
	wantEqual(t, recipe.Value("source"), "/plugin/.mcp.json")
}

// py: tests/test_install.py::test_codex_config_parser_keeps_hashes_inside_strings_and_legacy_sandbox_mode
func TestCodexConfigParserKeepsHashesInsideStringsAndLegacySandboxMode(t *testing.T) {
	setUp(t)
	summary := ParseCodexConfigSummary(`
default_permissions = "profile#with-hash" # trailing comment
sandbox_mode = "read-only"
[permissions."profile#with-hash"]
extends = ":danger-full-access"
[sandbox_workspace_write]
network_access = true # comment
`)
	wantEqual(t, summary.Value("default_permissions"), "profile#with-hash")
	wantEqual(t, summary.Value("sandbox_mode"), "read-only")
	wantEqual(t, dig(summary, "profile_extends", "profile#with-hash"), ":danger-full-access")
	wantEqual(t, summary.Value("workspace_network"), true)
}

// py: tests/test_install.py::test_codex_permission_profile_preserves_legacy_read_only_when_no_default_permissions
func TestCodexPermissionProfilePreservesLegacyReadOnlyWhenNoDefaultPermissions(t *testing.T) {
	dir := setUp(t)
	value, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed",
		loadObject(t, `{"sandbox_mode": "read-only"}`), dir+"/home")
	check(t, err)
	wantEqual(t, value.Value("extends"), ":read-only")
}

// py: tests/test_install.py::test_codex_permission_profile_preserves_legacy_danger_full_access_when_no_default_permissions
func TestCodexPermissionProfilePreservesLegacyDangerFullAccessWhenNoDefaultPermissions(t *testing.T) {
	dir := setUp(t)
	value, err := CodexPermissionProfileValue(dir+"/state", dir+"/installed",
		loadObject(t, `{"sandbox_mode": "danger-full-access"}`), dir+"/home")
	check(t, err)
	wantEqual(t, value.Value("extends"), ":danger-full-access")
}

// py: tests/test_install.py::test_claude_merge_is_idempotent_and_preserves_existing_settings
func TestClaudeMergeIsIdempotentAndPreservesExistingSettings(t *testing.T) {
	dir := setUp(t)
	settings := loadObject(t, `{"model": "keep", "env": {"CLAUDE_CODE_SHELL_PREFIX": "/old/prefix", "VISIBLE": "ok"},
		"permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(/already)"]},
		"sandbox": {"filesystem": {"denyWrite": ["/already"]}},
		"mcpServers": {"local": {"command": "/bin/echo", "args": ["ok"], "env": {"SECRET": "value"}, "cwd": "`+dir+`"}}}`)
	protected := []string{dir + "/transcripts", dir + "/settings.json"}
	prefix := dir + "/bin/agent-decisions-shell-prefix"
	once, previous, err := MergeClaudeSettings(settings, prefix, dir+"/state", protected, nil, nil)
	check(t, err)
	twice, previousAgain, err := MergeClaudeSettings(once, prefix, dir+"/state", protected, nil, nil)
	check(t, err)
	if previous == nil || *previous != "/old/prefix" || previousAgain != nil {
		t.Fatalf("previous prefixes = %v, %v", previous, previousAgain)
	}
	wantEqual(t, once, twice)
	wantEqual(t, once.Value("model"), "keep")
	wantEqual(t, dig(once, "permissions", "allow"), []any{"Bash(ls:*)"})
	if !contains(dig(once, "sandbox", "filesystem", "denyRead"), dir+"/state") ||
		!contains(dig(once, "sandbox", "filesystem", "denyWrite"), dir+"/state") {
		t.Fatal("the state directory is not denied")
	}
	deny := dig(once, "permissions", "deny")
	if !contains(deny, "Write(/"+protected[0]+"/**)") || contains(deny, "Read(/"+protected[0]+"/**)") {
		t.Fatalf("deny = %v", deny)
	}
}

// py: tests/test_install.py::test_claude_plan_records_servers_matchers_previous_prefix_and_write_protected_paths
func TestClaudePlanRecordsServersMatchersPreviousPrefixAndWriteProtectedPaths(t *testing.T) {
	dir := setUp(t)
	settings := loadObject(t, `{"env": {"CLAUDE_CODE_SHELL_PREFIX": "/old/prefix"},
		"mcpServers": {"local": {"command": "/bin/echo", "args": ["ok"], "env_vars": ["A"], "env": {"SECRET": "value"}}}}`)
	root := dir + "/installed"
	check(t, os.Mkdir(root, 0o755))
	plan, err := BuildClaudePlan(settings, dir+"/state", root, dir+"/home", nil, nil, nil)
	check(t, err)
	wantEqual(t, dig(plan, "servers", "local", "command"), "/bin/echo")
	wantEqual(t, dig(plan, "claude_matchers"), load(t, `[["/bin/echo", "ok"]]`))
	wantEqual(t, dig(plan, "previous_shell_prefix"), "/old/prefix")
	protected := dig(plan, "installation_entry", "spec", "protected_paths")
	claudeJSON := dir + "/home/.claude.json"
	for _, path := range []string{dir + "/home/.claude/projects", dir + "/home/.claude/settings.json", claudeJSON,
		dir + "/home/.claude/plugins/installed_plugins.json"} {
		if !contains(protected, path) {
			t.Fatalf("%s is not protected", path)
		}
	}
	if !contains(dig(plan, "protected_paths"), claudeJSON) ||
		!contains(dig(plan, "merged_settings", "permissions", "deny"), "Write(/"+claudeJSON+"/**)") {
		t.Fatal(".claude.json is not write-protected")
	}
}

// py: tests/test_install.py::test_claude_reinstall_uses_signed_previous_prefix_instead_of_self_recursing
func TestClaudeReinstallUsesSignedPreviousPrefixInsteadOfSelfRecursing(t *testing.T) {
	dir := setUp(t)
	root := dir + "/installed"
	check(t, os.Mkdir(root, 0o755))
	settings := json.NewObject("env", json.NewObject("CLAUDE_CODE_SHELL_PREFIX", root+"/bin/agent-decisions-shell-prefix"))
	plan, err := BuildClaudePlan(settings, dir+"/state", root, "", nil, nil,
		loadObject(t, `{"previous_shell_prefix": "/prior/prefix"}`))
	check(t, err)
	wantEqual(t, dig(plan, "previous_shell_prefix"), "/prior/prefix")
	wantEqual(t, dig(plan, "installation_entry", "spec", "previous_shell_prefix"), "/prior/prefix")
}

// py: tests/test_install.py::test_claude_inventory_reads_user_json_and_plugin_manifests_without_disabling_servers
func TestClaudeInventoryReadsUserJsonAndPluginManifestsWithoutDisablingServers(t *testing.T) {
	setUp(t)
	settings := loadObject(t, `{"mcpServers": {"settings-local": {"command": "/bin/echo"}}}`)
	configs := sourced(load(t, `[["/home/.claude.json", {"mcpServers": {"adrafinil": {"command": "/bin/echo", "args": ["a"]}}}],
		["/home/.claude-primary/.claude.json", {"mcpServers": {"notes": {"command": "/bin/echo"}}}]]`))
	manifests := sourced(load(t, `[["/plugin/claude-mem/.mcp.json",
		{"mcpServers": {"claude-mem": {"command": "/bin/echo", "args": ["m"]}}}]]`))
	servers, err := ClaudeServersFromSources(settings, configs, manifests)
	check(t, err)
	wantEqual(t, toAnyList(keys(servers)), []any{"adrafinil", "claude-mem", "notes", "settings-local"})
	wantEqual(t, dig(servers, "adrafinil", "source"), "/home/.claude.json")
	wantEqual(t, dig(servers, "notes", "source"), "/home/.claude-primary/.claude.json")
	wantEqual(t, dig(servers, "claude-mem", "source"), "/plugin/claude-mem/.mcp.json")
}

// py: tests/test_install.py::test_claude_config_dirs_cover_every_claude_json_and_the_selected_dir
func TestClaudeConfigDirsCoverEveryClaudeJsonAndTheSelectedDir(t *testing.T) {
	dir := setUp(t)
	home, selected := dir+"/home", dir+"/elsewhere"
	for _, root := range []string{home + "/.claude-primary", home + "/.claude-scratch", selected} {
		check(t, os.MkdirAll(root, 0o755))
	}
	writeFile(t, home+"/.claude-primary/.claude.json", "{}")
	t.Setenv("CLAUDE_CONFIG_DIR", selected)
	protected, err := ProtectedWritePaths("claude", dir+"/package", home)
	check(t, err)
	for _, pair := range [][2]string{{home + "/.claude", home + "/.claude.json"},
		{home + "/.claude-primary", home + "/.claude-primary/.claude.json"}, {selected, selected + "/.claude.json"}} {
		root := pair[0]
		for _, path := range []string{pair[1], root + "/settings.json", root + "/settings.local.json",
			root + "/plugins/installed_plugins.json", root + "/projects"} {
			if !slices.Contains(protected, canonical(t, path)) {
				t.Fatalf("%s is not protected", path)
			}
		}
	}
	for _, path := range protected {
		if strings.Contains(path, ".claude-scratch") { // no .claude.json, not a config dir
			t.Fatalf("%s is protected", path)
		}
	}
}

// py: tests/test_install.py::test_claude_transcript_denies_cover_jsonl_and_leave_memory_writable
func TestClaudeTranscriptDeniesCoverJsonlAndLeaveMemoryWritable(t *testing.T) {
	dir := setUp(t)
	home := dir + "/home"
	projects := canonical(t, home+"/.claude/projects")
	settings := json.NewObject("permissions", json.NewObject("deny",
		[]any{"Write(/" + projects + "/**)", "Edit(/" + projects + ")", "Bash(rm *)"}))
	plan, err := BuildClaudePlan(settings, dir+"/state", dir+"/package", home, nil, nil, nil)
	check(t, err)
	deny := dig(plan, "merged_settings", "permissions", "deny")
	if !contains(deny, "Write(/"+projects+"/**/*.jsonl)") || !contains(deny, "Edit(/"+projects+"/**/*.jsonl)") ||
		!contains(deny, "Bash(rm *)") {
		t.Fatalf("deny = %v", deny)
	}
	for _, rule := range deny.([]any) {
		if strings.Contains(rule.(string), "/"+projects) && !strings.HasSuffix(rule.(string), "/**/*.jsonl)") {
			t.Fatalf("%s closes memory files", rule) // memory/*.md stays open
		}
	}
	if !contains(dig(plan, "merged_settings", "sandbox", "filesystem", "denyWrite"), projects) {
		t.Fatal("projects is writable")
	}
}

// py: tests/test_install.py::test_claude_native_names_are_exempt_only_from_owner_config
func TestClaudeNativeNamesAreExemptOnlyFromOwnerConfig(t *testing.T) {
	dir := setUp(t)
	settings := loadObject(t, `{"mcpServers": {"node_repl": {"command": "/native/node_repl"},
		"local": {"command": "/bin/echo", "args": ["ok"]}}}`)
	configs := sourced(load(t, `[["/home/.claude.json", {"mcpServers": {"adrafinil": {"command": "/native/adrafinil"},
		"computer-use": {"command": "/native/computer-use"}}}]]`))
	manifests := sourced(load(t, `[["/plugin/other/.mcp.json", {"mcpServers": {"messages": {"command": "/plugin/messages"},
		"computer-use": {"command": "/plugin/computer-use"}}}]]`))
	plan, err := BuildClaudePlan(settings, dir+"/state", dir+"/package", dir, configs, manifests, nil)
	check(t, err)
	wantEqual(t, toAnyList(keys(dig(plan, "servers"))), []any{"adrafinil", "computer-use", "local", "messages", "node_repl"})
	wantEqual(t, dig(plan, "claude_matchers"), load(t, `[["/bin/echo", "ok"], ["/plugin/computer-use"], ["/plugin/messages"]]`))
}

// py: tests/test_install.py::test_claude_cli_reads_discovered_configs_unless_paths_are_explicit
func TestClaudeCliReadsDiscoveredConfigsUnlessPathsAreExplicit(t *testing.T) {
	dir := setUp(t)
	home := dir + "/home"
	t.Setenv("HOME", home) // Path.home is patched to home
	writeFile(t, home+"/.claude.json", `{"mcpServers": {"default": {"command": "/bin/echo"}}}`)
	writeFile(t, home+"/.claude-primary/.claude.json", `{"mcpServers": {"primary": {"command": "/bin/echo"}}}`)
	explicit := dir + "/explicit.json"
	writeFile(t, explicit, `{"mcpServers": {"explicit": {"command": "/bin/echo"}}}`)
	servers := func(extra ...string) []any {
		var stdout, stderr bytes.Buffer
		argv := append([]string{"--host", "claude", "--state-root", dir + "/state", "--install-root", dir + "/package"}, extra...)
		if code := Main(argv, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		return toAnyList(keys(dig(load(t, stdout.String()), "plans", 0, "servers")))
	}
	wantEqual(t, servers(), []any{"default", "primary"})
	wantEqual(t, servers("--claude-json", explicit), []any{"explicit"})
}

// py: tests/test_install.py::test_claude_manifest_discovery_uses_install_path_not_stale_cache
func TestClaudeManifestDiscoveryUsesInstallPathNotStaleCache(t *testing.T) {
	dir := setUp(t)
	home := dir + "/home"
	active := home + "/.claude/plugins/cache/thedotmack/claude-mem/13.12.4"
	stale := home + "/.claude/plugins/cache/thedotmack/claude-mem/12.0.0"
	writeFile(t, active+"/.mcp.json", "{}")
	writeFile(t, stale+"/.mcp.json", "{}")
	writeFile(t, home+"/.claude/plugins/installed_plugins.json",
		`{"plugins": {"claude-mem@thedotmack": {"installPath": "`+active+`"}}}`)
	manifests, err := DiscoverClaudePluginManifests(home)
	check(t, err)
	wantEqual(t, toAnyList(manifests), []any{active + "/.mcp.json"})
}

// py: tests/test_install.py::test_claude_manifest_discovery_skips_disabled_plugin_and_empty_registry
func TestClaudeManifestDiscoverySkipsDisabledPluginAndEmptyRegistry(t *testing.T) {
	dir := setUp(t)
	home := dir + "/home"
	disabled := home + "/.claude/plugins/cache/local/disabled/1.0.0"
	writeFile(t, disabled+"/.mcp.json", "{}")
	writeFile(t, home+"/.codex/plugins/cache/local/cached/1.0.0/.mcp.json", "{}")
	registry := home + "/.claude/plugins/installed_plugins.json"
	writeFile(t, registry, `{"plugins": {"disabled@local": {"installPath": "`+disabled+`",
		"settings": {"enabledPlugins": false}}}}`)
	manifests, err := DiscoverClaudePluginManifests(home)
	check(t, err)
	if len(manifests) != 0 {
		t.Fatalf("manifests = %v", manifests)
	}
	writeFile(t, registry, `{"plugins": {}}`)
	manifests, err = DiscoverClaudePluginManifests(home)
	check(t, err)
	if len(manifests) != 0 {
		t.Fatalf("manifests = %v", manifests)
	}
}

// py: tests/test_install.py::test_read_claude_plugin_manifest_expands_plugin_root_macros
func TestReadClaudePluginManifestExpandsPluginRootMacros(t *testing.T) {
	dir := setUp(t)
	root := dir + "/plugin"
	manifest := root + "/.mcp.json"
	writeFile(t, manifest, `{"mcpServers": {"macro": {"command": "${CLAUDE_PLUGIN_ROOT}/bin/server",
		"args": ["--root", "${PLUGIN_ROOT}"], "cwd": "${CLAUDE_PLUGIN_ROOT}", "env": {"DATA_DIR": "${PLUGIN_ROOT}/data"}}}}`)
	loaded, err := ReadClaudePluginManifests([]string{manifest})
	check(t, err)
	servers, err := ClaudeServersFromSources(json.NewObject(), nil, loaded)
	check(t, err)
	wantEqual(t, dig(servers, "macro", "command"), root+"/bin/server")
	wantEqual(t, dig(servers, "macro", "args"), []any{"--root", root})
	wantEqual(t, dig(servers, "macro", "cwd"), root)
	wantEqual(t, dig(servers, "macro", "env"), json.NewObject("DATA_DIR", root+"/data"))
}

// py: tests/test_install.py::test_signed_installation_entry_round_trips_and_tamper_is_ignored
func TestSignedInstallationEntryRoundTripsAndTamperIsIgnored(t *testing.T) {
	dir := setUp(t)
	entry, err := InstallationEntry("codex", dir+"/state", json.NewObject(), []any{}, nil, []string{dir + "/config"})
	check(t, err)
	wantEqual(t, dig(entry, "spec", "review_mode"), "required")
	check(t, SignInstallation(entry))
	state, err := policy.StateDir()
	check(t, err)
	path := state + "/sessions/installation.jsonl"
	data, err := os.ReadFile(path)
	check(t, err)
	row := loadObject(t, strings.SplitN(string(data), "\n", 2)[0])
	dig(row, "entry", "spec").(*json.Object).Set("state_root", "/attacker")
	tampered, err := json.Dumps(row, json.Options{SortKeys: true})
	check(t, err)
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	check(t, err)
	_, err = handle.WriteString(tampered + "\n")
	check(t, errors.Join(err, handle.Close()))
	spec, err := isolation.LatestInstallationSpec("codex")
	check(t, err)
	wantEqual(t, spec.Value("state_root"), dir+"/state")
}

// py: tests/test_install.py::test_sanitized_plan_does_not_print_env_values
func TestSanitizedPlanDoesNotPrintEnvValues(t *testing.T) {
	setUp(t)
	plan := load(t, `{"servers": {"local": {"env": {"TOKEN": "super-secret"}, "bearer_token": "abc", "env_vars": ["TOKEN"]}}}`)
	sanitized, err := Sanitize(plan)
	check(t, err)
	text, err := json.Dumps(sanitized, json.Options{SortKeys: true})
	check(t, err)
	if strings.Contains(text, "super-secret") || strings.Contains(text, "abc") || !strings.Contains(text, "TOKEN") {
		t.Fatal(text)
	}
}

// py: tests/test_install.py::test_sanitized_plan_redacts_plain_args_urls_and_secret_queries
func TestSanitizedPlanRedactsPlainArgsUrlsAndSecretQueries(t *testing.T) {
	setUp(t)
	plan := load(t, `{"servers": {"local": {
		"args": ["--api-key", "arg-secret-value", "--token=token-secret-value", "--password", "password-secret-value",
		         "--safe", "visible-value"],
		"command": "tool --token inline-secret-value OPENAI_API_KEY=assignment-secret-value",
		"url": "https://user:url-secret-value@example.invalid/mcp?access_token=query-secret-value&ok=visible&api-key=api-query-secret",
		"env_vars": ["TOKEN"]}}}`)
	sanitized, err := Sanitize(plan)
	check(t, err)
	text, err := json.Dumps(sanitized, json.Options{SortKeys: true})
	check(t, err)
	for _, secret := range []string{"arg-secret-value", "token-secret-value", "password-secret-value",
		"inline-secret-value", "assignment-secret-value", "url-secret-value", "query-secret-value", "api-query-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked: %s", secret, text)
		}
	}
	for _, visible := range []string{"visible-value", "ok=visible", "TOKEN", "<redacted>@example.invalid"} {
		if !strings.Contains(text, visible) {
			t.Fatalf("%s missing: %s", visible, text)
		}
	}
}

// writerFunc is the mock.Mock writer of the Python tests.
type writerFunc func(method string, params *json.Object) (*json.Object, error)

func (f writerFunc) Call(method string, params *json.Object) (*json.Object, error) {
	return f(method, params)
}

// codexApplyPlan builds the Codex plan of the apply tests: the repository is the install root, as in
// Python, and the runtime bundle goes to the test directory.
func codexApplyPlan(t *testing.T, dir, servers, config string) *json.Object {
	t.Helper()
	writeFile(t, dir+"/config.toml", config)
	configPath := dir + "/config.toml"
	plan, err := BuildCodexPlan(loadObjects(t, servers), dir+"/state", os.Getenv("AGENT_DECISIONS_SOURCE"),
		json.NewObject(), &configPath, dir, nil)
	check(t, err)
	return plan
}

// py: tests/test_install.py::test_apply_codex_uses_config_rpc_without_rewriting_toml
func TestApplyCodexUsesConfigRpcWithoutRewritingToml(t *testing.T) {
	dir := setUp(t)
	plan := codexApplyPlan(t, dir, `[]`, "default_permissions = \":workspace\"\n")
	var calls []string
	result, err := ApplyCodexPlan(plan, writerFunc(func(method string, params *json.Object) (*json.Object, error) {
		spec, err := isolation.LatestInstallationSpec("codex")
		check(t, err)
		wantEqual(t, spec.Value("host"), "codex")
		wantEqual(t, params, dig(plan, "config_rpc", "params"))
		calls = append(calls, method)
		return json.NewObject("ok", true), nil
	}))
	check(t, err)
	wantEqual(t, toAnyList(calls), []any{"config/batchWrite"})
	data, err := os.ReadFile(dir + "/config.toml")
	check(t, err)
	wantEqual(t, string(data), "default_permissions = \":workspace\"\n")
	if !ospath.Exists(result.Value("backup").(string)) {
		t.Fatal("no backup")
	}
	spec, err := isolation.LatestInstallationSpec("codex")
	check(t, err)
	wantEqual(t, spec.Value("host"), "codex")
}

// py: tests/test_install.py::test_apply_codex_failure_keeps_signed_recipe_available_for_wrappers
func TestApplyCodexFailureKeepsSignedRecipeAvailableForWrappers(t *testing.T) {
	dir := setUp(t)
	plan := codexApplyPlan(t, dir, `[{"name": "local", "transport": {"type": "stdio", "command": "/bin/echo", "args": ["ok"]}}]`, "")
	_, err := ApplyCodexPlan(plan, writerFunc(func(string, *json.Object) (*json.Object, error) {
		return nil, installError("config write failed")
	}))
	if pyerr.Class(err) != "InstallError" {
		t.Fatalf("err = %v, want InstallError", err)
	}
	spec, err := isolation.LatestInstallationSpec("codex")
	check(t, err)
	wantEqual(t, dig(spec, "servers", "local", "command"), "/bin/echo")
}

// py: tests/test_install.py::test_backup_file_uses_private_permissions_and_unique_names
func TestBackupFileUsesPrivatePermissionsAndUniqueNames(t *testing.T) {
	dir := setUp(t)
	config := dir + "/settings.json"
	writeFile(t, config, "{\"secret\":\"value\"}\n")
	check(t, os.Chmod(config, 0o644))
	first, err := BackupFile(config)
	check(t, err)
	second, err := BackupFile(config)
	check(t, err)
	if *first == *second {
		t.Fatal("backups share a name")
	}
	data, err := os.ReadFile(*first)
	check(t, err)
	wantEqual(t, string(data), "{\"secret\":\"value\"}\n")
	for _, path := range []string{*first, *second} {
		info, err := os.Stat(path)
		check(t, err)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", path, info.Mode().Perm())
		}
	}
}

// py: tests/test_install.py::test_write_json_uses_private_permissions
func TestWriteJsonUsesPrivatePermissions(t *testing.T) {
	dir := setUp(t)
	settings := dir + "/settings.json"
	check(t, WriteJSON(settings, json.NewObject("secret", "value")))
	info, err := os.Stat(settings)
	check(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(settings)
	check(t, err)
	wantEqual(t, load(t, string(data)), json.NewObject("secret", "value"))
}

func TestWriteJSONPreservesSettingsSymlink(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing_target", false: "new_target"}[existing], func(t *testing.T) {
			dir := setUp(t)
			target := filepath.Join(dir, "settings.json")
			alias := filepath.Join(dir, "primary-settings.json")
			if existing {
				writeFile(t, target, "{}\n")
			}
			check(t, os.Symlink("settings.json", alias))
			want := json.NewObject("enabledPlugins", json.NewObject("agent-decisions", true))
			check(t, WriteJSON(alias, want))
			link, err := os.Readlink(alias)
			check(t, err)
			if link != "settings.json" {
				t.Fatalf("settings link changed: %q", link)
			}
			data, err := os.ReadFile(target)
			check(t, err)
			wantEqual(t, load(t, string(data)), want)
			info, err := os.Stat(target)
			check(t, err)
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("target mode %o", info.Mode().Perm())
			}
		})
	}
}

// fakeCodexWriter returns a writer whose Codex binary is this test binary acting as the fake Codex, and
// the file where the fake records its arguments.
func fakeCodexWriter(t *testing.T, dir string, socket *string) (*CodexRpcWriter, string) {
	t.Helper()
	argsPath := dir + "/codex-args.json"
	t.Setenv(fakeCodexArgs, argsPath)
	binary, err := os.Executable()
	check(t, err)
	writer, err := NewCodexRpcWriter(binary, socket, 5)
	check(t, err)
	return writer, argsPath
}

// Replaces tests/test_install.py::test_real_codex_proxy_writer_round_trips_json_rpc_over_temp_socket,
// which ran the real Codex binary. A fake Codex checks that an existing control socket selects the proxy
// command, and the JSON-RPC exchange.
//
// py: tests/test_install.py::test_real_codex_proxy_writer_round_trips_json_rpc_over_temp_socket
func TestCodexWriterUsesProxyForExistingControlSocket(t *testing.T) {
	dir := setUp(t)
	socket := os.Getenv("HOME") + "/.codex/app-server-control/app-server-control.sock"
	writeFile(t, socket, "")
	writer, argsPath := fakeCodexWriter(t, dir, nil)
	response, err := writer.Call("config/read", json.NewObject("includeLayers", false))
	check(t, err)
	wantEqual(t, response.Value("result"), json.NewObject("method", "config/read"))
	data, err := os.ReadFile(argsPath)
	check(t, err)
	wantEqual(t, load(t, string(data)), []any{"app-server", "proxy", "--sock", socket})
	data, err = os.ReadFile(argsPath + ".methods")
	check(t, err)
	wantEqual(t, load(t, string(data)), []any{"initialize", "config/read"})
}

// py: tests/test_install.py::test_codex_writer_uses_stdio_fallback_when_no_socket_exists
func TestCodexWriterUsesStdioFallbackWhenNoSocketExists(t *testing.T) {
	dir := setUp(t)
	saved := defaultAppServerSocket
	t.Cleanup(func() { defaultAppServerSocket = saved })
	defaultAppServerSocket = func() (*string, error) { return nil, nil }
	writer, argsPath := fakeCodexWriter(t, dir, nil)
	response, err := writer.Call("config/read", json.NewObject())
	check(t, err)
	data, err := os.ReadFile(argsPath)
	check(t, err)
	wantEqual(t, load(t, string(data)), []any{"app-server", "--stdio"})
	data, err = os.ReadFile(argsPath + ".methods")
	check(t, err)
	wantEqual(t, load(t, string(data)), []any{"initialize", "config/read"})
	wantEqual(t, response.Value("result"), json.NewObject("method", "config/read"))
}

// py: tests/test_install.py::test_cli_dry_run_uses_sanitized_output_and_does_not_apply
func TestCliDryRunUsesSanitizedOutputAndDoesNotApply(t *testing.T) {
	dir := setUp(t)
	mcp := dir + "/mcp.json"
	writeFile(t, mcp, `{"servers": [{"name": "local", "transport": {"type": "stdio", "command": "/bin/echo",
		"env": {"SECRET": "super-secret-cli"}}}]}`)
	writeFile(t, dir+"/config.toml", "")
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--host", "codex", "--state-root", dir + "/state", "--review-mode", "availability",
		"--install-root", dir + "/installed", "--codex-mcp-json", mcp, "--codex-config", dir + "/config.toml"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, `"apply": false`) || strings.Contains(text, "super-secret-cli") ||
		!strings.Contains(text, "keyPath") {
		t.Fatal(text)
	}
	wantEqual(t, dig(load(t, text), "plans", 0, "installation_entry", "spec", "review_mode"), "availability")
	entries, err := ledger.Entries("installation", nil)
	check(t, err)
	if len(entries) != 0 {
		t.Fatalf("entries = %d", len(entries))
	}
}

// py: tests/test_install.py::test_cli_apply_refuses_state_root_that_does_not_match_policy_state_dir
func TestCliApplyRefusesStateRootThatDoesNotMatchPolicyStateDir(t *testing.T) {
	dir := setUp(t)
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--host", "codex", "--apply", "--state-root", dir + "/different-state",
		"--install-root", dir + "/installed"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--state-root must match AGENT_DECISIONS_STATE") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	entries, err := ledger.Entries("installation", nil)
	check(t, err)
	if len(entries) != 0 {
		t.Fatalf("entries = %d", len(entries))
	}
}

// py: tests/test_session_exceptions.py::test_exception_flags_require_explicit_codex_host
func TestExceptionFlagsRequireExplicitCodexHost(t *testing.T) {
	setUp(t)
	// Python patched read_codex_servers and read_json to fail; the default Codex binary must not run either.
	for _, host := range []string{"both", "claude"} {
		var stdout, stderr bytes.Buffer
		code := Main([]string{"--host", host, "--session-review-exception", "approved-sdk",
			"--codex-binary", "/nonexistent/codex", "--claude-settings", "/nonexistent/settings.json"}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "session review exceptions require --host codex") {
			t.Fatalf("%s: exit %d: %s", host, code, stderr.String())
		}
	}
}

// py: tests/test_session_exceptions.py::test_installer_preserves_and_explicitly_revokes_exception
func TestInstallerPreservesAndExplicitlyRevokesException(t *testing.T) {
	dir := setUp(t)
	prior := loadObject(t, `{"session_review_exceptions": ["approved-sdk"]}`)
	entry, err := InstallationEntry("codex", dir+"/state", json.NewObject(), []any{}, nil, nil)
	check(t, err)
	check(t, ConfigureSessionExceptions(entry, prior, nil, nil))
	wantEqual(t, dig(entry, "spec", "session_review_exceptions"), []any{"approved-sdk"})
	check(t, ConfigureSessionExceptions(entry, prior, []string{"other"}, []string{"approved-sdk"}))
	wantEqual(t, dig(entry, "spec", "session_review_exceptions"), []any{"other"})
}

// py: tests/test_runtime.py::test_plans_protect_namespace_and_keep_lexical_launch_paths
func TestPlansProtectNamespaceAndKeepLexicalLaunchPaths(t *testing.T) {
	dir := setUp(t)
	source := os.Getenv("AGENT_DECISIONS_SOURCE")
	active, err := bundle.Publish(source, dir)
	check(t, err)
	namespace, err := bundle.Namespace(dir)
	check(t, err)
	server := loadObjects(t, `[{"name": "local", "transport": {"type": "stdio", "command": "/bin/echo", "args": ["ok"]}}]`)
	plan, err := BuildCodexPlan(server, dir+"/state", source, nil, nil, dir, nil)
	check(t, err)
	edits := dig(plan, "config_rpc", "params", "edits")
	wantEqual(t, dig(edit(edits, "mcp_servers.local"), "value", "command"), active+"/bin/agent-decisions-isolate")
	if !contains(dig(plan, "protected_paths"), namespace) {
		t.Fatal("the namespace is not protected")
	}
	wantEqual(t, dig(edits, 0, "value", "filesystem", namespace), "read")
	claude, err := BuildClaudePlan(json.NewObject(), dir+"/state", source, dir, nil, nil, nil)
	check(t, err)
	wantEqual(t, dig(claude, "merged_settings", "env", "CLAUDE_CODE_SHELL_PREFIX"), active+"/bin/agent-decisions-shell-prefix")
	if !contains(dig(claude, "merged_settings", "sandbox", "filesystem", "denyWrite"), namespace) {
		t.Fatal("the namespace is writable")
	}
}

// DefaultInstallRoot is the plugin root that the bootstrap launcher names, else the directory two levels
// above the executable.
func TestDefaultInstallRoot(t *testing.T) {
	setUp(t)
	t.Setenv("AGENT_DECISIONS_SOURCE", "/plugin/root/")
	got, err := DefaultInstallRoot()
	check(t, err)
	wantEqual(t, got, "/plugin/root")
	os.Unsetenv("AGENT_DECISIONS_SOURCE")
	saved := executable
	t.Cleanup(func() { executable = saved })
	executable = func() (string, error) { return "/plugin/root/bin/agent-decisions", nil }
	got, err = DefaultInstallRoot()
	check(t, err)
	wantEqual(t, got, "/plugin/root")
}

// TestLauncherPathAtRootCwd checks that a relative install root under the working directory "/" gives
// "/x/...", as Path.absolute does, not "//x/...".
func TestLauncherPathAtRootCwd(t *testing.T) {
	t.Chdir("/")
	if got, err := IsolatePath("x"); got != "/x/bin/agent-decisions-isolate" || err != nil {
		t.Fatalf("IsolatePath = %q, %v", got, err)
	}
}

// TestMainPrintsTracebackLastLine checks the last traceback line of an exception that escapes cli: CPython
// names a class outside builtins by its module.
func TestMainPrintsTracebackLastLine(t *testing.T) {
	dir := setUp(t)
	for text, want := range map[string]string{
		"{":   "json.decoder.JSONDecodeError: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)\n",
		`"x"`: "agent_decisions.install.InstallError: MCP JSON must be an object or list\n",
	} {
		writeFile(t, dir+"/mcp.json", text)
		var stdout, stderr bytes.Buffer
		code := Main([]string{"--host", "codex", "--state-root", dir + "/state", "--install-root", dir + "/installed",
			"--codex-mcp-json", dir + "/mcp.json"}, &stdout, &stderr)
		if code != 1 || stderr.String() != want {
			t.Errorf("exit %d, stderr %q, want %q", code, stderr.String(), want)
		}
	}
}
