package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMain lets a launcher test run this test binary as the real agent-decisions engine.
func TestMain(m *testing.M) {
	if os.Getenv("AGENT_DECISIONS_TEST_MAIN") == "1" {
		main()
	}
	os.Exit(m.Run())
}

const repo = "../.."

var events = []string{"PreToolUse", "SessionStart", "UserPromptSubmit"}

// registration is one hooks.json file and the --host its commands pass.
var registrations = []struct{ path, host string }{{"hooks/hooks.json", "claude"}, {"codex/hooks.json", "codex"}}

type hooksFile struct {
	Hooks map[string][]struct {
		Matcher *string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// commands returns the one launcher command of each event in a hooks.json file.
func commands(t *testing.T, path string) map[string]string {
	t.Helper()
	var file hooksFile
	readJSON(t, path, &file)
	out := map[string]string{}
	for event, groups := range file.Hooks {
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s %s: want one group with one hook", path, event)
		}
		if groups[0].Matcher != nil {
			t.Errorf("%s %s has a matcher; every tool, including future ones, must reach the hook", path, event)
		}
		out[event] = groups[0].Hooks[0].Command
	}
	return out
}

// launch runs a launcher command as the host does, with home as HOME and "{}" as the payload.
func launch(t *testing.T, command, home string) map[string]any {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = strings.NewReader("{}")
	cmd.Env = append(os.Environ(), "HOME="+home, "AGENT_DECISIONS_STATE="+filepath.Join(home, "state"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("launcher failed: %v\n%s", err, stderr.String())
	}
	response := map[string]any{}
	if len(stdout) > 0 && string(stdout) != "\n" {
		if err := json.Unmarshal(stdout, &response); err != nil {
			t.Fatalf("launcher stdout is not one JSON object: %q", stdout)
		}
	}
	return response
}

// engine installs an engine at HOME's active runtime and returns HOME.
func engine(t *testing.T, script string) string {
	t.Helper()
	home := t.TempDir()
	runtime := filepath.Join(home, ".local/share/agent-decisions/runtimes/fixture")
	if err := os.MkdirAll(filepath.Join(runtime, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "bin/agent-decisions"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("runtimes/fixture", filepath.Join(home, ".local/share/agent-decisions/current")); err != nil {
		t.Fatal(err)
	}
	return home
}

func reason(t *testing.T, event string, response map[string]any) string {
	t.Helper()
	if event == "PreToolUse" {
		output, _ := response["hookSpecificOutput"].(map[string]any)
		if output["permissionDecision"] != "deny" {
			t.Fatalf("PreToolUse response %v does not deny", response)
		}
		text, _ := output["permissionDecisionReason"].(string)
		return text
	}
	text, _ := response["systemMessage"].(string)
	return text
}

func TestRegistrationsCoverEveryEventAndHost(t *testing.T) {
	for _, r := range registrations {
		registered := commands(t, r.path)
		if len(registered) != len(events) {
			t.Errorf("%s registers %d events, want %v", r.path, len(registered), events)
		}
		for _, event := range events {
			if !strings.Contains(registered[event], "--host "+r.host) {
				t.Errorf("%s %s does not pass --host %s", r.path, event, r.host)
			}
		}
	}
}

func TestLauncherRunsInstalledEngine(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := engine(t, "#!/bin/sh\nAGENT_DECISIONS_TEST_MAIN=1 exec '"+self+"' \"$@\"\n")
	for _, r := range registrations {
		response := launch(t, commands(t, r.path)["SessionStart"], home)
		output, _ := response["hookSpecificOutput"].(map[string]any)
		if text, _ := output["additionalContext"].(string); !strings.Contains(text, "ts-decide") {
			t.Errorf("%s SessionStart: response %v has no ts-decide context", r.path, response)
		}
	}
}

func TestLauncherForwardsHealthyDenial(t *testing.T) {
	home := engine(t, "#!/bin/sh\nprintf '%s\\n' '{\"decision\":\"block\",\"reason\":\"healthy denial\"}'\n")
	for _, r := range registrations {
		for event, command := range commands(t, r.path) {
			if got := launch(t, command, home); got["reason"] != "healthy denial" {
				t.Errorf("%s %s: response %v, want the engine's denial", r.path, event, got)
			}
		}
	}
}

func TestLauncherBlocksWhenEngineFails(t *testing.T) {
	home := engine(t, "#!/bin/sh\necho 'invalid partial output'\nexit 1\n")
	for _, r := range registrations {
		for event, command := range commands(t, r.path) {
			if text := reason(t, event, launch(t, command, home)); !strings.Contains(text, "hook startup failed") {
				t.Errorf("%s %s: reason %q, want a startup failure", r.path, event, text)
			}
		}
	}
}

func TestLauncherBlocksWhenEngineIsMissing(t *testing.T) {
	broken := t.TempDir() // the pointer names a runtime that no longer exists
	namespace := filepath.Join(broken, ".local/share/agent-decisions")
	if err := os.MkdirAll(namespace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("runtimes/removed", filepath.Join(namespace, "current")); err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{t.TempDir(), broken} {
		for _, r := range registrations {
			for event, command := range commands(t, r.path) {
				if text := reason(t, event, launch(t, command, home)); !strings.Contains(text, "engine not installed") {
					t.Errorf("%s %s: reason %q, want engine not installed", r.path, event, text)
				}
			}
		}
	}
}

// TestLauncherRunsPythonRuntimeUntilReinstall covers a host that loads the new hooks.json while the active
// runtime is still a Python bundle from before the switch.
func TestLauncherRunsPythonRuntimeUntilReinstall(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on PATH")
	}
	home := t.TempDir()
	entry := filepath.Join(home, ".local/share/agent-decisions/current/hooks/hook.py")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "import json; print(json.dumps({'decision': 'block', 'reason': 'python runtime'}))\n"
	if err := os.WriteFile(entry, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range registrations {
		if got := launch(t, commands(t, r.path)["PreToolUse"], home); got["reason"] != "python runtime" {
			t.Errorf("%s: response %v, want the Python runtime's denial", r.path, got)
		}
	}
}

func TestManifestsAgree(t *testing.T) {
	var claude, codex struct{ Name, Version, Hooks string }
	readJSON(t, ".claude-plugin/plugin.json", &claude)
	readJSON(t, ".codex-plugin/plugin.json", &codex)
	var market struct{ Plugins []struct{ Name string } }
	readJSON(t, ".claude-plugin/marketplace.json", &market)
	if claude.Name != "agent-decisions" || codex.Name != "agent-decisions" || len(market.Plugins) == 0 ||
		market.Plugins[0].Name != "agent-decisions" {
		t.Errorf("plugin names: claude %q, codex %q, marketplace %v", claude.Name, codex.Name, market.Plugins)
	}
	if codex.Version != claude.Version {
		t.Errorf("codex version %q, claude version %q", codex.Version, claude.Version)
	}
	if codex.Hooks != "./codex/hooks.json" {
		t.Errorf("codex hooks %q, want ./codex/hooks.json", codex.Hooks)
	}
	skill, err := os.ReadFile(filepath.Join(repo, "skills/deciding/SKILL.md"))
	if err != nil || !strings.HasPrefix(string(skill), "---\nname: deciding\n") {
		t.Errorf("skills/deciding/SKILL.md has no deciding front matter (err %v)", err)
	}
}

// TestPinNamesEveryReleaseAsset checks the pin file that bin/agent-decisions-install reads. Whether the pins
// match the release build is checked by scripts/build-release && git diff --exit-code bin/agent-decisions.sha256.
func TestPinNamesEveryReleaseAsset(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repo, "bin/agent-decisions.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`^[0-9a-f]{64}  agent-decisions-(darwin-arm64|linux-amd64)$`)
	var assets []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		match := want.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("pin line %q does not match %v", line, want)
		}
		assets = append(assets, match[1])
	}
	if strings.Join(assets, ",") != "darwin-arm64,linux-amd64" {
		t.Errorf("pinned assets %v, want darwin-arm64 and linux-amd64", assets)
	}
}

// TestWrappersRunActiveRuntimeFromPluginRoot covers the isolate and shell-prefix launchers that host
// configs name in the plugin root, which holds no binary.
func TestWrappersRunActiveRuntimeFromPluginRoot(t *testing.T) {
	home := engine(t, "#!/bin/sh\nprintf '%s,' \"$@\"\n")
	for _, command := range []string{"isolate", "shell-prefix"} {
		cmd := exec.Command(filepath.Join(repo, "bin/agent-decisions-"+command), "a b", "c")
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if want := command + ",a b,c,"; err != nil || string(out) != want {
			t.Errorf("%s wrapper: %q, %v; want %q", command, out, err, want)
		}
	}
}

// TestPreviousLauncherRunsInstalledEngine covers a host session that loaded the hooks.json of 1.2.8 and still
// runs it after the installer activates a Go runtime: it runs hooks/hook.py from the active runtime.
func TestPreviousLauncherRunsInstalledEngine(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on PATH")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := engine(t, "#!/bin/sh\nAGENT_DECISIONS_TEST_MAIN=1 exec '"+self+"' \"$@\"\n")
	shim, err := os.ReadFile(filepath.Join(repo, "hooks/hook.py"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(home, ".local/share/agent-decisions/runtimes/fixture")
	if err := os.MkdirAll(filepath.Join(runtime, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "hooks/hook.py"), shim, 0o644); err != nil {
		t.Fatal(err)
	}
	const previous = `ad_runtime="$HOME/.local/share/agent-decisions/current"; ad_hook="$ad_runtime/hooks/hook.py"; ` +
		`python3 -I -B "$ad_hook" session_start --host claude`
	response := launch(t, previous, home)
	output, _ := response["hookSpecificOutput"].(map[string]any)
	if text, _ := output["additionalContext"].(string); !strings.Contains(text, "ts-decide") {
		t.Errorf("1.2.8 launcher: response %v has no ts-decide context", response)
	}
}
