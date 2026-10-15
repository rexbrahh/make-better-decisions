package evidence

import (
	"bufio"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// oracleScript answers one JSON request per stdin line with one json.dumps line on stdout: ["ok", result]
// or [exception class, str(exception)]. Text arrives as hex of its UTF-8 bytes with surrogatepass, so lone
// surrogates and NUL bytes survive; a JSON value arrives as hex of its json.dumps text. Calls that take a
// frame run at that depth of the Python stack, counting <module> as 1.
const oracleScript = `
import functools, json, sys
sys.path.insert(0, sys.argv[1])
from agent_decisions import evidence
from agent_decisions.transcript import ToolCall, Transcript

def text(wire):
    return bytes.fromhex(wire).decode("utf-8", "surrogatepass")

def texts(wires):
    return tuple(text(wire) for wire in wires)

def value(wire):
    return json.loads(text(wire))

def outcome(call):
    try:
        return ["ok", call()]
    except Exception as error:
        return [type(error).__name__, str(error)]

def at_frame(frame, call):
    depth, f = 0, sys._getframe()
    while f:
        depth, f = depth + 1, f.f_back
    return call() if depth == frame - 1 else at_frame(frame, call)

def make_transcript(users, calls, turn, brief):
    tool_calls = tuple(ToolCall(c[0], c[1], text(c[2]), text(c[3]), None if c[4] is None else value(c[4]))
                       for c in calls)
    return Transcript(texts(users), tool_calls, tool_calls[len(tool_calls) - turn:] if turn else (), "",
                      None if brief is None else text(brief))

def trim(bundle, contracts, query, reserved):
    evidence._trim_to_budget(bundle, contracts=contracts, query=query, reserved_chars=reserved)
    return bundle

for line in sys.stdin:
    op, *a = json.loads(line)
    if op == "build":
        t = make_transcript(a[0], a[1], a[2], a[3])
        call = functools.partial(evidence.build, t, texts(a[4]), query=text(a[5]),
                                 cwd=None if a[6] is None else text(a[6]), contracts=texts(a[7]))
        reply = outcome(lambda: at_frame(a[8], call))
    elif op == "redact":
        reply = outcome(lambda: evidence.redact(text(a[0]), texts(a[1])))
    elif op == "scrub":
        reply = outcome(lambda: at_frame(a[2], functools.partial(evidence.scrub, value(a[0]), texts(a[1]))))
    elif op == "clip":
        reply = outcome(lambda: evidence.clip(text(a[0]), a[1]))
    elif op == "clip_source":
        reply = outcome(lambda: evidence._clip_source(text(a[0]), text(a[1]), a[2]))
    elif op == "paths":
        reply = outcome(lambda: at_frame(a[1], functools.partial(list, evidence._path_candidates(texts(a[0])))))
    elif op == "shorten":
        reply = outcome(lambda: evidence.shorten_observations(value(a[0]), text(a[1]), texts(a[2])))
    elif op == "trim":
        reply = outcome(lambda: trim(value(a[0]), texts(a[1]), text(a[2]), a[3]))
    elif op == "contracts":
        reply = outcome(lambda: evidence.contract_observations(texts(a[0]), text(a[1]), texts(a[2])))
    elif op == "history":
        reply = outcome(lambda: evidence.architecture_history(value(a[0]), value(a[1])))
    print(json.dumps(reply), flush=True)
`

// oracleCase is one request to the Python oracle and the Go call that must answer it with the same bytes.
type oracleCase struct {
	name    string
	request []any
	run     func() (any, error)
}

// compareWithPython runs every case through one CPython process named by AGENT_DECISIONS_PYTHON, with
// this process's environment, and compares each reply with json.dumps of the Go outcome. It skips the test
// when the variable is unset.
func compareWithPython(t *testing.T, cases []oracleCase) {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot := oracletest.Source(t)
	var input strings.Builder
	for _, c := range cases {
		line, err := stdjson.Marshal(c.request)
		mustDo(t, err)
		input.Write(line)
		input.WriteByte('\n')
	}
	command := exec.Command(python, "-I", "-c", oracleScript, repoRoot)
	command.Stdin = strings.NewReader(input.String())
	command.Stderr = os.Stderr
	output, err := command.Output()
	mustDo(t, err)
	lines := bufio.NewScanner(strings.NewReader(string(output)))
	lines.Buffer(nil, 1<<30)
	for i := 0; lines.Scan(); i++ {
		if i >= len(cases) {
			t.Fatalf("python answered more than %d requests", len(cases))
		}
		value, err := cases[i].run()
		got := dumps(t, outcome(value, err))
		if want := lines.Text(); got != want {
			t.Errorf("%s:\n  go     %s\n  python %s", cases[i].name, around(got, want), around(want, got))
		}
	}
}

// around returns s near the first byte where it differs from other.
func around(s, other string) string {
	i := 0
	for i < len(s) && i < len(other) && s[i] == other[i] {
		i++
	}
	return fmt.Sprintf("[%d] %q", i, s[max(0, i-60):min(len(s), i+60)])
}

func outcome(value any, err error) []any {
	if err == nil {
		return []any{"ok", value}
	}
	if typed, ok := err.(interface{ PyType() string }); ok {
		return []any{typed.PyType(), err.Error()}
	}
	return []any{fmt.Sprintf("Go %T", err), err.Error()}
}

func wire(s string) string {
	return hex.EncodeToString([]byte(s))
}

func wires(items []string) []any {
	values := []any{}
	for _, item := range items {
		values = append(values, wire(item))
	}
	return values
}

// buildInput is the Transcript and keyword arguments of one build call.
type buildInput struct {
	users     []string
	calls     []transcript.ToolCall
	turn      int // the last turn calls are the current turn's
	brief     *string
	secrets   []string
	query     string
	cwd       *string
	contracts []string
	frame     int
}

func buildCase(t *testing.T, name string, in buildInput) oracleCase {
	if in.frame == 0 {
		in.frame = hookFrame
	}
	calls := []any{}
	for _, c := range in.calls {
		var execution any
		if c.Execution != nil {
			execution = wire(dumps(t, c.Execution))
		}
		calls = append(calls, []any{c.ID, c.Name, wire(c.Input), wire(c.Output), execution})
	}
	var brief, cwd any
	if in.brief != nil {
		brief = wire(*in.brief)
	}
	if in.cwd != nil {
		cwd = wire(*in.cwd)
	}
	return oracleCase{
		name: name,
		request: []any{"build", wires(in.users), calls, in.turn, brief, wires(in.secrets), wire(in.query), cwd,
			wires(in.contracts), in.frame},
		run: func() (any, error) {
			tx := transcript.Transcript{UserMessages: in.users, ToolCalls: in.calls, Brief: in.brief}
			if in.turn > 0 {
				tx.TurnCalls = in.calls[len(in.calls)-in.turn:]
			}
			return Build(tx, in.secrets, Options{Query: in.query, Cwd: in.cwd, Contracts: in.contracts, Frame: in.frame})
		},
	}
}

func loads(t *testing.T, text string) any {
	t.Helper()
	value, err := json.Loads(text)
	mustDo(t, err)
	return value
}

func nested(depth int, leaf string) string {
	return strings.Repeat("[", depth) + leaf + strings.Repeat("]", depth)
}

// units are the escape widths that json.dumps gives a str: plain, a two-character escape, a BMP and an
// astral code point, a lone surrogate (WTF-8), and NUL.
var units = map[string]string{
	"ascii": "x", "quote": "\"", "bmp": "\u03bb", "astral": "\U0001F600", "surrogate": "\xed\xa0\x80", "nul": "\x00",
}

// edge returns the smallest n in [0, limit] for which drops(n) holds, or -1 when none does.
func edge(limit int, drops func(n int) bool) int {
	n := sort.Search(limit+1, drops)
	if n > limit {
		return -1
	}
	return n
}

// TestBudgetEdgesMatchPython builds around the length at which a user message, a tool call or a brief
// stops fitting policy.EvidenceChars, for each escape width, and clips around policy.ClipChars.
func TestBudgetEdgesMatchPython(t *testing.T) {
	stateDir(t)
	full := strings.Repeat("x", policy.ClipChars)
	var cases []oracleCase
	for name, unit := range units {
		for _, n := range []int{policy.ClipChars - 1, policy.ClipChars, policy.ClipChars + 1, policy.ClipChars + 2} {
			text := strings.Repeat(unit, n)
			for _, limit := range []int{policy.ClipChars, 0, 1, 3} {
				cases = append(cases, oracleCase{fmt.Sprintf("clip %s %d/%d", name, n, limit),
					[]any{"clip", wire(text), limit}, func() (any, error) { return Clip(text, limit), nil }})
			}
		}

		users := func(n int) []string {
			return []string{full, full, strings.Repeat(unit, n), full, full}
		}
		included := func(n int) int {
			bundle, err := Build(tr(users(n), nil, nil), nil, Options{Frame: hookFrame})
			mustDo(t, err)
			return len(strs(bundle, "user_messages"))
		}
		userEdge := edge(policy.ClipChars, func(n int) bool { return included(n) < included(0) })

		base := []transcript.ToolCall{call("a", "Bash", "a", full), call("b", "Bash", "b", full),
			call("c", "Bash", "c", full)}
		calls := func(n int) []transcript.ToolCall {
			return append(append([]transcript.ToolCall{call("edge", "Read", "edge.py", strings.Repeat(unit, n))},
				base...), call("d", "Bash", "d", full))
		}
		retained := func(n int) int {
			bundle, err := Build(tr([]string{"go"}, calls(n), nil), nil, Options{Frame: hookFrame})
			mustDo(t, err)
			return len(objects(bundle, "tool_calls_newest_first"))
		}
		callEdge := edge(policy.ClipChars, func(n int) bool { return retained(n) < retained(0) })

		briefFits := func(n int) bool {
			brief := strings.Repeat(unit, n)
			bundle, err := Build(tr(users(policy.ClipChars), nil, &brief), nil, Options{Frame: hookFrame})
			mustDo(t, err)
			_, ok := bundle.Get("brief")
			return !ok
		}
		briefEdge := edge(policy.ClipChars, briefFits)

		t.Logf("%s: user edge %d, call edge %d, brief edge %d", name, userEdge, callEdge, briefEdge)
		for _, at := range []int{userEdge, callEdge, briefEdge, policy.ClipChars} {
			if at < 0 {
				continue
			}
			for n := max(0, at-2); n <= min(policy.ClipChars+2, at+2); n++ {
				brief := strings.Repeat(unit, n)
				cases = append(cases,
					buildCase(t, fmt.Sprintf("users %s %d", name, n), buildInput{users: users(n)}),
					buildCase(t, fmt.Sprintf("calls %s %d", name, n), buildInput{users: []string{"go"}, calls: calls(n), turn: 2}),
					buildCase(t, fmt.Sprintf("brief %s %d", name, n), buildInput{users: users(policy.ClipChars), brief: &brief}))
			}
		}
	}
	compareWithPython(t, cases)
}

// TestRecursionLimitsMatchPython nests values around the depth at which scrub, build and
// _path_candidates raise RecursionError.
func TestRecursionLimitsMatchPython(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "a", "b.py"), "ok = True\n")
	var cases []oracleCase
	for depth := 985; depth <= 994; depth++ {
		for _, leaf := range []string{`"x"`, `"TOKEN=abcdefgh1234"`, `5`, `{"k": "v"}`} {
			text := nested(depth, leaf)
			cases = append(cases, oracleCase{fmt.Sprintf("scrub %d %s", depth, leaf),
				[]any{"scrub", wire(text), []any{}, hookFrame + 2},
				func() (any, error) { return Scrub(loads(t, text), nil, hookFrame+2) }})
			execution := json.NewObject("nested", loads(t, text))
			cases = append(cases, buildCase(t, fmt.Sprintf("build execution %d %s", depth, leaf), buildInput{
				users: []string{"go"}, calls: []transcript.ToolCall{{ID: "e", Name: "Bash", Input: "ls", Output: "ok",
					Execution: execution}}}))
		}
		query := nested(depth, `"a/b.py"`)
		cases = append(cases,
			oracleCase{fmt.Sprintf("paths %d", depth), []any{"paths", wires([]string{query}), hookFrame + 1},
				func() (any, error) {
					found := []any{}
					err := pathCandidates([]string{query}, hookFrame+1, func(raw string) bool {
						found = append(found, raw)
						return true
					})
					return found, err
				}},
			buildCase(t, fmt.Sprintf("build query %d", depth), buildInput{users: []string{"go"}, query: query}),
			buildCase(t, fmt.Sprintf("build query cwd %d", depth), buildInput{users: []string{"go"}, query: query, cwd: &dir}),
			buildCase(t, fmt.Sprintf("build call input cwd %d", depth), buildInput{users: []string{"go"}, cwd: &dir,
				calls: []transcript.ToolCall{call("c", "Read", query, "x")}, turn: 1}))
	}
	compareWithPython(t, cases)
}

// TestTextHandlingMatchesPython covers the hand-ported regexes: Unicode \b, IGNORECASE folds, the
// tempered private-key body, the path-token lookahead and the line-suffix $.
func TestTextHandlingMatchesPython(t *testing.T) {
	stateDir(t)
	key := strings.Repeat("A", 20)
	redactions := []string{
		"Bearer " + key, "xBearer " + key, "éBearer " + key, "_Bearer " + key, "BEARER\u2003" + key, "bearer\u0131" + key,
		"Bearer " + strings.Repeat("\u0130", 16), "sk-" + key, "ask-" + key, "λsk-" + key, "\u00b2sk-" + key,
		"AKIAABCDEFGHIJKLMNOP", "AKIAABCDEFGHIJKLMNOPQ", "AKIAABCDEFGHIJKLMNOPé", "AKIAABCDEFGHIJKLMNOP-",
		"eyJ" + key + "." + key + "." + key, "ghp_" + key + key, "xoxb-" + key,
		"API_KEY=abcdefgh1", "AP\u0130_KEY=abcdefgh1", "AP\u0131_KEY: 'abcdefgh1'", "TO\u212aEN=abcdefgh1",
		"\u017fecret = \"abcdefgh12\"", "my_password:abcdefgh", "xPASSWD=abcdefgh1", "éTOKEN=abcdefgh1",
		"token= abcdefg", "TOKEN=abc\u2003defghij", "TOKEN=" + strings.Repeat("\u03bb", 8),
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----a-----BEGIN PRIVATE KEY-----b-----END PRIVATE KEY-----",
		"-----BEGIN X PRIVATE KEY-----a-----END Y PRIVATE KEY-----b-----END Z PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----a-----END bad-----END PRIVATE KEY-----BEGIN ",
		"-----BEGIN PRIVATE KEY-----never ends", "x\xed\xa0\x80sk-" + key + "\x00",
	}
	var cases []oracleCase
	for _, text := range redactions {
		for _, secrets := range [][]string{nil, {"abcdefgh12345678"}, {"short"}} {
			cases = append(cases, oracleCase{"redact " + text, []any{"redact", wire(text + " abcdefgh12345678"), wires(secrets)},
				func() (any, error) { return Redact(text+" abcdefgh12345678", secrets), nil }})
		}
	}
	pathTexts := []string{
		"see src/a.py:12:3, and ./b/c.go:7.", "(src/x.go) [y.ts] {z.md} <w.rs>", "\"q.json\"'r.yaml'`s.sh`",
		"a/b.py\n", "x.py:12\n)", "https://host/a.py file:///etc/x.py", ". .. ./ ../", ".env .env.local x/.env.prod",
		"cat 'dir with spaces/a.py' \"other dir/b.md\"", "a\u2003b/c.py\u3000d.txt", "a/b/c?d.py!", "x.PY Y.Py",
		`{"file_path": "/abs/one.py", "nested": ["two/three.md", {"k": "four.txt"}]}`, `["a/b.py:1"]`,
		"“quoted.py” ‘single.md’", "tail.py...", "trail/slash/", "@scope/pkg+x/y-z.js", "x.py\x00y.py",
		"bad 'quote a.py", "λ/μ.py ünïcode/path.py", strings.Repeat("d/", 600) + "x.py",
	}
	for _, text := range pathTexts {
		cases = append(cases, oracleCase{"paths " + text, []any{"paths", wires([]string{text}), hookFrame + 1},
			func() (any, error) {
				found := []any{}
				err := pathCandidates([]string{text}, hookFrame+1, func(raw string) bool {
					found = append(found, raw)
					return true
				})
				return found, err
			}})
	}
	long := strings.Repeat("x", 9_000) + " Needle \u212aelvin \u017fign i\u0130 " + strings.Repeat("y", 9_000)
	for _, query := range []string{"needle", "kelvin", "sign", "ii", "nothing here", "x", "needle kelvin sign ii abc"} {
		for _, limit := range []int{policy.ClipChars, 1_000, 128, 3} {
			cases = append(cases, oracleCase{fmt.Sprintf("clip_source %q %d", query, limit),
				[]any{"clip_source", wire(long), wire(query), limit},
				func() (any, error) { return clipSource(long, query, limit), nil }})
		}
	}
	compareWithPython(t, cases)
}

// TestBundleOperationsMatchPython compares build with sources, shorten_observations, _trim_to_budget,
// contract_observations and architecture_history on the same inputs.
func TestBundleOperationsMatchPython(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Preserve the rules.\n"+strings.Repeat("a", 20_000))
	writeFile(t, filepath.Join(dir, "docs", "contract.md"), "Contract \u03bb\n"+strings.Repeat("c", 30_000)+"Needle\n")
	writeFile(t, filepath.Join(dir, "src", "main.py"), "TOKEN=abcdefgh1234\n"+strings.Repeat("m", 5_000))
	writeFile(t, filepath.Join(dir, ".env"), "SECRET=1\n")
	writeFile(t, filepath.Join(dir, "keys", "k.txt"), "key\n")
	writeFile(t, filepath.Join(dir, "bin.dat"), "a\x00b")
	writeFile(t, filepath.Join(dir, "bad.txt"), "\xff\xfe\xed\xa0\x80ok")
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeFile(t, outside, "outside\n")
	mustDo(t, os.Symlink(outside, filepath.Join(dir, "link.md")))
	users := []string{"Preserve the contract. " + strings.Repeat("u", 20_000), "Check src/main.py and bin.dat bad.txt",
		"Read docs/contract.md Needle"}
	calls := []transcript.ToolCall{call("1", "Read", `{"file_path": "docs/contract.md"}`, strings.Repeat("o", 20_000)),
		call("2", "Bash", "cat src/main.py", strings.Repeat("p", 20_000)), call("3", "Bash", "ls", "AGENTS.md link.md")}
	contracts := []string{"docs/contract.md", "missing.md", "./docs/contract.md"}
	in := buildInput{users: users, calls: calls, turn: 1, secrets: []string{"abcdefgh1234"},
		query: "Needle in docs/contract.md and .env and keys/k.txt", cwd: &dir, contracts: contracts}
	cases := []oracleCase{buildCase(t, "build with sources", in)}
	bundle, err := buildCase(t, "", in).run()
	mustDo(t, err)
	text := dumps(t, bundle)

	history := []any{}
	for i := range 40 {
		history = append(history, json.NewObject("id", fmt.Sprint(i), "note", strings.Repeat("h", 2_000)))
	}
	over := loads(t, text).(*json.Object)
	over.Set("architecture_history", history)
	overText := dumps(t, over)
	for _, reserved := range []int{0, 256, 40_000, 79_000} {
		cases = append(cases, oracleCase{fmt.Sprintf("trim %d", reserved),
			[]any{"trim", wire(overText), wires(contracts), wire("Needle"), reserved},
			func() (any, error) {
				b := loads(t, overText).(*json.Object)
				return b, TrimToBudget(b, contracts, "Needle", reserved)
			}})
	}
	for _, query := range []string{"Needle", ""} {
		for _, protect := range [][]string{nil, contracts} {
			cases = append(cases, oracleCase{fmt.Sprintf("shorten %q %v", query, protect),
				[]any{"shorten", wire(text), wire(query), wires(protect)},
				func() (any, error) { return ShortenObservations(loads(t, text).(*json.Object), query, protect) }})
		}
	}
	paths := []string{"docs/contract.md", "docs/contract.md", "missing.md", ".env", "keys/k.txt", "link.md",
		"bin.dat", "bad.txt", "src/main.py", "AGENTS.md", "../outside.md", "docs/./contract.md", "x\x00.md"}
	for _, cwd := range []string{dir, dir + "/docs/..", filepath.Join(dir, "missing")} {
		cases = append(cases, oracleCase{"contracts " + cwd, []any{"contracts", wires(paths), wire(cwd), []any{}},
			func() (any, error) { return ContractObservations(paths, cwd, nil) }})
	}
	records := `[{"id": "r1", "contracts": {"a.md": "h1", "b.md": "h2", "c.md": "h3", "d.md": "h4"}},
		{"id": "r2"}, {"id": "r3", "contracts": {}, "trust": "forged"}]`
	current := `{"a.md": {"sha256": "h1"}, "b.md": {"sha256": null}, "c.md": {"sha256": "other"}, "x": {}}`
	cases = append(cases, oracleCase{"history", []any{"history", wire(records), wire(current)},
		func() (any, error) {
			var list []*json.Object
			for _, record := range loads(t, records).([]any) {
				list = append(list, record.(*json.Object))
			}
			return ArchitectureHistory(list, loads(t, current).(*json.Object))
		}})
	compareWithPython(t, cases)
}

// TestGitBaselinesMatchPython builds with sources under each git outcome: a repository, a plain
// directory, no git on PATH, a git that outlives the 1 s timeout, and a fake git with odd output.
func TestGitBaselinesMatchPython(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_PYTHON") == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	fake := t.TempDir()
	writeFile(t, filepath.Join(fake, "slow", "git"), "#!/bin/sh\nexec /bin/sleep 5\n")
	writeFile(t, filepath.Join(fake, "odd", "git"), "#!/bin/sh\ncase \"$3\" in\n"+
		"rev-parse) printf '%b' \"$FAKE_REV\"; exit \"${FAKE_EXIT:-0}\" ;;\n"+
		"cat-file) if [ \"$4\" = -s ]; then printf '%b' \"$FAKE_SIZE\"; else printf abcdefghij; fi ;;\nesac\n")
	for _, name := range []string{"slow", "odd"} {
		mustDo(t, os.Chmod(filepath.Join(fake, name, "git"), 0o755))
	}
	mustDo(t, os.MkdirAll(filepath.Join(fake, "empty"), 0o755))
	revision := strings.Repeat("0123456789abcdef", 3)[:40]
	scenarios := []struct {
		name, path, rev, size, exit string
		repo                        bool
	}{
		{name: "repository", repo: true},
		{name: "plain directory"},
		{name: "no git", path: filepath.Join(fake, "empty")},
		{name: "slow git", path: filepath.Join(fake, "slow")},
		{name: "odd size", path: filepath.Join(fake, "odd"), rev: " " + revision + " \\n", size: " +1_0\\n"},
		{name: "bad size", path: filepath.Join(fake, "odd"), rev: revision, size: "1__0"},
		{name: "negative size", path: filepath.Join(fake, "odd"), rev: revision, size: "-5"},
		{name: "large size", path: filepath.Join(fake, "odd"), rev: revision, size: "1000001"},
		{name: "non-ASCII revision", path: filepath.Join(fake, "odd"), rev: revision + "\\0377", size: "1"},
		{name: "upper-case revision", path: filepath.Join(fake, "odd"), rev: strings.ToUpper(revision), size: "1"},
		{name: "failed rev-parse", path: filepath.Join(fake, "odd"), rev: revision, size: "1", exit: "1"},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			dir := stateDir(t)
			writeFile(t, filepath.Join(dir, "contract.md"), "Retain leases.\n")
			writeFile(t, filepath.Join(dir, "new.md"), "New file.\n")
			if s.repo {
				commitFixture(t, dir, "contract.md")
				writeFile(t, filepath.Join(dir, "contract.md"), "Changed.\n")
			}
			if s.path != "" {
				t.Setenv("PATH", s.path)
			}
			t.Setenv("FAKE_REV", s.rev)
			t.Setenv("FAKE_SIZE", s.size)
			t.Setenv("FAKE_EXIT", s.exit)
			compareWithPython(t, []oracleCase{buildCase(t, s.name, buildInput{users: []string{"Inspect."},
				query: "contract.md new.md", cwd: &dir})})
		})
	}
}
