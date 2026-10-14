package install

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// oracleDriver runs install operations with the Python module and prints one outcome per operation:
// ["ok", json.dumps(result, sort_keys=True)] or [error class, str(error)]. The clock is fixed at
// oracleEpoch. The request and reply are ASCII JSON, so lone surrogates cross as escapes.
const oracleDriver = `
import io, json, os, sys, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
gmtime = time.gmtime
time.gmtime = lambda seconds=None: gmtime(int(sys.argv[3]) if seconds is None else seconds)
from agent_decisions import install

def home(value):
    return None if value is None else Path(value)

def run(op, *a):
    if op == "codex_plan":
        return install.build_codex_plan(a[0], a[1], a[2], a[3], config_path=a[4], home=home(a[5]), previous_spec=a[6])
    if op == "codex_edits":
        return install.codex_config_edits(a[0], a[1], a[2], a[3], home=home(a[4]), previous_spec=a[5])
    if op == "claude_plan":
        return install.build_claude_plan(a[0], a[1], a[2], home=home(a[3]), claude_configs=a[4],
                                         plugin_manifests=a[5], previous_spec=a[6])
    if op == "merge":
        return install.merge_claude_settings(a[0], a[1], a[2], a[3], previous_spec=a[4], transcripts=a[5])
    if op == "sanitize":
        return install.sanitize(a[0])
    if op == "summary":
        return install.parse_codex_config_summary(a[0])
    if op == "nfkc":
        import unicodedata
        return [c for c in range(0x80, 0x110000)
                if any(s in unicodedata.normalize("NFKC", chr(c)) for s in "/?#@:")]
    if op == "cli":
        out, err = io.StringIO(), io.StringIO()
        sys.stdout, sys.stderr = out, err
        try:
            code = install.cli(a[0])
        except SystemExit as exit:
            code = exit.code
        except Exception as error:
            text = type(error).__name__
            if str(error):
                text += ": " + str(error)
            err.write(text + "\n")
            code = 1
        finally:
            sys.stdout, sys.stderr = sys.__stdout__, sys.__stderr__
        return [code, out.getvalue(), err.getvalue()]
    raise AssertionError(op)

replies = []
for env, op in json.load(open(sys.argv[2], encoding="ascii")):
    for key, value in env.items():
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value
    try:
        replies.append(["ok", json.dumps(run(*op), sort_keys=True)])
    except Exception as error:
        replies.append([type(error).__name__, str(error)])
print(json.dumps(replies))
`

// oracleEpoch is 2026-10-14T00:00:00Z.
const oracleEpoch = 1791936000

// managedEnv lists the variables each operation sets or removes in both runs.
var managedEnv = []string{"HOME", "AGENT_DECISIONS_STATE", "AGENT_DECISIONS_SOURCE", "AGENT_DECISIONS_OFF",
	"TYPESAFE_API_KEY", "CLAUDE_CONFIG_DIR", "COLUMNS", "NO_COLOR", "FORCE_COLOR", "TERM", "PYTHON_COLORS"}

// oracleOp is one operation: its environment changes (nil removes a variable) and its arguments.
type oracleOp struct {
	env map[string]any
	op  []any
}

// compareWithPython runs ops through Python and Go and fails on any byte difference in the outcomes.
func compareWithPython(t *testing.T, ops []oracleOp) {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	request := make([]any, len(ops))
	for i, op := range ops {
		env := json.NewObject()
		for _, key := range managedEnv {
			env.Set(key, op.env[key])
		}
		request[i] = []any{env, op.op}
	}
	text, err := json.Dumps(request, json.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ops.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-I", "-c", oracleDriver, repoRoot, path, fmt.Sprint(oracleEpoch))
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("python oracle: %v", err)
	}
	parsed, err := json.Loads(string(output))
	if err != nil {
		t.Fatalf("python oracle reply: %v", err)
	}
	want := parsed.([]any)

	for _, key := range managedEnv {
		t.Setenv(key, "")
	}
	saved := now
	t.Cleanup(func() { now = saved })
	now = func() time.Time { return time.Unix(oracleEpoch, 0) }
	for i, op := range ops {
		for _, key := range managedEnv {
			if value, ok := op.env[key].(string); ok {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		}
		got := runOracleOp(t, op.op)
		if same, err := json.Equal(got, want[i]); err != nil || !same {
			t.Errorf("op %d %s %v\n%s", i, op.op[0], op.op[1:], firstDifference(render(got), render(want[i].([]any))))
		}
	}
}

// render returns an outcome as text: the error, or the JSON result with a cli result's streams expanded.
func render(outcome []any) string {
	if outcome[0] != "ok" {
		return fmt.Sprintf("%s: %s", outcome[0], outcome[1])
	}
	text := outcome[1].(string)
	if parsed, err := json.Loads(text); err == nil {
		if result, ok := parsed.([]any); ok && len(result) == 3 {
			if stdout, ok := result[1].(string); ok {
				return fmt.Sprintf("exit %v\n--- stdout\n%s--- stderr\n%s", result[0], stdout, result[2])
			}
		}
	}
	return strings.ReplaceAll(text, ", ", ",\n")
}

// firstDifference shows the first line where got and want differ, with the lines around it.
func firstDifference(got, want string) string {
	a, b := strings.Split(got, "\n"), strings.Split(want, "\n")
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	window := func(lines []string) string {
		return strings.Join(lines[max(0, i-3):min(len(lines), i+3)], "\n")
	}
	return fmt.Sprintf("line %d\n  go:\n%s\n  python:\n%s", i+1, window(a), window(b))
}

// runOracleOp runs one operation in Go with the outcome shape of oracleDriver.
func runOracleOp(t *testing.T, op []any) []any {
	t.Helper()
	result, err := oracleResult(op)
	if err != nil {
		msg := err.Error()
		return []any{pyerr.Class(err), msg}
	}
	text, err := json.Dumps(result, json.Options{SortKeys: true})
	if err != nil {
		t.Fatalf("dumps %s: %v", op[0], err)
	}
	return []any{"ok", text}
}

func oracleResult(op []any) (any, error) {
	a := op[1:]
	switch op[0] {
	case "codex_plan":
		return BuildCodexPlan(objects(a[0]), a[1].(string), a[2].(string), object(a[3]), text(a[4]), homeArg(a[5]),
			object(a[6]))
	case "codex_edits":
		return CodexConfigEdits(objects(a[0]), a[1].(string), a[2].(string), object(a[3]), homeArg(a[4]), object(a[5]))
	case "claude_plan":
		return BuildClaudePlan(a[0].(*json.Object), a[1].(string), a[2].(string), homeArg(a[3]), sourced(a[4]),
			sourced(a[5]), object(a[6]))
	case "merge":
		merged, previous, err := MergeClaudeSettings(a[0].(*json.Object), a[1].(string), a[2].(string), strings_(a[3]),
			object(a[4]), strings_(a[5]))
		if err != nil {
			return nil, err
		}
		var prior any
		if previous != nil {
			prior = *previous
		}
		return []any{merged, prior}, nil
	case "sanitize":
		return Sanitize(a[0])
	case "summary":
		return ParseCodexConfigSummary(a[0].(string)), nil
	case "nfkc":
		codes := make([]any, len(nfkcSeparators))
		for i, r := range nfkcSeparators {
			codes[i] = json.Int(fmt.Sprint(r))
		}
		return codes, nil
	case "cli":
		var stdout, stderr bytes.Buffer
		code := Main(strings_(a[0]), &stdout, &stderr)
		return []any{json.Int(fmt.Sprint(code)), stdout.String(), stderr.String()}, nil
	}
	panic(op[0])
}

func objects(v any) []*json.Object {
	var out []*json.Object
	for _, item := range v.([]any) {
		out = append(out, item.(*json.Object))
	}
	return out
}

func object(v any) *json.Object {
	if v == nil {
		return nil
	}
	return v.(*json.Object)
}

func text(v any) *string {
	if v == nil {
		return nil
	}
	s := v.(string)
	return &s
}

func homeArg(v any) string {
	if v == nil {
		return ""
	}
	return v.(string)
}

func strings_(v any) []string {
	var out []string
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}

func sourced(v any) []Sourced {
	if v == nil {
		return nil
	}
	var out []Sourced
	for _, item := range v.([]any) {
		pair := item.([]any)
		out = append(out, Sourced{pair[0].(string), pair[1].(*json.Object)})
	}
	return out
}

// oracleFixture builds a home with Claude and Codex configs, and returns the directory and its
// environment.
func oracleFixture(t *testing.T) (string, map[string]any) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	plugin := filepath.Join(home, ".claude/plugins/cache/market/tool/1.0.0")
	files := map[string]string{
		".claude.json":                                      `{"mcpServers": {"default": {"command": "/bin/echo", "args": ["d"]}}}`,
		".claude-primary/.claude.json":                      `{"mcpServers": {"primary": {"command": "/bin/echo", "env": {"TOKEN": "t"}}, "node_repl": {"command": "/native/node"}}}`,
		".claude-scratch/notes.txt":                         "",
		".claude/settings.json":                             `{"model": "keep", "env": {"CLAUDE_CODE_SHELL_PREFIX": "/old/prefix"}, "permissions": {"deny": ["Bash(rm *)"]}, "mcpServers": {"local": {"command": "/bin/echo", "args": ["--token", "s3cret"]}}}`,
		".claude/plugins/installed_plugins.json":            `{"plugins": {"tool@market": {"installPath": "` + plugin + `"}, "off@market": {"installPath": "/nowhere", "settings": {"enabledPlugins": false}}}}`,
		".claude/plugins/cache/market/tool/1.0.0/.mcp.json": `{"mcpServers": {"tool": {"command": "${CLAUDE_PLUGIN_ROOT}/bin/tool", "args": ["${PLUGIN_ROOT}"], "url": "https://u:p@example.invalid/x?token=t&ok=1"}}}`,
		".codex/config.toml":                                "default_permissions = \"trusted\" # c\n[permissions.trusted]\nextends = \":workspace\"\n[sandbox_workspace_write]\nnetwork_access = true\n",
		".codex/cli.config.toml":                            "model = \"m\"\n",
		"mcp.json":                                          `{"servers": [{"name": "local", "source": {"plugin": "p"}, "transport": {"type": "stdio", "command": "/bin/echo", "args": ["--api-key=k"], "env": {"SECRET": "super-secret-cli"}}}, {"name": "remote", "transport": {"type": "streamable_http", "url": "https://example.invalid/mcp?access_token=x"}}]}`,
		"list.json":                                         `[{"name": "a.b", "transport": {"type": "stdio", "command": "/bin/cat"}}]`,
		"bad.json":                                          `{"servers": [1]}`,
		"broken.json":                                       `{"servers": [`,
	}
	for name, content := range files {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"package/bin", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]any{"HOME": home, "AGENT_DECISIONS_STATE": filepath.Join(dir, "state"),
		"TYPESAFE_API_KEY": "test-key-0123456789abcdef", "AGENT_DECISIONS_SOURCE": repoRoot, "COLUMNS": "80"}
	return dir, env
}

func loads(t *testing.T, text string) any {
	t.Helper()
	value, err := json.Loads(text)
	if err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return value
}

// with returns env with changes applied; a nil value removes the variable.
func with(env map[string]any, changes ...any) map[string]any {
	out := map[string]any{}
	for key, value := range env {
		out[key] = value
	}
	for i := 0; i < len(changes); i += 2 {
		out[changes[i].(string)] = changes[i+1]
	}
	return out
}

func TestOraclePlans(t *testing.T) {
	dir, env := oracleFixture(t)
	home, state, root := filepath.Join(dir, "home"), filepath.Join(dir, "state"), filepath.Join(dir, "package")
	l := func(s string) any { return loads(t, strings.ReplaceAll(s, "{DIR}", dir)) }
	servers := l(`[
		{"name": "local", "enabled": true, "startup_timeout_sec": 3, "tool_timeout_sec": 4, "source": "/plugin/s.toml",
		 "auth_status": "x", "transport": {"type": "stdio", "command": "/bin/echo", "args": ["ok"], "cwd": "{DIR}",
		 "env": {"TOKEN": "secret"}, "env_vars": ["INHERITED"], "http_headers": null}},
		{"name": "remote", "transport": {"type": "streamable_http", "url": "https://example.invalid/mcp"}},
		{"name": "node_repl", "transport": {"type": "stdio", "command": "/native/node"}},
		{"name": "plugin.server:one", "startup_timeout_sec": null, "disabled_reason": null,
		 "transport": {"type": "stdio", "command": "/bin/echo", "args": [], "cwd": null, "env": null}},
		{"name": "flat", "type": "stdio", "command": "/bin/cat", "args": ["-u"], "url": null},
		{"name": "src", "source": {"manifest_path": "/m/.mcp.json", "other": null}, "transport": {"command": "/bin/cat"}},
		{"name": "plug", "source": {"plugin_id": "pid"}, "transport": {"command": "/bin/cat"}},
		{"name": "raw", "source": {"x": 1, "y": null}, "transport": {"command": "/bin/cat"}},
		{"name": "wrapped", "transport": {"type": "stdio", "command": "/old/bin/agent-decisions-isolate",
		 "args": ["--server", "wrapped", "--host", "codex"]}},
		{"name": "messages", "transport": {"type": "stdio", "command": "/old/agent-decisions-isolate",
		 "args": ["--server", "messages", "--host", "codex", "extra"]}}]`)
	previous := l(`{"servers": {"wrapped": {"command": "/original", "args": ["--ok"], "env": {"S": "v"}, "env_vars": [],
		"cwd": null, "source": "prior"}, "messages": {"command": "/native/messages"}},
		"previous_shell_prefix": "/prior/prefix", "session_review_exceptions": ["s1"]}`)
	summaries := []any{nil, l(`{}`), l(`{"default_permissions": ":workspace", "workspace_network": true}`),
		l(`{"default_permissions": "trusted-profile", "workspace_network": false}`),
		l(`{"default_permissions": "agent-decisions-isolation", "profile_extends": {"agent-decisions-isolation": "original"}}`),
		l(`{"default_permissions": "agent-decisions-isolation", "profile_extends": {}, "sandbox_mode": "read-only"}`),
		l(`{"default_permissions": "agent-decisions-isolation", "profile_extends": {"agent-decisions-isolation": "agent-decisions-isolation"}}`),
		l(`{"sandbox_mode": "danger-full-access", "profile_extends": []}`),
		l(`{"sandbox_mode": "workspace-write", "workspace_network": "yes"}`)}
	config := filepath.Join(home, ".codex/config.toml")
	var ops []oracleOp
	for _, summary := range summaries {
		ops = append(ops, oracleOp{env, []any{"codex_plan", servers, state, root, summary, config, home, previous}})
	}
	ops = append(ops,
		oracleOp{env, []any{"codex_plan", servers, state, root, nil, nil, nil, nil}},
		oracleOp{env, []any{"codex_plan", []any{}, "~/state", root, nil, nil, nil, nil}},
		oracleOp{env, []any{"codex_plan", l(`[{"transport": {"command": "/bin/cat"}}]`), state, root, nil, nil, nil, nil}},
		oracleOp{env, []any{"codex_plan", l(`[{"name": "x", "transport": {"type": "stdio"}}]`), state, root, nil, nil, nil, nil}},
		oracleOp{env, []any{"codex_plan", l(`[{"name": "x", "transport": {"type": "stdio", "command": "/c", "args": "a"}}]`), state, root, nil, nil, nil, nil}},
		oracleOp{env, []any{"codex_plan", servers, "", root, nil, nil, nil, nil}},
		oracleOp{with(env, "CLAUDE_CONFIG_DIR", filepath.Join(dir, "elsewhere")), []any{"codex_plan", []any{}, state, root, nil, nil, home, nil}},
		oracleOp{env, []any{"codex_edits", servers, state, root, nil, nil, nil}},
		oracleOp{env, []any{"codex_edits", servers, state, root, summaries[3], home, previous}},
	)
	claudeSettings := []any{l(`{}`), l(`{"model": "keep", "env": {"CLAUDE_CODE_SHELL_PREFIX": "/old/prefix", "V": "ok"},
		"permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(/already)", "Write(/{DIR}/home/.claude/projects/**)"]},
		"sandbox": {"filesystem": {"denyWrite": ["/already"]}},
		"mcpServers": {"local": {"command": "/bin/echo", "args": ["ok"], "env": {"SECRET": "v"}, "cwd": "{DIR}"},
			"node_repl": {"command": "/native/node"}, "http": {"url": "https://x.invalid"}, "bad": "text"}}`),
		l(`{"env": "text", "permissions": [], "sandbox": {"filesystem": null}}`),
		l(`{"env": {"CLAUDE_CODE_SHELL_PREFIX": "{DIR}/home/.local/share/agent-decisions/current/bin/agent-decisions-shell-prefix"}}`)}
	claudeConfigs := l(`[["/home/.claude.json", {"mcpServers": {"adrafinil": {"command": "/native/a"}, "computer-use": {"command": "/n/cu"}}}],
		["/home/.claude-primary/.claude.json", {"mcpServers": {"notes": {"command": "/bin/echo", "args": ["n"]}}}]]`)
	manifests := l(`[["/plugin/other/.mcp.json", {"mcpServers": {"messages": {"command": "/plugin/messages"},
		"computer-use": {"command": "/plugin/computer-use", "args": ["x"]}}}]]`)
	for _, settings := range claudeSettings {
		ops = append(ops,
			oracleOp{env, []any{"claude_plan", settings, state, root, home, claudeConfigs, manifests, previous}},
			oracleOp{env, []any{"claude_plan", settings, state, root, nil, nil, nil, nil}},
			oracleOp{env, []any{"merge", settings, "/p/bin/agent-decisions-shell-prefix", state,
				[]any{filepath.Join(dir, "t"), "~/x"}, previous, []any{filepath.Join(home, ".claude/projects")}}},
			oracleOp{env, []any{"merge", settings, "/p", "~/s", []any{}, nil, []any{}}},
		)
	}
	ops = append(ops, oracleOp{with(env, "CLAUDE_CONFIG_DIR", "~/selected"), []any{"claude_plan", claudeSettings[1], state, root, nil, claudeConfigs, nil, nil}})
	compareWithPython(t, ops)
}

func TestOracleSanitize(t *testing.T) {
	_, env := oracleFixture(t)
	corpus := []string{
		"plain", "", "--api-key", "--token=abc def", "--TOKEN  x", "--apı-key=x", "--APİKEY y", "--access_token=1 --secret 2",
		"OPENAI_API_KEY=value tool", "https://user:pw@example.invalid/p?access_token=q&ok=v&api-key=z#f",
		"https://example.invalid/?a=1&b=&&c", "see https://h.invalid/x?token=a+b%20c%zz%E2%82 now",
		"xhttps://h.invalid/?token=1 _https://h.invalid/?token=2 éhttps://h.invalid/?token=3",
		"https://[::1]:8/?token=1", "https://[v1.x]/?token=1", "https://[vz]/?token=1", "https://[1.2.3.4]/?token=1",
		"https://[::1/?token=1", "https://a]b/?token=1", "https://[x]y/?token=1", "https://[::1]x/?token=1",
		"https://u@[fe80::1%25en0]/?token=1", "https://h\u2100x/?token=1", "https://h\uff1a/?a=1",
		"https://h.invalid/?q=\u00e9&token=\U0001f600", "HTTP://H.INVALID/P?TOKEN=1", "https://h.invalid?password=x",
		"http://h/?a=%ED%A0%80", "https://user:@h/", "https://:pw@h/", "https://h/?=x&token", "https://h/;p?token=1",
		"'https://h/?token=1'", "<https://h/?token=1>", "https://[::ffff:1.2.3.4]/?t=1", "https://[1:2:3:4:5:6:7:8:9]/",
		"https://[1::2::3]/", "https://[::]/", "https://[1:2:3:4:5:6:7::]/", "https://[:1:2:3:4:5:6:7]/",
		"https://[12345::]/", "https://[1.2.3.04]/", "https://[::1.2.3.256]/", "https://[%en0]/",
	}
	values := []any{}
	for _, text := range corpus {
		values = append(values, text)
	}
	structured := loads(t, `{"servers": {"local": {"env": {"TOKEN": "super-secret"}, "bearer_token": "abc",
		"env_vars": ["TOKEN"], "args": ["--api-key", "v", "--password", "p", "--safe", "ok", 5, null],
		"headers": {"Authorization": "x"}, "http_headers": {"X": "y"}, "apiKey": "k", "my-private-key": "pk",
		"nested": [{"client_secret": 1}, ["--token", "t"]], "count": 3, "flag": true, "f": 1.5}}}`)
	ops := []oracleOp{{env, []any{"nfkc"}}, {env, []any{"sanitize", structured}}, {env, []any{"sanitize", values}}}
	for _, text := range corpus {
		ops = append(ops, oracleOp{env, []any{"sanitize", text}})
	}
	compareWithPython(t, ops)
}

func TestOracleCodexConfigSummary(t *testing.T) {
	_, env := oracleFixture(t)
	texts := []string{
		"", "default_permissions = \"profile#with-hash\" # trailing\nsandbox_mode = \"read-only\"\n[permissions.\"profile#with-hash\"]\nextends = \":danger-full-access\"\n[sandbox_workspace_write]\nnetwork_access = true # c\n",
		"default_permissions = 'single'\r\n[permissions.a]\r\nextends='b'\r\n", "  [ permissions . 'x.y' ]  \n extends = \"z\"\n",
		"[sandbox_workspace_write]\nnetwork_access = false\n", "[sandbox_workspace_write]\nnetwork_access = 1\n",
		"default_permissions = \"a\\\"b\"\n", "default_permissions = unquoted\n", "= x\n", "novalue\n",
		"[permissions.a.b]\nextends = \"c\"\n", "[permissions]\nextends = \"c\"\n", "[[array]]\nx = 1\n",
		"default_permissions = \"x\" \"y\"\n", "# comment only\n", "default_permissions = \"\u00e9\"\u0085x = 1\n",
		"sandbox_mode = \"workspace-write\"\nsandbox_mode = \"read-only\"\n", "[permissions.\"unterminated]\nextends = \"x\"\n",
	}
	var ops []oracleOp
	for _, text := range texts {
		ops = append(ops, oracleOp{env, []any{"summary", text}})
	}
	compareWithPython(t, ops)
}

func TestOracleCLI(t *testing.T) {
	dir, env := oracleFixture(t)
	home, state, root := filepath.Join(dir, "home"), filepath.Join(dir, "state"), filepath.Join(dir, "package")
	mcp := filepath.Join(home, "mcp.json")
	base := []string{"--state-root", state, "--install-root", root}
	plus := func(head []string, tail ...string) []string { return slices.Concat(head, tail) }
	codex := plus([]string{"--host", "codex", "--codex-mcp-json", mcp}, base...)
	claude := plus([]string{"--host", "claude"}, base...)
	cases := [][]string{
		codex, plus(codex, "--review-mode", "availability"),
		plus(codex, "--session-review-exception", "s1", "--session-review-exception", "s2", "--clear-session-review-exception", "s1"),
		plus(codex, "--session-review-exception", ""), plus(codex, "--session-review-exception", strings.Repeat("x", 129)),
		plus(codex, "--codex-config", filepath.Join(home, "missing.toml")),
		plus(codex, "--codex-config", filepath.Join(home, ".claude/settings.json")),
		{"--host", "codex", "--codex-mcp-json", filepath.Join(home, "list.json"), "--state-root", state, "--install-root", root},
		{"--host", "codex", "--codex-mcp-json", filepath.Join(home, "bad.json"), "--state-root", state, "--install-root", root},
		{"--host", "codex", "--codex-mcp-json", filepath.Join(home, "broken.json"), "--state-root", state, "--install-root", root},
		{"--host", "codex", "--codex-mcp-json", filepath.Join(home, "absent.json"), "--state-root", state, "--install-root", root},
		claude, plus(claude, "--claude-json", filepath.Join(home, ".claude.json")),
		plus(claude, "--claude-plugin-manifest", filepath.Join(home, ".claude/plugins/cache/market/tool/1.0.0/.mcp.json")),
		plus(claude, "--claude-settings", filepath.Join(home, "absent.json")),
		plus(claude, "--session-review-exception", "s1"),
		{"--host", "both", "--codex-mcp-json", mcp, "--state-root", state, "--install-root", root},
		{"--apply", "--state-root", filepath.Join(dir, "other")}, {"--apply", "--host", "codex", "--state-root", "~/x"},
		{"-h"}, {"--help"}, {"--he"}, {"--h"}, {"-hx"}, {"--host"}, {"--host", "cod"}, {"--host", "Codex"},
		{"--host=claud"}, {"--review-mode", "avail"}, {"--apply=yes"}, {"--ap", "--state-root", filepath.Join(dir, "other")}, {"-a", "--state-root", "~"}, {"extra", "args"},
		{"--host", "codex", "--", "x"}, {"--unknown"}, {"-x"}, {"--state-root"}, {"--host", "-1"}, {"--claude-json"},
		{"--session-review-exception"}, {"--codex-sock=x", "--nope=1", "-"}, {"--host", "codex", "--host", "both", "-h"},
		{"--clear"}, {"--claude"}, {"--codex-m=x", "--host", "x y"},
	}
	var ops []oracleOp
	for _, argv := range cases {
		ops = append(ops, oracleOp{env, []any{"cli", toAnyList(argv)}})
	}
	for _, columns := range []string{"200", "60", "40", "30", "10", "1", "0", "x", " 50 ", "1_00"} {
		ops = append(ops, oracleOp{with(env, "COLUMNS", columns), []any{"cli", toAnyList([]string{"-h"})}})
		ops = append(ops, oracleOp{with(env, "COLUMNS", columns), []any{"cli", toAnyList([]string{"--host", "x"})}})
	}
	for _, colors := range [][]any{{"FORCE_COLOR", "1"}, {"FORCE_COLOR", "1", "NO_COLOR", "1"}, {"FORCE_COLOR", "1", "TERM", "dumb"}} {
		for _, argv := range [][]string{{"-h"}, {"--host", "x"}, {"--bogus"}} {
			ops = append(ops, oracleOp{with(env, colors...), []any{"cli", toAnyList(argv)}})
		}
	}
	ops = append(ops, oracleOp{with(env, "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude-primary")), []any{"cli", toAnyList(claude)}})
	ops = append(ops, oracleOp{with(env, "AGENT_DECISIONS_STATE", "~/elsewhere"), []any{"cli", toAnyList([]string{"--host", "codex", "--codex-mcp-json", mcp})}})
	compareWithPython(t, ops)
}

func toAnyList(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
