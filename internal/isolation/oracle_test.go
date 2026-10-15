package isolation

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// oracleDriver runs a main function of the Python module as its bin wrapper does, with the arguments of
// mainCommand. It imports agent_decisions from a code root, so __file__ sits in a chosen install layout.
const oracleDriver = `
import json, os, sys
capture, platform, root, command, *args = sys.argv[1:]
os.environ.pop("__CF_USER_TEXT_ENCODING", None)  # Python startup adds it outside the explicit fixture environment.
sys.path.insert(0, root)
from agent_decisions import isolation
if platform != "-":
    sys.platform = platform
if capture != "-":
    def execvpe(file, argv, env):
        with open(capture, "w", encoding="ascii") as handle:
            json.dump({"file": file, "args": list(argv), "env": list(env.items()), "cwd": os.getcwd()}, handle)
    os.execvpe = execvpe
sys.argv = ["agent-decisions-" + command, *args]
raise SystemExit(isolation.main_isolate() if command == "isolate" else isolation.main_shell_prefix())
`

// oracleCase is one run of a command. Every string may hold {DIR}, the directory of the run, which holds
// home/, state/, tmp/, cwd/ (the working directory) and bin/.
type oracleCase struct {
	name     string
	command  string   // "isolate" or "shell-prefix"
	args     []string // the command's arguments
	env      []string // NAME=VALUE replaces that name in the base environment once, -NAME removes it, and any other entry is appended as it is
	platform string   // sys.platform; "" keeps the host's
	layout   string   // the install layout of the code: "bundle" (default), "repo" or "cache"
	rows     []string // ledger entries of the installation session, as JSON; nil is defaultRows
	stdin    string
	real     bool // exec for real instead of capturing the exec
	setup    func(t *testing.T, dir string)
	gap      string // a known foundation gap that makes Go differ; the case skips with it

}

func entry(spec string) string {
	return `{"session_id": "installation", "gate": "installation", "action": "configured", ` +
		`"time": "2026-10-11T00:00:00Z", "spec": ` + spec + `}`
}

const (
	codexSpec = `{"host": "codex", "state_root": "{DIR}/state", "servers": {
		"echo": {"command": "/bin/cat", "args": ["-u"], "env": {"VISIBLE": "ok", "PATH": "/opt/x"},
			"env_vars": ["INHERITED"], "cwd": "{DIR}/cwd", "protected_paths": ["~/recipe-only"]},
		"plain": {"command": "cat"},
		"relative": {"command": "./tool", "args": ["a b"], "cwd": "sub"},
		"home": {"command": "~/bin/tool", "env": null, "env_vars": null},
		"nocwd": {"command": "/bin/cat", "cwd": "{DIR}/missing"},
		"badcwd": {"command": "/bin/cat", "cwd": ""},
		"intcwd": {"command": "/bin/cat", "cwd": 5},
		"text": "not an object",
		"nocommand": {"args": []},
		"emptycommand": {"command": ""},
		"badargs": {"command": "/bin/cat", "args": "x"},
		"intargs": {"command": "/bin/cat", "args": [1]},
		"badenv": {"command": "/bin/cat", "env": {"A": 1}},
		"listenv": {"command": "/bin/cat", "env": ["A"]},
		"badvars": {"command": "/bin/cat", "env_vars": "A"},
		"intvars": {"command": "/bin/cat", "env_vars": [1]},
		"badprotected": {"command": "/bin/cat", "protected_paths": [1]},
		"nullprotected": {"command": "/bin/cat", "protected_paths": null},
		"emptykey": {"command": "/usr/bin/env", "env": {"": "x"}},
		"equalskey": {"command": "/usr/bin/env", "env": {"A=B": "x"}},
		"nulvalue": {"command": "/usr/bin/env", "env": {"A": "a\u0000b"}},
		"nularg": {"command": "/usr/bin/env", "args": ["a\u0000b"]},
		"nulcommand": {"command": "/usr/bin/e\u0000nv"},
		"escaped": {"command": "/usr/bin/env", "env": {"RAW": "\udc80\udcff"}},
		"surrogate": {"command": "/usr/bin/env", "env": {"BAD": "a\ud800\ud800b"}},
		"surrogatearg": {"command": "/usr/bin/env", "args": ["\udc00"]},
		"env": {"command": "/usr/bin/env"}},
		"claude_matchers": [], "previous_shell_prefix": null,
		"protected_paths": ["~/notes", "rel/x", "{DIR}/link", "/a\"b\\c"]}`
	claudeSpec = `{"host": "claude", "state_root": "~/state-root", "servers": {},
		"claude_matchers": [["/bin/echo", "hi"], ["git", "status"], ["~/bin/tool", "x"], ["./rel/tool"],
			["/usr/bin/env"]],
		"previous_shell_prefix": null}`
)

var defaultRows = []string{entry(codexSpec), entry(claudeSpec)}

// jsonU starts a JSON \u escape.
const jsonU = `\` + "u"

// claudeWith returns claudeSpec with previous_shell_prefix set to the JSON value previous.
func claudeWith(previous string) string {
	return strings.Replace(claudeSpec, `"previous_shell_prefix": null`, `"previous_shell_prefix": `+previous, 1)
}

func oracleCases() []oracleCase {
	isolate := func(name string, args ...string) oracleCase {
		return oracleCase{name: name, command: "isolate", args: args}
	}
	prefix := func(name string, args ...string) oracleCase {
		return oracleCase{name: name, command: "shell-prefix", args: args}
	}
	with := func(c oracleCase, change func(*oracleCase)) oracleCase {
		change(&c)
		return c
	}
	env := func(c oracleCase, entries ...string) oracleCase {
		return with(c, func(c *oracleCase) { c.env = append(c.env, entries...) })
	}
	rows := func(c oracleCase, entries ...string) oracleCase {
		return with(c, func(c *oracleCase) { c.rows = append([]string{}, entries...) })
	}
	realExec := func(c oracleCase) oracleCase {
		return with(c, func(c *oracleCase) { c.real = true })
	}
	onPlatform := func(c oracleCase, name string) oracleCase {
		return with(c, func(c *oracleCase) { c.platform = name })
	}
	execX := []string{"exec", "--host", "codex", "--command", "echo hi"}
	cases := []oracleCase{
		isolate("no arguments"),
		isolate("help", "-h"),
		isolate("long help", "--help"),
		isolate("abbreviated help", "--he"),
		isolate("exec help", "exec", "-h"),
		isolate("ambiguous exec option", "exec", "--h"),
		isolate("ambiguous main option", "--s", "x", "--h"),
		env(isolate("help at 60 columns", "-h"), "COLUMNS=60"),
		env(isolate("help at 20 columns", "-h"), "COLUMNS=20"),
		env(isolate("help at 1 column", "-h"), "COLUMNS=1"),
		env(isolate("exec help at 30 columns", "exec", "-h"), "COLUMNS=30"),
		env(isolate("exec help at 45 columns", "exec", "-h"), "COLUMNS=45"),
		env(isolate("help with spaced columns", "-h"), "COLUMNS= 70 "),
		env(isolate("help with underscored columns", "-h"), "COLUMNS=7_0"),
		env(isolate("help with bad columns", "-h"), "COLUMNS=abc"),
		env(isolate("help with zero columns", "-h"), "COLUMNS=0"),
		env(isolate("help with negative columns", "-h"), "COLUMNS=-5"),
		env(isolate("help at 300 columns", "-h"), "COLUMNS=300"),
		env(isolate("error at 30 columns", "exec"), "COLUMNS=30"),
		env(isolate("colored help", "-h"), "FORCE_COLOR=1"),
		env(isolate("colored exec help", "exec", "--help"), "FORCE_COLOR=1", "COLUMNS=40"),
		env(isolate("colored error", "exec", "--host", "clade"), "FORCE_COLOR=1"),
		env(isolate("colored usage error", "--server", "x"), "FORCE_COLOR=yes", "COLUMNS=40"),
		env(isolate("no color wins", "-h"), "FORCE_COLOR=1", "NO_COLOR=1"),
		env(isolate("dumb terminal", "-h"), "FORCE_COLOR=1", "TERM=dumb"),
		env(isolate("python colors ignored", "-h"), "PYTHON_COLORS=1"),
		isolate("exec without options", "exec"),
		isolate("exec without command", "exec", "--host", "codex"),
		isolate("exec with bad host", "exec", "--host", "x", "--command", "y"),
		isolate("misspelled exec", "exce"),
		isolate("misspelled host", "--host", "clade"),
		isolate("server without value", "--server"),
		isolate("server without host", "--server", "echo"),
		isolate("host without server", "--host", "codex"),
		isolate("empty server", "--server", "", "--host", "codex"),
		isolate("server", "--server", "echo", "--host", "codex"),
		isolate("server with equals", "--server=echo", "--host=codex"),
		isolate("server abbreviated", "--ser", "echo", "--ho", "codex"),
		isolate("server with extra positional", "--server", "echo", "--host", "codex", "extra"),
		isolate("exec", execX...),
		isolate("exec read-only", "exec", "--host", "codex", "--read-only", "--command", "ls -la ~"),
		isolate("exec abbreviated", "exec", "--ho", "codex", "--com=x", "--read"),
		isolate("exec empty command", "exec", "--host", "codex", "--command", ""),
		isolate("exec claude", "exec", "--host", "claude", "--command", "x"),
		isolate("exec unknown option", "exec", "--host", "codex", "--command", "x", "--bogus"),
		isolate("main unknown option", "--bogus", "exec", "--host", "codex", "--command", "x"),
		isolate("both unknown", "--a", "exec", "--host", "codex", "--command", "x", "--b", "c"),
		isolate("store_true with value", "exec", "--host", "codex", "--command", "x", "--read-only=1"),
		isolate("short flag cluster", "-hx"),
		isolate("short flag with value", "-h=1"),
		isolate("short flag dash", "-h-"),
		isolate("double dash", "--", "exec", "--host", "codex", "--command", "x"),
		isolate("double dash in exec", "exec", "--", "--host", "codex"),
		isolate("negative number", "-1"),
		isolate("negative fraction", "-.5"),
		isolate("dash with space", "-x y"),
		isolate("lone dash", "-"),
		isolate("undecodable choice", "\xff"),
		isolate("undecodable option", "--\xff"),
		isolate("unicode choice", "exéc"),
		isolate("subcommand host wins", "--host", "codex", "--server", "echo", "exec", "--host", "claude",
			"--command", "x"),
		isolate("exec twice", "exec", "--host", "codex", "--command", "x", "exec"),
		isolate("repeated options", "exec", "--host", "claude", "--host", "codex", "--command", "a", "--command", "b"),
		isolate("option value looks like option", "exec", "--host", "codex", "--command", "--read-only"),
		isolate("option value negative", "exec", "--host", "codex", "--command", "-1"),
	}
	for _, server := range []string{"plain", "relative", "home", "nocwd", "badcwd", "intcwd", "text", "nocommand",
		"emptycommand", "badargs", "intargs", "badenv", "listenv", "badvars", "intvars", "badprotected",
		"nullprotected", "missing", "env"} {
		cases = append(cases, isolate("server "+server, "--server", server, "--host", "codex"))
	}
	for _, server := range []string{"emptykey", "equalskey", "nulvalue", "nularg", "nulcommand", "escaped",
		"surrogate", "surrogatearg", "env"} {
		cases = append(cases, realExec(isolate("real server "+server, "--server", server, "--host", "codex")))
	}
	for _, spec := range []struct{ name, row string }{
		{"no rows", ""},
		{"other gate", strings.Replace(entry(codexSpec), `"gate": "installation"`, `"gate": "other"`, 1)},
		{"other action", strings.Replace(entry(codexSpec), `"configured"`, `"removed"`, 1)},
		{"spec not object", entry(`["codex"]`)},
		{"empty state root", entry(`{"host": "codex", "state_root": ""}`)},
		{"state root number", entry(`{"host": "codex", "state_root": 1}`)},
		{"servers list", entry(`{"host": "codex", "state_root": "/s", "servers": []}`)},
		{"servers null", entry(`{"host": "codex", "state_root": "/s", "servers": null}`)},
		{"bad matchers", entry(`{"host": "codex", "state_root": "/s", "claude_matchers": [[]]}`)},
		{"matchers not list", entry(`{"host": "codex", "state_root": "/s", "claude_matchers": {}}`)},
		{"empty previous", entry(`{"host": "codex", "state_root": "/s", "previous_shell_prefix": ""}`)},
		{"number previous", entry(`{"host": "codex", "state_root": "/s", "previous_shell_prefix": 0}`)},
		{"bad protected", entry(`{"host": "codex", "state_root": "/s", "protected_paths": "x"}`)},
		{"null protected", entry(`{"host": "codex", "state_root": "/s", "protected_paths": null}`)},
		{"empty protected", entry(`{"host": "codex", "state_root": "/s", "protected_paths": [""]}`)},
		{"tilde state root", entry(`{"host": "codex", "state_root": "~"}`)},
		{"relative state root", entry(`{"host": "codex", "state_root": "rel/state"}`)},
		{"quoted state root", entry(`{"host": "codex", "state_root": "/q\"u\\o te"}`)},
		{"unknown user state root", entry(`{"host": "codex", "state_root": "~nosuchuser9/x"}`)},
	} {
		var entries []string
		if spec.row != "" {
			entries = []string{spec.row}
		}
		cases = append(cases, rows(isolate("spec "+spec.name, execX...), entries...))
	}
	for _, c := range []struct{ name, spec string }{
		{"spec surrogate state root", `{"host": "codex", "state_root": "/x/` + jsonU + `dcff/` + jsonU + `d800/y"}`},
		{"spec surrogate protected path", `{"host": "codex", "state_root": "/s", "protected_paths": ["{DIR}/a` +
			jsonU + `d800"]}`},
	} {
		cases = append(cases, rows(isolate(c.name, execX...), entry(c.spec)))
	}
	codexOnly := entry(`{"host": "codex", "state_root": "{DIR}/state"}`)
	cases = append(cases,
		rows(isolate("newest entry wins", execX...), entry(codexSpec), entry(`{"host": "codex", "state_root": "/newer"}`)),
		rows(isolate("newest invalid entry fails", execX...), entry(codexSpec), entry(`{"host": "codex", "state_root": ""}`)),
		rows(isolate("other host skipped", execX...), entry(codexSpec), entry(claudeSpec)),
		env(isolate("unset home", execX...), "-HOME"),
		env(isolate("empty home", execX...), "HOME="),
		env(isolate("relative home", execX...), "HOME=relhome"),
		env(isolate("home with slash", execX...), "HOME={DIR}/home/"),
		with(isolate("repo layout", execX...), func(c *oracleCase) { c.layout = "repo" }),
		with(isolate("cache layout", execX...), func(c *oracleCase) { c.layout = "cache" }),
		onPlatform(isolate("linux without bwrap", execX...), "linux"),
		onPlatform(env(isolate("linux with missing path", execX...), "PATH={DIR}/bin:/usr/bin:/bin"), "linux"),
		with(onPlatform(env(rows(isolate("linux", "exec", "--host", "codex", "--read-only", "--command", "x"),
			codexOnly), "PATH={DIR}/bin:/usr/bin:/bin"), "linux2"), func(c *oracleCase) { c.setup = linuxHome }),
		with(onPlatform(env(rows(isolate("linux without path", execX...), codexOnly), "-PATH"), "linux"),
			func(c *oracleCase) { c.setup = linuxHome }),
		onPlatform(isolate("windows", execX...), "win32"),
		env(isolate("environment edges", execX...), "JUNK", "DUP=1", "DUP=2", "=weird", "PATH=/second"),
		env(isolate("lang C", execX...), "LANG=C"),
		env(isolate("lang UTF-8", execX...), "LANG=en_US.UTF-8"),
		env(isolate("lang Latin-1", execX...), "LANG=en_US.ISO8859-1"),
		env(isolate("lang unknown", execX...), "LANG=xx_YY.UTF-8"),
		env(isolate("lc_all C", execX...), "LC_ALL=C"),
		env(isolate("lc_ctype POSIX", execX...), "LC_CTYPE=POSIX", "LANG=en_US.UTF-8"),
		env(isolate("empty lc_ctype", execX...), "LANG=fr_FR.UTF-8", "LC_CTYPE="),
		env(isolate("lc_ctype with slash", execX...), "LC_CTYPE=../en_US.UTF-8"),
		env(isolate("lang with dot", execX...), "LANG=."),
		env(isolate("state is a file", execX...), "AGENT_DECISIONS_STATE={DIR}/home/.claude.json"),

		prefix("prefix without arguments"),
		prefix("prefix with two arguments", "a", "b"),
		rows(prefix("prefix without spec", "ls -la"), entry(codexSpec)),
		prefix("prefix exact match", "/bin/echo hi"),
		prefix("prefix spaced match", "/bin/echo   'hi'"),
		prefix("prefix bare name is not a path", "echo hi"),
		prefix("prefix name match", "git status"),
		prefix("prefix tilde match", "~/bin/tool x"),
		prefix("prefix relative match", "./rel/tool"),
		prefix("prefix unbalanced quote", "'unterminated"),
		prefix("prefix empty", ""),
		prefix("prefix blank", "   "),
		prefix("prefix empty word", "'' x"),
		rows(prefix("prefix previous passthrough", "ls"), entry(claudeWith(`"{DIR}/bin/prior"`))),
		rows(prefix("prefix previous match", "git status"), entry(claudeWith(`"{DIR}/bin/prior"`))),
		rows(prefix("prefix bare previous", "ls"), entry(claudeWith(`"prior"`))),
		rows(prefix("prefix invalid spec", "git status"), entry(claudeWith(`""`))),
		rows(prefix("prefix bad matcher", "git status"),
			entry(`{"host": "claude", "state_root": "/s", "claude_matchers": [["git", ""]]}`)),
		rows(prefix("prefix matcher list missing", "git status"), entry(`{"host": "claude", "state_root": "/s"}`)),
		onPlatform(prefix("prefix linux without bwrap", "git status"), "linux"),
		env(prefix("prefix state is a file", "git status"), "AGENT_DECISIONS_STATE={DIR}/home/.claude.json"),
		realExec(rows(prefix("real prefix shell", "echo hi"), entry(codexSpec))),
		realExec(env(rows(prefix("real prefix env", "/usr/bin/env"), entry(claudeWith(`"/usr/bin/env"`))),
			"JUNK", "DUP=1", "DUP=2", "LANG=C")),
		realExec(rows(prefix("real prefix passthrough env", "env"), entry(claudeWith(`"/usr/bin/env"`)))),
		realExec(rows(prefix("real prefix signals", "x"), entry(claudeWith(`"{DIR}/bin/signals"`)))),
		realExec(rows(prefix("real prefix missing program", "x"), entry(claudeWith(`"nonexistent-prefix"`)))),
		realExec(env(rows(prefix("real prefix denied program", "x"), entry(claudeWith(`"tool"`))),
			"PATH={DIR}/nope:{DIR}/home:{DIR}/bin")),
	)

	// Generated argument lists for the parser.
	tokens := []string{"exec", "--host", "claude", "codex", "--server", "echo", "--command", "true", "--read-only",
		"-h", "--ho", "--h", "--s", "--host=codex", "--server=", "-x", "--", "-1", "a b", "-hx", "exe", "clade",
		"--read", "--comm", "-", "--command=", "--he", "\xff"}
	random := rand.New(rand.NewPCG(2026, 10))
	for i := range 120 {
		args := make([]string, random.IntN(7))
		for j := range args {
			args[j] = tokens[random.IntN(len(tokens))]
		}
		cases = append(cases, isolate(fmt.Sprintf("generated %d", i), args...))
	}
	return cases
}

// linuxHome creates every path that a Linux sandbox must protect, and drops the dangling profile link.
func linuxHome(t *testing.T, dir string) {
	if err := os.Remove(filepath.Join(dir, "home/.codex/dangling.config.toml")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(dir, "home/.claude/projects"))
	for _, file := range []string{".claude/settings.json", ".claude/settings.local.json", ".codex/config.toml"} {
		writeFile(t, filepath.Join(dir, "home", file), "", 0o644)
	}
}

// buildCase creates the run directory of c in dir and returns its environment.
func buildCase(t *testing.T, dir string, c oracleCase) []string {
	t.Helper()
	expand := func(s string) string { return strings.ReplaceAll(s, "{DIR}", dir) }
	for _, sub := range []string{"home/.codex/sessions", "home/targets", "home/bin", "tmp", "cwd/sub", "cwd/rel",
		"bin", "nope"} {
		mkdir(t, filepath.Join(dir, sub))
	}
	writeFile(t, filepath.Join(dir, "home/.codex/b.config.toml"), "b", 0o644)
	writeFile(t, filepath.Join(dir, "home/.codex/.config.toml"), "hidden", 0o644)
	writeFile(t, filepath.Join(dir, "home/.codex/other.toml"), "other", 0o644)
	writeFile(t, filepath.Join(dir, "home/.codex/B.CONFIG.TOML"), "upper", 0o644)
	writeFile(t, filepath.Join(dir, "home/targets/a.toml"), "a", 0o644)
	writeFile(t, filepath.Join(dir, "home/.claude.json"), "{}", 0o644)
	writeFile(t, filepath.Join(dir, "home/tool"), "", 0o644) // found on PATH but not executable
	writeFile(t, filepath.Join(dir, "bin/bwrap"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(dir, "bin/prior"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(dir, "bin/signals"),
		"#!/usr/bin/perl\nprint \"PIPE=$SIG{PIPE} XFSZ=$SIG{XFSZ} ARGS=@ARGV\\n\";\n", 0o755)
	for link, target := range map[string]string{
		"home/.codex/a.config.toml":        filepath.Join(dir, "home/targets/a.toml"),
		"home/.codex/dangling.config.toml": filepath.Join(dir, "home/targets/none"),
		"link":                             filepath.Join(dir, "home/targets"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	if c.setup != nil {
		c.setup(t, dir)
	}

	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	entries := c.rows
	if entries == nil {
		entries = defaultRows
	}
	for _, row := range entries {
		if err := ledger.Append(loads(t, expand(row)).(*json.Object)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + dir + "/home",
		"AGENT_DECISIONS_STATE=" + dir + "/state", "TMPDIR=" + dir + "/tmp", "INHERITED=yes"}
	replaced := map[string]bool{}
	var extra []string
	for _, raw := range c.env {
		item := expand(raw)
		if removed, ok := strings.CutPrefix(item, "-"); ok {
			env = withoutName(env, removed)
			continue
		}
		name, _, found := strings.Cut(item, "=")
		position := slices.IndexFunc(env, func(base string) bool { return strings.HasPrefix(base, name+"=") })
		if found && name != "" && !replaced[name] && position >= 0 {
			env[position] = item
			replaced[name] = true
		} else {
			extra = append(extra, item)
		}
	}
	return append(env, extra...)
}

func withoutName(env []string, name string) []string {
	var kept []string
	for _, item := range env {
		if !strings.HasPrefix(item, name+"=") {
			kept = append(kept, item)
		}
	}
	return kept
}

var denyName = regexp.MustCompile(`agent-decisions-deny-[a-z0-9_]{8}`)

// normalize removes what differs between the two runs of a case: their directories and random names.
func normalize(text, dir string) string {
	return denyName.ReplaceAllString(strings.ReplaceAll(text, dir, "{DIR}"), "agent-decisions-deny-*")
}

// snapshot lists the files under dir with their modes and contents. The state directory has a random key,
// so its files show only their sizes.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a directory that the run made unreadable
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, path)
		line := rel + " " + info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			line += " -> " + target
		case info.Mode().IsRegular() && (rel == "state" || strings.HasPrefix(rel, "state/")):
			line += fmt.Sprintf(" %d bytes", info.Size())
		case info.Mode().IsRegular():
			data, _ := os.ReadFile(path)
			line += " " + string(data)
		}
		lines = append(lines, normalize(line, dir))
		return nil
	})
	return strings.Join(lines, "\n")
}

// oracleRun is the observable outcome of one run.
type oracleRun struct {
	result
	capture, files string
}

// layouts installs a copy of the Python package in each install layout under base and returns the code
// root of each. The bundle root is reached through a symlink, which the code root must resolve.
func layouts(t *testing.T, base string) map[string]string {
	t.Helper()
	roots := map[string]string{
		"repo":   filepath.Join(base, "repo"),
		"cache":  filepath.Join(base, "plugins/cache/market/agent-decisions/1.2.3"),
		"bundle": filepath.Join(base, "bundle"),
	}
	repoPackage := filepath.Join(oracletest.Source(t), "agent_decisions")
	for name, root := range roots {
		target := root
		if name == "bundle" {
			target = filepath.Join(base, "bundle-real")
			if err := os.Symlink(target, root); err != nil {
				t.Fatal(err)
			}
		}
		mkdir(t, target)
		if output, err := exec.Command("/bin/cp", "-R", repoPackage, target+"/").CombinedOutput(); err != nil {
			t.Fatalf("cp: %v: %s", err, output)
		}
	}
	mkdir(t, filepath.Join(roots["repo"], ".git"))
	return roots
}

func runOracleCase(t *testing.T, c oracleCase, argv0 []string, root string) oracleRun {
	t.Helper()
	dir := tempDir(t)
	env := buildCase(t, dir, c)
	capture := "-"
	if !c.real {
		capture = filepath.Join(t.TempDir(), "capture.json")
	}
	platformName := c.platform
	if platformName == "" {
		platformName = "-"
	}
	argv := append(append([]string{}, argv0...), capture, platformName, root, c.command)
	for _, arg := range c.args {
		argv = append(argv, strings.ReplaceAll(arg, "{DIR}", dir))
	}
	run := oracleRun{result: spawn(t, argv, env, filepath.Join(dir, "cwd"), c.stdin)}
	run.stdout, run.stderr = normalize(run.stdout, dir), normalize(run.stderr, dir)
	if capture != "-" {
		if data, err := os.ReadFile(capture); err == nil {
			run.capture = normalize(string(data), dir)
		}
	}
	run.files = snapshot(t, dir)
	return run
}

// lastLine returns the last line of a Python traceback, which names the exception. The frames before it
// show Python source positions, which the Go command has no equivalent of.
func lastLine(stderr string) string {
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	return lines[len(lines)-1] + "\n"
}

// TestMainsMatchPython runs both commands through the Python module and through this package with the
// same arguments, environment, signed ledger and files, and compares their output, exit status, the exec
// they ask for (argv, environment and working directory) and the files they leave. Only retired argparse
// presentation is normalized.
func TestMainsMatchPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	roots := layouts(t, tempDir(t))
	for _, c := range oracleCases() {
		t.Run(c.name, func(t *testing.T) {
			if c.gap != "" {
				t.Skip(c.gap)
			}
			layout := c.layout
			if layout == "" {
				layout = "bundle"
			}
			root := roots[layout]
			want := runOracleCase(t, c, []string{python, "-I", "-B", "-c", oracleDriver}, root)
			got := runOracleCase(t, c, []string{self, mainCommand}, root+"/agent_decisions/isolation.py")
			if !c.real && want.code == got.code && (want.code == 0 || want.code == 2) {
				want.stdout, got.stdout = oracletest.CLIPresentation(want.stdout), oracletest.CLIPresentation(got.stdout)
				want.stderr, got.stderr = oracletest.CLIPresentation(want.stderr), oracletest.CLIPresentation(got.stderr)
			}
			if want.code == 1 && strings.HasPrefix(want.stderr, "Traceback (most recent call last):\n") {
				want.stderr = lastLine(want.stderr)
			}
			t.Logf("Python exit %d\nstdout: %q\nstderr: %q\nexec: %s", want.code, want.stdout, want.stderr, want.capture)
			if got.code != want.code || got.stdout != want.stdout || got.stderr != want.stderr {
				t.Errorf("args %q\nGo exit %d\nstdout:\n%s\nstderr:\n%s\nPython exit %d\nstdout:\n%s\nstderr:\n%s",
					c.args, got.code, got.stdout, got.stderr, want.code, want.stdout, want.stderr)
			}
			if got.capture != want.capture {
				t.Errorf("args %q exec\nGo:     %s\nPython: %s", c.args, got.capture, want.capture)
			}
			if got.files != want.files {
				t.Errorf("args %q files\nGo:\n%s\nPython:\n%s", c.args, got.files, want.files)
			}
		})
	}
}
