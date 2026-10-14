package gates

import (
	"bufio"
	"bytes"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// oracleScript answers one JSON request per stdin line with one json.dumps line on stdout: ["ok", result]
// or [exception class, str(exception)]. Text arrives as hex of its UTF-8 bytes with surrogatepass; a JSON
// value arrives as hex of its json.dumps text. Calls that take a frame run at that depth of the Python
// stack, counting <module> as 1; functools.partial adds no frame.
//
// A "gates" request runs a sequence of gate calls against one context, the way hook.py does: each record
// becomes the next context's ledger entry, and a decide verdict also becomes a completed tool call. Each
// step answers its outcome, the request bodies Jev received (sorted, since chunks ask in parallel) and the
// entry it added.
const oracleScript = `
import dataclasses, json, os, sys, threading, time, uuid
from functools import partial
from unittest import mock
sys.path.insert(0, sys.argv[1])
from agent_decisions import evidence, gates, jev, ledger
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev

def text(wire):
    return bytes.fromhex(wire).decode("utf-8", "surrogatepass")

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

def shown(result):
    if isinstance(result, gates.Outcome):
        return [result.action, result.message, result.record, result.note]
    return result

def context(spec):
    calls = tuple(ToolCall(*c) for c in spec["calls"])
    t = Transcript(tuple(spec["user_messages"]), calls, calls[len(calls) - spec["turn"]:] if spec["turn"] else (),
                   spec["last"], spec["brief"])
    bundle = evidence.build(t)
    bundle.update(spec["evidence"])
    return gates.Context(spec["host"], "s1", spec["agent_id"], "toolu_0", t, bundle, tuple(spec["entries"]),
                         spec["repo_id"], tuple(spec["inherited"]))

def step(ctx, n, kind, args, answers, frame):
    raises = None if answers["raises"] is None else jev.JevError(*answers["raises"])
    raw = None if answers["raw"] is None else answers["raw"].encode()
    fake = FakeJev(raises=raises, raw=raw, **answers["answers"])
    bodies = []
    def post(body, key, timeout):
        bodies.append(body.hex())
        return fake(body, key, timeout)
    ask = lambda state, questions: jev.ask(state, questions, key="k", post=post)
    ctx = dataclasses.replace(ctx, tool_use_id=f"toolu_{n}")
    if kind == "decide":
        call = partial(gates.decide, args[0], ctx, ask)
    elif kind == "edit":
        call = partial(gates.edit_gate, args[0], args[1], ctx, ask)
    elif kind == "question":
        call = partial(gates.question_gate, args[0], ctx, ask)
    elif kind == "plan":
        call = partial(gates.plan_gate, ctx, ask if args[0] else None, plan=args[1])
    elif kind == "stop":
        call = partial(gates.stop_gate, args[0], args[1], ctx, ask)
    else:
        call = partial(gates.consulted, ctx)
    result = outcome(lambda: at_frame(frame, call))
    while threading.active_count() > 1:  # chunks that the gate stopped waiting for still ask
        time.sleep(0.001)
    entry = None
    if result[0] == "ok" and isinstance(result[1], gates.Outcome) and result[1].record is not None:
        record = result[1].record
        if kind == "decide":
            record = {**record, "verdict": result[1].message}
        entry = {"host": ctx.host, "session_id": "s1", "agent_id": ctx.agent_id, "tool_use_id": ctx.tool_use_id,
                 "turn": ctx.transcript.turn_key, "action": result[1].action, **record}
        t = ctx.transcript
        if kind == "decide":
            done = ToolCall(ctx.tool_use_id, "Bash", '{"command": "ts-decide \'{}\'"}', result[1].message + "\n")
            t = dataclasses.replace(t, tool_calls=(*t.tool_calls, done), turn_calls=(*t.turn_calls, done))
        ctx = dataclasses.replace(ctx, transcript=t, entries=(*ctx.entries, entry))
    return ctx, [[result[0], shown(result[1])], sorted(bodies), entry]

def run_gates(spec, steps, state, availability, uuids, frame):
    os.environ["AGENT_DECISIONS_STATE"] = state
    if availability:
        ledger.append({"session_id": "installation", "gate": "installation", "action": "configured",
                       "spec": {"host": spec["host"], "review_mode": "availability"}})
    ids = iter(uuids)
    replies = []
    with mock.patch.object(gates.uuid, "uuid4", lambda: uuid.UUID(bytes=bytes.fromhex(next(ids)), version=4)):
        ctx = context(spec)
        for n, (kind, args, answers) in enumerate(steps, 1):
            ctx, reply = step(ctx, n, kind, args, answers, frame)
            replies.append(reply)
    return replies

for line in sys.stdin:
    op, *a = json.loads(line)
    if op == "classify":
        reply = outcome(lambda: gates.classify(text(a[0])))
    elif op == "command_of":
        reply = outcome(lambda: gates.command_of(value(a[0])))
    elif op == "interactive":
        reply = outcome(lambda: gates._interactive(text(a[0])))
    elif op == "guard":
        cwd = None if a[1] is None else text(a[1])
        reply = outcome(lambda: shown(at_frame(a[2], partial(gates.guard, value(a[0]), cwd))))
    elif op == "gates":
        reply = ["ok", run_gates(value(a[0]), value(a[1]), text(a[2]), a[3], a[4], a[5])]
    print(json.dumps(reply), flush=True)
`

type oracleCase struct {
	name    string
	request []any
	run     func() any // the reply that json.dumps must render as Python's line
}

// compareWithPython runs every case through one CPython process named by AGENT_DECISIONS_PYTHON and
// compares each reply with json.dumps of the Go reply. Python answers every case before Go runs any.
func compareWithPython(t *testing.T, cases []oracleCase) {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var input strings.Builder
	for _, c := range cases {
		line, err := stdjson.Marshal(c.request)
		if err != nil {
			t.Fatal(err)
		}
		input.Write(line)
		input.WriteByte('\n')
	}
	command := exec.Command(python, "-I", "-c", oracleScript, repoRoot)
	command.Stderr = os.Stderr
	command.Stdin = strings.NewReader(input.String())
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(bytes.NewReader(output))
	lines.Buffer(nil, 1<<30)
	i, failures := 0, 0
	for ; lines.Scan(); i++ {
		if i >= len(cases) {
			t.Fatalf("python answered more than %d requests", len(cases))
		}
		got := dumps(t, cases[i].run())
		if want := lines.Text(); got != want {
			if failures++; failures <= 20 {
				t.Errorf("%s:\n  go     %s\n  python %s", cases[i].name, around(got, want), around(want, got))
			}
		}
	}
	if i != len(cases) {
		t.Fatalf("python answered %d of %d requests", i, len(cases))
	}
	if failures > 0 {
		t.Errorf("%d of %d cases differ", failures, len(cases))
	}
}

// around returns s near the first byte where it differs from other.
func around(s, other string) string {
	i := 0
	for i < len(s) && i < len(other) && s[i] == other[i] {
		i++
	}
	return fmt.Sprintf("[%d] %q", i, s[max(0, i-80):min(len(s), i+80)])
}

func result(value any, err error) []any {
	if err == nil {
		return []any{"ok", value}
	}
	if typed, ok := err.(interface{ PyType() string }); ok {
		return []any{typed.PyType(), err.Error()}
	}
	return []any{fmt.Sprintf("Go %T", err), err.Error()}
}

func wire(s string) string { return hex.EncodeToString([]byte(s)) }

func shown(outcome *Outcome) any {
	if outcome == nil {
		return nil
	}
	var record any
	if outcome.Record != nil {
		record = outcome.Record
	}
	return []any{outcome.Action, outcome.Message, record, outcome.Note}
}

// --- shell commands

// commandCorpus combines command heads, arguments, suffixes and prefixes into shell commands with quotes,
// heredocs, pipes, redirects, wrappers, environment assignments and interactive tools.
func commandCorpus() []string {
	heads := []string{"ts-decide", "/x/bin/ts-decide", "./ts-decide", "env ts-decide", "env -i A=1 ts-decide",
		"env -u A ts-decide", "env -C /tmp ts-decide", "env -- ts-decide", "env -S 'ts-decide'",
		"env -S'bash -'", "command ts-decide", "command -p ts-decide", "command -v ts-decide", "exec ts-decide",
		"exec -a x ts-decide", "time ts-decide", "time -p ts-decide", "xargs ts-decide", "xargs -I x ts-decide",
		"nohup ts-decide", "sudo ts-decide", "FLAG=1 ts-decide", "A=1 B=2 env ts-decide", "bash", "bash -s",
		"bash -i", "sh -c 'ts-decide'", "python3", "python3 -", "python3 -u -", "python3 -mcode", "python3 -m code",
		"node", "node -i", "node -", "ruby", "perl -", "cat", "echo ts-decide", "grep ts-decide", "pytest -q",
		"npm test", "\"ts-decide\"", "'ts-decide'", "ts\\-decide", "ts-decid\\\ne", "$(which ts-decide)"}
	args := []string{"", " '{}'", ` "{\"a\": 1}"`, " <<'EOF'\n{}\nEOF\n", " <<EOF\n{}\nEOF", " <<-'X'\n\t{}\n\tX\n",
		" <<\"EOF\"\r\n{}\r\nEOF\r\n", " - <<'PY'\nprint('ts-decide')\nPY\n", " < in.py", " '<<'", " -c 'exec(input())'",
		" \\\n'{}'", " '{}' 'unterminated", " -- '{}'", " <<<'{}'", " <<'EOF'\n{}\n", " '{\"decision\": \"café\"}'"}
	suffixes := []string{"", " && ls", "; ls", " | tee out", " > out", "\nls", " # note", " &", " 2>&1"}
	prefixes := []string{"", "cd /x && ", "# c\n", "(", "  ", "echo a\n", "python3 - <<'PY'\nx = 'ts-decide'\nPY\n",
		"! ", "{ "}
	var corpus []string
	for _, head := range heads {
		for _, arg := range args {
			for _, suffix := range suffixes {
				for _, prefix := range prefixes {
					corpus = append(corpus, prefix+head+arg+suffix)
				}
			}
		}
	}
	return append(corpus, "", " ", "\n", "#", "'", "\"", "\\", "<<", "<<'EOF'", "ts-decide <<'", "ts-decide <<''\n\n",
		"cat <<'LOG'\n(ts-decide) is the subject\nLOG\n", "echo 'literal\nts-decide'", "ts-decide '{}'\r\n",
		"env -S", "env -S ''", "command", "exec", "xargs", "time", "env -P /usr/bin ts-decide '{}'",
		"env --split-string='bash -' <<'SH'\nts-decide '{}'\nSH\n", "ts-décide '{}'", "ts-decide\u00a0'{}'",
		"ts-decide '\x00'", "ts-decide '\xed\xbf\xbf'", strings.Repeat("(", 200)+"ts-decide", strings.Repeat("env ", 300)+"ts-decide")
}

// py: agent_decisions/gates.py classify, command_of, _interactive
func TestClassificationMatchesPython(t *testing.T) {
	setUp(t)
	var cases []oracleCase
	for i, command := range commandCorpus() {
		name := "#" + strconv.Itoa(i) + " " + strconv.Quote(command)
		cases = append(cases,
			oracleCase{"classify " + name, []any{"classify", wire(command)}, func() any {
				kind, request := Classify(command)
				return []any{"ok", []any{kind, request}}
			}},
			oracleCase{"interactive " + name, []any{"interactive", wire(command)}, func() any {
				return result(interactive(command))
			}})
	}
	for _, input := range []string{`{"command": "ls"}`, `{"cmd": "ls -l"}`, `{"command": ["bash", "-lc", "ts-decide '{}'"]}`,
		`{"command": ["bash", "-c", "x"]}`, `{"command": ["sh", "-lc", "x", "y"]}`, `{"command": ["ls", "-l", "a b"]}`,
		`{"command": ["it's"]}`, `{"command": []}`, `{"command": 5}`, `{"command": null, "cmd": "x"}`,
		`{"cmd": ["bash", "-lc", "x"]}`, `{"input": "patch"}`, `{"command": ["bash", "-lc", 5]}`, `{"command": [1, 2]}`,
		`{"command": "", "cmd": "x"}`, `{"command": ["", "-c", ""]}`, `{"command": {"a": 1}}`} {
		cases = append(cases, oracleCase{"command_of " + input, []any{"command_of", wire(input)}, func() any {
			return []any{"ok", CommandOf(obj(t, input))}
		}})
	}
	compareWithPython(t, cases)
}

// guardCorpus returns tool inputs that name paths in and around the private state directory.
func guardCorpus(t *testing.T) []*json.Object {
	state, home := stateDir(t), os.Getenv("HOME")
	words := []string{state, state + "/sessions/s.jsonl", state[:len(state)-3] + "*/x", filepath.Dir(state) + "/*",
		filepath.Dir(state) + "/**", filepath.Dir(state) + "/[a-z]tate/x", filepath.Dir(state) + "/{state,x}/k",
		"~/.local/state/agent-decisions/ledger.key", "~/.local/state/agent-d*/k", "~/.local/state/*/k", "~/.local/**/k",
		"~/.local/sta?e/agent-decisions", "~/.local/state/agent-{decisions,x}/k", "~/.local/state/agent-{a,b}{c,d}/k",
		"$HOME/.local/state/agent-decisions/k", "${HOME}/.local/state/agent-decisions/k", "$HOME_DIR/x", "${HOME}x",
		"~", "~/", "~/*", "~/.local/*", "~/.local/state", "~nosuchuser/x", "~5%", "~root/.local", home + "/../x",
		".local/state/agent-decisions/k", "../state/x", "state", "./state/../state/k", "/tmp/[z-a]", "/tmp/[!x]",
		"/", "*", "**", "*/*/*", "{a,b}" + strings.Repeat("{c,d}", 5), "echo{,}", "{}", "{a}", "{a,}", "\\{a,b\\}",
		"'~/.local/state/agent-decisions'", "\"$HOME/.local/state/agent-decisions\"", "~/agent-decisions/README.md",
		"$HOME", "${HOME}", "$HOMEx/.local/state/agent-decisions", "x$HOME/y", "~/.local/state/agent-decisions\u00e9",
		strings.Repeat("{a,b}", 6) + "{" + strings.Repeat(",", 50), filepath.Dir(state) + "/" + strings.Repeat("*", 30)}
	var corpus []*json.Object
	for _, word := range words {
		corpus = append(corpus, cmd("cat "+word), cmd("cd ~ && ls "+word), cmd("echo x >> "+word),
			json.NewObject("cmd", []any{"bash", "-lc", "cat " + word}), json.NewObject("file_path", word),
			json.NewObject("paths", []any{"/tmp/[z-a]", word}), json.NewObject("notebook_path", word),
			json.NewObject("input", "*** Begin Patch\n*** Add File: "+word+"\n+x\n*** End Patch"),
			json.NewObject("edits", []any{json.NewObject("file_path", word, "nested", json.NewObject("path", word))}),
			json.NewObject("content", "Documented at "+word))
	}
	return append(corpus, cmd("'unterminated "+state), cmd("ls ~ ~/x"), json.NewObject("path", 5),
		json.NewObject("file_path", []any{state}), json.NewObject(), json.NewObject("command", ""))
}

// py: agent_decisions/gates.py guard
func TestGuardMatchesPython(t *testing.T) {
	dir := setUp(t)
	if err := os.MkdirAll(stateDir(t)+"/sessions", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stateDir(t), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	inputs := guardCorpus(t)
	inputs = append(inputs, json.NewObject("path", filepath.Join(dir, "alias", "ledger.key")))
	nested := func(depth int) *json.Object {
		var value any = "x"
		for range depth {
			value = []any{value}
		}
		return json.NewObject("paths", value)
	}
	for depth := 990; depth <= 996; depth++ {
		inputs = append(inputs, nested(depth))
	}
	var cases []oracleCase
	for i, input := range inputs {
		text := dumps(t, input)
		for _, cwd := range []any{nil, os.Getenv("HOME"), dir, "/"} {
			var cwdWire any
			if cwd != nil {
				cwdWire = wire(cwd.(string))
			}
			name := fmt.Sprintf("guard #%d %.200s cwd=%v", i, text, cwd)
			cases = append(cases, oracleCase{name, []any{"guard", wire(text), cwdWire, guardFrame}, func() any {
				outcome, err := Guard(obj(t, text), cwd, guardFrame)
				return result(shown(outcome), err)
			}})
		}
	}
	compareWithPython(t, cases)
}

// --- gate scenarios

// gateStep is one gate call in a scenario. args are the call's arguments after the tool name, as JSON.
type gateStep struct {
	kind   string // decide, edit, question, plan, stop or consulted
	args   string // a JSON array
	answer string // FakeJev arguments: {"answers": {...}, "raises": [reason, detail] or null, "raw": text or null}
}

type scenario struct {
	name         string
	ctx          string // a JSON context spec; see oracleScript context()
	availability bool
	steps        []gateStep
}

func answers(pairs string) string {
	return `{"answers": {` + pairs + `}, "raises": null, "raw": null}`
}

func raises(reason string) string {
	return `{"answers": {}, "raises": ["` + reason + `", null], "raw": null}`
}

func spec(fields string) string {
	base := `{"host": "claude", "agent_id": null, "user_messages": ["decide the cache policy"], "calls": [],
		"turn": 0, "last": "", "brief": null, "evidence": {}, "entries": [], "repo_id": "", "inherited": []`
	if fields != "" {
		base += ", " + fields
	}
	return base + "}"
}

// contextOf builds the Go context that oracleScript context() builds.
func contextOf(t *testing.T, s *json.Object) Context {
	var calls []transcript.ToolCall
	for _, c := range s.Value("calls").([]any) {
		fields := c.([]any)
		call := transcript.ToolCall{ID: fields[0].(string), Name: fields[1].(string), Input: fields[2].(string)}
		if len(fields) > 3 {
			call.Output = fields[3].(string)
		}
		if len(fields) > 4 && fields[4] != nil {
			call.Execution = fields[4].(*json.Object)
		}
		calls = append(calls, call)
	}
	var users []string
	for _, message := range s.Value("user_messages").([]any) {
		users = append(users, message.(string))
	}
	tr := transcript.Transcript{UserMessages: users, ToolCalls: calls, LastAssistantText: s.Value("last").(string)}
	if turn, _ := s.Value("turn").(json.Int).Int64(); turn > 0 {
		tr.TurnCalls = calls[len(calls)-int(turn):]
	}
	if brief, ok := s.Value("brief").(string); ok {
		tr.Brief = &brief
	}
	bundle, err := evidence.Build(tr, nil, evidence.Options{Frame: 3})
	if err != nil {
		t.Fatal(err)
	}
	update := s.Value("evidence").(*json.Object)
	for _, key := range update.Keys() {
		bundle.Set(key, update.Value(key))
	}
	objects := func(key string) []*json.Object {
		var list []*json.Object
		for _, item := range s.Value(key).([]any) {
			list = append(list, item.(*json.Object))
		}
		return list
	}
	return Context{Host: s.Value("host").(string), SessionID: "s1", AgentID: s.Value("agent_id"), ToolUseID: "toolu_0",
		Transcript: tr, Evidence: bundle, Entries: objects("entries"), RepoID: s.Value("repo_id").(string),
		InheritedEntries: objects("inherited")}
}

// stepOf ports oracleScript step().
func stepOf(t *testing.T, ctx Context, n int, s gateStep) (Context, []any) {
	answer := obj(t, s.answer)
	api := &jevtest.Fake{Answers: map[string]any{}}
	scripted := answer.Value("answers").(*json.Object)
	for _, key := range scripted.Keys() {
		api.Answers[key] = scripted.Value(key)
	}
	if raised, ok := answer.Value("raises").([]any); ok {
		failure := &jev.JevError{Reason: raised[0].(string)}
		if detail, ok := raised[1].(string); ok {
			failure.Detail = detail
		}
		api.Raises = failure
	}
	if raw, ok := answer.Value("raw").(string); ok {
		api.Raw = []byte(raw)
	}
	var mu sync.Mutex
	bodies := []string{}
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		mu.Lock()
		bodies = append(bodies, hex.EncodeToString(body))
		mu.Unlock()
		return api.Post(body, key, timeout)
	}
	ask := func(state, questions *json.Object) (*json.Object, error) {
		return jev.Ask(state, questions, "k", post, nil)
	}
	ctx.ToolUseID = "toolu_" + strconv.Itoa(n)
	args := loads(t, s.args).([]any)
	var outcome Outcome
	var err error
	running := runtime.NumGoroutine()
	switch s.kind {
	case "decide":
		outcome, err = Decide(args[0].(string), ctx, ask)
	case "edit":
		outcome, err = EditGate(args[0].(string), args[1].(*json.Object), ctx, ask, gateFrame)
	case "question":
		outcome, err = QuestionGate(args[0].(*json.Object), ctx, ask)
	case "plan":
		var plan *string
		if text, ok := args[1].(string); ok {
			plan = &text
		}
		if args[0] != true {
			ask = nil
		}
		outcome, err = PlanGate(ctx, ask, plan, gateFrame)
	case "stop":
		outcome, err = StopGate(args[0].(string), args[1].(bool), ctx, ask, gateFrame)
	case "consulted":
		entries, err := Consulted(ctx)
		list := []any{}
		for _, entry := range entries {
			list = append(list, entry)
		}
		return ctx, []any{result(list, err), bodies, nil}
	}
	for runtime.NumGoroutine() > running { // chunks that the gate stopped waiting for still ask, as in Python
		time.Sleep(time.Millisecond)
	}
	slices.Sort(bodies)
	if err != nil {
		return ctx, []any{result(nil, err), bodies, nil}
	}
	var entry any
	if outcome.Record != nil {
		record := outcome.Record
		if s.kind == "decide" {
			record = entryOf(record, "verdict", outcome.Message)
		}
		added := merge(json.NewObject("host", ctx.Host, "session_id", "s1", "agent_id", ctx.AgentID,
			"tool_use_id", ctx.ToolUseID, "turn", ctx.Transcript.TurnKey(), "action", outcome.Action), record)
		entry = added
		if s.kind == "decide" {
			done := transcript.ToolCall{ID: ctx.ToolUseID.(string), Name: "Bash", Input: `{"command": "ts-decide '{}'"}`,
				Output: outcome.Message + "\n"}
			ctx.Transcript.ToolCalls = append(slices.Clip(ctx.Transcript.ToolCalls), done)
			ctx.Transcript.TurnCalls = append(slices.Clip(ctx.Transcript.TurnCalls), done)
		}
		ctx.Entries = append(slices.Clip(ctx.Entries), added)
	}
	return ctx, []any{result(shown(&outcome), nil), bodies, entry}
}

// scenarioCase runs s on both sides with its own state directory and a fixed sequence of uuid4 values.
func scenarioCase(t *testing.T, root string, i int, s scenario) oracleCase {
	var ids []any
	var stream []byte
	for n := range 8 {
		id := bytes.Repeat([]byte{byte(i*8 + n)}, 16)
		ids = append(ids, hex.EncodeToString(id))
		stream = append(stream, id...)
	}
	steps := []any{}
	for _, step := range s.steps {
		steps = append(steps, []any{step.kind, loads(t, step.args), loads(t, step.answer)})
	}
	name := strconv.Itoa(i)
	return oracleCase{
		name:    "scenario " + s.name,
		request: []any{"gates", wire(s.ctx), wire(dumps(t, steps)), wire(filepath.Join(root, "py", name)), s.availability, ids, gateFrame},
		run: func() any {
			t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(root, "go", name))
			if s.availability {
				availability(t, obj(t, s.ctx).Value("host").(string))
			}
			patch(t, &random, io.Reader(bytes.NewReader(stream)))
			replies := []any{}
			ctx := contextOf(t, obj(t, s.ctx))
			for n, step := range s.steps {
				var reply []any
				ctx, reply = stepOf(t, ctx, n+1, step)
				replies = append(replies, reply)
			}
			return []any{"ok", replies}
		},
	}
}

const (
	plainRequest = `{"decision": "Which cache policy should the parser use?", "options": [
		{"id": "lru", "description": "Least recently used eviction"},
		{"id": "lfu", "description": "Least frequently used eviction"}]}`
	factRequest = `{"decision": "Which cache policy?", "options": [{"id": "lru", "description": "LRU"},
		{"id": "lfu", "description": "LFU"}], "facts": ["decide the cache policy", "The tests passed."]}`
	verdictCall = `["toolu_d", "Bash", "{\"command\": \"ts-decide '{}'\"}", "ts-decide verdict: ADVISORY\ndecision: Which cache?\npick: lru\n"]`
)

func args(t *testing.T, values ...any) string { return dumps(t, values) }

// gateScenarios returns decide, edit, question, plan and stop scenarios, alone and chained after a decision.
func gateScenarios(t *testing.T) []scenario {
	decideAnswers := []string{"", `"pick": "lfu", "sufficient": 0.95`, `"pick": "lfu", "sufficient": 0.95, "owner": "user"`,
		`"pick": "lfu", "sufficient": 0.95, "unsupported": 0.9`, `"pick": "lfu", "sufficient": 0.95, "missing_option": 0.9`,
		`"pick": "lfu", "sufficient": 0.95, "helper_present": 0.9`, `"pick": "lfu", "sufficient": 0.95, "specified_lru": 0.9`,
		`"argued": 0.95, "argued_field": "decision"`, `"argued": 0.95, "argued_field": "options"`, `"pick": "none_fit", "sufficient": 0.95`,
		`"pick": "lfu", "sufficient": 0.83, "unsupported": 0.82, "missing_fact": "not_identified"`,
		`"pick": "lfu", "sufficient": 0.4, "evidence_needed": "implementation"`, `"pick": "lfu", "sufficient": 0.4, "evidence_needed": "comparison"`,
		`"pick": "lfu", "sufficient": 0.95, "invariant_0_lru": 0.95, "invariant_0_lfu": 0.01`,
		`"pick": "lfu", "sufficient": 0.95, "invariant_0_lru": 0.95, "invariant_0_lfu": 0.95`,
		`"pick": "lru", "sufficient": 1, "unsupported": 0, "owner": "agent"`, `"pick": "lfu", "sufficient": 0.79`,
		`"pick": "lfu", "sufficient": 0.8, "unsupported": 0.31`}
	invalid := []string{"not json", "{}", "[]", `{"decision": "x"}`, `{"decision": 5, "options": []}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}]}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "a", "description": "B"}]}`,
		`{"decision": "Which?", "options": [{"id": "A B", "description": "A"}, {"id": "b", "description": "B"}]}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b"}]}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "facts": "x"}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "facts": [5]}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "scope": []}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "scope": {"paths": "x"}}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "scope": {"paths": ["/abs"]}}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "scope": {"paths": ["../up"]}}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "contracts": ["AGENTS.md"]}`,
		`{"decision": "Which?", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}], "extra": 1}`,
		`{"decision": "Which?\nWhy?", "options": [{"id": "a", "description": "A;\nB"}, {"id": "none_fit", "description": "B"}]}`,
		`{"decision": "` + strings.Repeat("x", 3000) + `", "options": [{"id": "a", "description": "A"}, {"id": "b", "description": "B"}]}`}
	contexts := []string{spec(""), spec(`"agent_id": "w1"`), spec(`"host": "codex"`),
		spec(`"user_messages": ["Use LFU for the parser cache.", "The tests passed."], "brief": "Keep it simple."`)}
	var scenarios []scenario
	for _, request := range []string{plainRequest, progressRequest, factRequest} {
		for _, answer := range decideAnswers {
			for c, ctx := range contexts {
				scenarios = append(scenarios, scenario{name: fmt.Sprintf("decide %.40s ctx%d {%s}", request, c, answer), ctx: ctx,
					steps: []gateStep{{"decide", args(t, request), answers(answer)}}})
			}
		}
	}
	for _, request := range invalid {
		scenarios = append(scenarios, scenario{name: "decide invalid " + request, ctx: spec(""),
			steps: []gateStep{{"decide", args(t, request), answers("")}}})
	}
	for _, failure := range []string{raises("timeout"), raises("http_529"), `{"answers": {}, "raises": null, "raw": "not json"}`,
		`{"answers": {}, "raises": null, "raw": "{\"model\": \"jev\", \"answers\": {}}"}`} {
		for _, available := range []bool{false, true} {
			scenarios = append(scenarios, scenario{name: "decide failure " + failure, ctx: spec(""), availability: available,
				steps: []gateStep{{"decide", args(t, plainRequest), failure}}})
		}
	}
	// Repeated refusals reach the refusal cap; repeated advisories reuse the review.
	for _, answer := range []string{`"pick": "lfu", "sufficient": 0.95, "missing_option": 0.9`, `"pick": "lfu", "sufficient": 0.95`,
		`"argued": 0.95, "argued_field": "decision"`, `"pick": "lfu", "sufficient": 0.5`} {
		var steps []gateStep
		for range 4 {
			steps = append(steps, gateStep{"decide", args(t, progressRequest), answers(answer)})
		}
		steps = append(steps, gateStep{"consulted", "[]", answers("")})
		scenarios = append(scenarios, scenario{name: "repeat {" + answer + "}", ctx: spec(""), steps: steps})
	}

	edits := []struct{ name, input string }{
		{"Edit", `{"file_path": "parser.py", "old_string": "a", "new_string": "read_chunks()"}`},
		{"Edit", `{"file_path": "other.py", "new_string": "x"}`},
		{"Edit", `{"file_path": "/abs/parser.py", "new_string": "x"}`},
		{"Write", `{"file_path": "parser.py", "content": "` + strings.Repeat("# pad\\n", 3000) + `read_chunks()"}`},
		{"MultiEdit", `{"file_path": "parser.py", "edits": [{"old_string": "` + strings.Repeat("x", 9000) + `", "new_string": "` + strings.Repeat("y", 9000) + `", "replace_all": false}]}`},
		{"Bash", `{"command": "python update.py parser.py"}`},
		{"Bash", `{"command": "python3"}`},
		{"Bash", `{"command": "bash -s <<'SH'\nprintf ok\nSH"}`},
		{"Bash", `{"command": "python3 -c 'exec(input())'"}`},
		{"Bash", `{"command": "echo 'unterminated"}`},
		{"exec_command", `{"cmd": ["bash", "-lc", "sed -i s/a/b/ parser.py"]}`},
		{"apply_patch", `{"input": "*** Begin Patch\n*** Update File: parser.py\n@@\n-a\n+b\n*** End Patch"}`},
		{"NotebookEdit", `{"notebook_path": "nb.ipynb", "new_source": "x = 1"}`},
		{"spawn_agent", `{"task_name": "t", "message": "Read-only trace."}`},
		{"mcp__x__write", `{"path": "parser.py", "data": {"nested": [1, 2.5, true, null, "s"]}}`},
		{"Edit", `{"file_path": "parser.py", "new_string": "` + strings.Repeat("é", 30000) + `"}`},
	}
	editAnswers := []string{"", `"needs_decision": 0.99`, `"needs_decision": 0.99, "covered_0": 0.9`, `"writes_files": 0.99, "needs_decision": 0.99`,
		`"writes_files": 0.99, "needs_decision": 0.01`, `"accepts_code_input": 0.99`}
	for _, edit := range edits {
		for _, answer := range editAnswers {
			scenarios = append(scenarios, scenario{name: "edit " + edit.name + " {" + answer + "} " + edit.input, ctx: spec(""),
				steps: []gateStep{{"edit", args(t, edit.name, obj(t, edit.input)), answers(answer)},
					{"edit", args(t, edit.name, obj(t, edit.input)), answers(`"needs_decision": 0.99`)}}})
		}
		for _, available := range []bool{false, true} {
			scenarios = append(scenarios, scenario{name: "edit outage " + edit.name, ctx: spec(""), availability: available,
				steps: []gateStep{{"edit", args(t, edit.name, obj(t, edit.input)), raises("timeout")}}})
		}
	}

	questions := []string{asked, `{"questions": [{"title": "Which database?", "options": ["Postgres", "SQLite"]}]}`,
		`{"questions": [{"question": "A?", "options": []}, {"question": "B?", "header": "b", "multiSelect": true, "options": [{"label": "x"}]}]}`,
		`{"questions": []}`, `{"questions": "x"}`, `{}`, `{"questions": [5]}`,
		`{"questions": [{"title": "` + strings.Repeat("x", 250000) + `"}]}`}
	for _, question := range questions {
		for _, answer := range []string{"", `"stated_0": 0.9, "owner_0": "user"`, `"stated_0": 0.1, "owner_0": "agent"`,
			`"stated_0": 0.1, "owner_0": "user", "stated_1": 0.95`} {
			for c, ctx := range contexts[:3] {
				scenarios = append(scenarios, scenario{name: fmt.Sprintf("question ctx%d {%s} %.80s", c, answer, question), ctx: ctx,
					steps: []gateStep{{"question", args(t, obj(t, question)), answers(answer)},
						{"question", args(t, obj(t, question)), answers(answer)}}})
			}
		}
		scenarios = append(scenarios, scenario{name: "question outage", ctx: spec(""), availability: true,
			steps: []gateStep{{"question", args(t, obj(t, question)), raises("http_529")}}})
	}

	finals := []string{"", "Done.", "I chose SQLite over PostgreSQL.", "Status. " + strings.Repeat("x ", 9000) + "I chose LFU.",
		"```\nI chose SQLite\n```\nAll tests pass.", "~~~py\nx\n~~~\n\n   ```\nunterminated", "Use LRU.\n\n\nNext: run tests."}
	for _, final := range finals {
		for _, answer := range []string{"", `"made_choice": 0.95`, `"made_choice": 0.95, "uncovered_choice": 0.99`,
			`"made_choice": 0.95, "covered_0": 0.95`} {
			for _, active := range []bool{false, true} {
				scenarios = append(scenarios, scenario{name: fmt.Sprintf("stop %v {%s} %.60q", active, answer, final), ctx: spec(""),
					steps: []gateStep{{"stop", args(t, final, active), answers(answer)}, {"stop", args(t, final, active), answers(answer)}}})
			}
		}
	}
	for _, plan := range []any{nil, "1. Use LRU in parser.py.\n2. Test it."} {
		for _, ask := range []bool{false, true} {
			for _, answer := range []string{"", `"made_choice": 0.95`, `"needs_decision": 0.99`} {
				scenarios = append(scenarios, scenario{name: fmt.Sprintf("plan %v %v {%s}", ask, plan, answer), ctx: spec(""),
					steps: []gateStep{{"plan", args(t, ask, plan), answers(answer)}, {"plan", args(t, ask, plan), answers(answer)}}})
			}
		}
	}

	// Chains: a decision followed by the gates that read it.
	for _, answer := range []string{`"pick": "lfu", "sufficient": 0.95, "invariant_0_lru": 0.95, "invariant_0_lfu": 0.95`,
		`"pick": "lfu", "sufficient": 0.95, "owner": "user", "invariant_0_lru": 0.95, "invariant_0_lfu": 0.95`,
		`"pick": "lfu", "sufficient": 0.4`, `"pick": "lfu", "sufficient": 0.95, "missing_option": 0.9`} {
		for c, ctx := range contexts[:3] {
			for _, request := range []string{plainRequest, progressRequest} {
				steps := []gateStep{{"decide", args(t, request), answers(answer)}, {"consulted", "[]", answers("")}}
				for _, edit := range edits[:3] {
					steps = append(steps, gateStep{"edit", args(t, edit.name, obj(t, edit.input)), answers(`"needs_decision": 0.99, "covered_0": 0.9`)},
						gateStep{"edit", args(t, edit.name, obj(t, edit.input)), answers(`"needs_decision": 0.99`)})
				}
				steps = append(steps, gateStep{"plan", args(t, true, "Use LFU."), answers(`"made_choice": 0.95`)},
					gateStep{"plan", args(t, false, nil), answers("")},
					gateStep{"stop", args(t, "I chose LFU over LRU.", false), answers(`"made_choice": 0.95, "covered_0": 0.95`)},
					gateStep{"stop", args(t, "I chose SQLite.", false), answers(`"made_choice": 0.95, "uncovered_choice": 0.99`)},
					gateStep{"decide", args(t, request), answers(answer)})
				scenarios = append(scenarios, scenario{name: fmt.Sprintf("chain ctx%d {%s} %.30s", c, answer, request), ctx: ctx, steps: steps})
			}
		}
	}
	// Ledger lines that consulted() must reject or accept.
	entryLines := []string{
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY\ndecision: Which cache?"}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY"}`,
		`{"host": "codex", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY"}`,
		`{"host": "claude", "agent_id": "w", "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY"}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "binding", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY"}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_x", "receipt_id": "r1", "verdict": "ts-decide verdict: ADVISORY"}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "unverified", "tool_use_id": "toolu_d", "verdict": "ts-decide: decision unverified ("}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": 5}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY", "receipt_id": 7}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY", "state": {"scope": {"paths": ["parser.py"]}}, "contracts": {"AGENTS.md": "h"}}`,
		`{"host": "claude", "agent_id": null, "gate": "decide", "action": "advisory", "tool_use_id": "toolu_d", "verdict": "ts-decide verdict: ADVISORY", "state": {"scope": {"paths": ["src/"]}, "options": {"lru": "LRU"}}, "pick": "lru", "receipt_id": null}`,
	}
	for _, line := range entryLines {
		ctx := spec(`"calls": [` + verdictCall + `], "turn": 1, "entries": [` + line + `]`)
		scenarios = append(scenarios, scenario{name: "entry " + line, ctx: ctx, steps: []gateStep{
			{"consulted", "[]", answers("")}, {"plan", args(t, false, nil), answers("")},
			{"edit", args(t, "Edit", obj(t, `{"file_path": "parser.py", "new_string": "x"}`)), answers(`"needs_decision": 0.99, "covered_0": 0.9`)},
			{"edit", args(t, "Edit", obj(t, `{"file_path": "src/a.py", "new_string": "x"}`)), answers(`"needs_decision": 0.99, "covered_0": 0.9`)},
			{"stop", args(t, "I chose LRU.", false), answers(`"made_choice": 0.95, "covered_0": 0.95`)}}})
	}
	// Deep tool inputs reach the Python recursion limit inside the gates.
	// Only a chunked input recurses, so the leaf exceeds the clip.
	for depth := 984; depth <= 996; depth++ {
		var list, object any = strings.Repeat("x", 20000), strings.Repeat("x", 20000)
		for range depth {
			list, object = []any{list}, json.NewObject("k", object)
		}
		for _, value := range []any{list, object} {
			scenarios = append(scenarios, scenario{name: fmt.Sprintf("deep edit %d", depth), ctx: spec(""),
				steps: []gateStep{{"edit", args(t, "mcp__x__write", json.NewObject("file_path", "parser.py", "data", value)), answers("")}}})
		}
	}
	// A scoped decision makes the edit gate collect the paths of a deep input.
	steps := []gateStep{{"decide", args(t, progressRequest),
		answers(`"pick": "lfu", "sufficient": 0.95, "invariant_0_lru": 0.95, "invariant_0_lfu": 0.95`)}}
	for depth := 980; depth <= 1000; depth++ {
		var value any = json.NewObject("path", "parser.py")
		for range depth {
			value = []any{value}
		}
		steps = append(steps, gateStep{"edit", args(t, "mcp__x__write", json.NewObject("data", value)), answers(`"needs_decision": 0.99`)})
	}
	return append(scenarios, scenario{name: "deep scoped edit", ctx: spec(""), steps: steps})
}

// py: agent_decisions/gates.py decide, edit_gate, question_gate, plan_gate, stop_gate, consulted
func TestGatesMatchPython(t *testing.T) {
	root := setUp(t)
	var cases []oracleCase
	for i, s := range gateScenarios(t) {
		cases = append(cases, scenarioCase(t, root, i, s))
	}
	compareWithPython(t, cases)
}
