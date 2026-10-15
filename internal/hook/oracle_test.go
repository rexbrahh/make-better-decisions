package hook

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
)

// The fixed clock and uuid of both engines in a replay.
const (
	oracleTime = 1_791_000_000
	oracleUUID = "0123456789ab4def8123456789abcdef"
)

// replayScript runs the steps of a replay case through agent_decisions/hook.py handle, with the clock,
// perf_counter and uuid4 of hook.py fixed. It reads the case as JSON on stdin and prints one result per
// step: null for a setup step, or {"out": json.dumps(response) or null, "error": [class, str] or null,
// "requests": [json.dumps(request), ...], "elapsed": seconds} with the Jev request bodies in arrival order
// and the wall time of the handle call.
//
// Setup steps: "append" adds a signed ledger entry; "complete" appends a finished tool call to a transcript,
// whose output is the verdict that this engine printed for that tool_use_id.
const replayScript = `
import json, shlex, sys, time, types
sys.path.insert(0, sys.argv[1])
from agent_decisions import hook, jev, ledger
from tests.helpers import FakeJev, codex_call, codex_output, tool_result, tool_use

hook.__file__ = sys.argv[2]
spec = json.loads(sys.stdin.read())
hook.time = types.SimpleNamespace(perf_counter=lambda: 0.0, strftime=time.strftime,
                                  gmtime=lambda: time.gmtime(spec["time"]))
hook.uuid = types.SimpleNamespace(uuid4=lambda: types.SimpleNamespace(hex=spec["uuid"]))
outs, results = {}, []
for step in spec["steps"]:
    if step["kind"] == "append":
        ledger.append(step["entry"])
        results.append(None)
        continue
    if step["kind"] == "complete":
        out = outs[step["id"]]
        verdict = shlex.split(out["hookSpecificOutput"]["updatedInput"][step["key"]])[2]
        if step["host"] == "codex":
            records = [codex_call(step["id"], step["name"], json.dumps(step["input"])),
                       codex_output(step["id"], verdict)]
        else:
            records = [tool_use(step["id"], step["name"], step["input"]), tool_result(step["id"], verdict)]
        with open(step["path"], "a") as stream:
            stream.write("".join(json.dumps(r) + "\n" for r in records))
        results.append(None)
        continue
    fake = step["fake"]
    api = FakeJev(raises=None if fake["raises"] is None else jev.JevError(fake["raises"]),
                  raw=None if fake["raw"] is None else fake["raw"].encode(), **fake["answers"])
    started = time.perf_counter()
    try:
        out, error = hook.handle(step["event"], step["payload"], step["host"], post=api), None
    except Exception as exception:
        out, error = None, [type(exception).__name__, str(exception)]
    elapsed = time.perf_counter() - started
    if out is not None:
        outs[step["payload"].get("tool_use_id")] = out
    results.append({"out": None if out is None else json.dumps(out), "error": error,
                    "requests": [json.dumps(request) for request in api.requests], "elapsed": elapsed})
print(json.dumps(results))
`

// replayStep is one step of a replay case: a hook call, or a setup step.
type replayStep struct {
	kind    string // "handle", "append" or "complete"
	event   string
	host    string
	payload any
	fake    replayFake
	entry   *json.Object // append
	// complete: append tool call id, with this name and input, to the transcript at path, as host records it.
	id, key, name, path string
	input               *json.Object
}

// replayFake is a scripted FakeJev.
type replayFake struct {
	answers map[string]any
	raises  string // a JevError reason
	raw     *string
}

func (f replayFake) wire() *json.Object {
	answers := &json.Object{}
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range f.answers {
			if !yield(key) {
				return
			}
		}
	}) {
		answers.Set(key, f.answers[key])
	}
	var raises, raw any
	if f.raises != "" {
		raises = f.raises
	}
	if f.raw != nil {
		raw = *f.raw
	}
	return json.NewObject("answers", answers, "raises", raises, "raw", raw)
}

func (f replayFake) jev() *jevtest.Fake {
	api := &jevtest.Fake{Answers: f.answers}
	if f.raises != "" {
		api.Raises = &jev.JevError{Reason: f.raises}
	}
	if f.raw != nil {
		api.Raw = []byte(*f.raw)
	}
	return api
}

// replayCase is a sequence of steps against one state directory. files writes the case's transcripts and
// sources into the work directory before each engine runs.
type replayCase struct {
	name  string
	files func(t *testing.T, work string)
	steps func(work string) []replayStep
}

// stepResult is what one engine produced for one step.
type stepResult struct {
	out      *string
	errClass string
	errText  string
	requests []string
	elapsed  time.Duration // the wall time of the handle call
}

// oracleSetUp gives the replay its work directory and environment, and returns the Python interpreter.
func oracleSetUp(t *testing.T) (string, string) {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	work := setUp(t)
	patch(t, &now, func() time.Time { return time.Unix(oracleTime, 0) })
	patch(t, &perfCounter, func() float64 { return 0 })
	patch(t, &newID, func() string { return oracleUUID })
	return work, python
}

// resetState empties the state directory except for a fixed ledger key, so both engines seal with it.
func resetState(t testing.TB) string {
	t.Helper()
	state := os.Getenv("AGENT_DECISIONS_STATE")
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "ledger.key"), []byte("oracle-ledger-key-0123456789abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

// snapshot returns the files of the state directory by relative path, without the key and lock files.
func snapshot(t *testing.T, state string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(state, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, _ := filepath.Rel(state, path)
		if name == "ledger.key" || strings.HasSuffix(name, ".lock") {
			return nil
		}
		data, err := os.ReadFile(path)
		files[name] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// runPython replays steps through hook.py.
func runPython(t *testing.T, python string, steps []replayStep) []*stepResult {
	t.Helper()
	wire := make([]any, len(steps))
	for i, step := range steps {
		switch step.kind {
		case "append":
			wire[i] = json.NewObject("kind", "append", "entry", step.entry)
		case "complete":
			wire[i] = json.NewObject("kind", "complete", "id", step.id, "key", step.key, "host", step.host,
				"name", step.name, "input", step.input, "path", step.path)
		default:
			wire[i] = json.NewObject("kind", "handle", "event", step.event, "host", step.host,
				"payload", step.payload, "fake", step.fake.wire())
		}
	}
	source := oracletest.Source(t)
	command := exec.Command(python, "-I", "-c", replayScript, source, filepath.Join(repoRoot, "bin", "agent-decisions"))
	command.Stdin = strings.NewReader(dumps(t, json.NewObject("time", json.IntOf(oracleTime), "uuid", oracleUUID,
		"steps", wire)))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, stderr.String())
	}
	replies := loads(t, string(output)).([]any)
	results := make([]*stepResult, len(replies))
	for i, reply := range replies {
		object, ok := reply.(*json.Object)
		if !ok {
			continue
		}
		result := &stepResult{}
		if text, ok := object.Value("out").(string); ok {
			result.out = &text
		}
		if failure, ok := object.Value("error").([]any); ok {
			result.errClass, result.errText = failure[0].(string), failure[1].(string)
		}
		for _, request := range object.Value("requests").([]any) {
			result.requests = append(result.requests, request.(string))
		}
		result.elapsed = time.Duration(object.Value("elapsed").(float64) * float64(time.Second))
		results[i] = result
	}
	return results
}

// runGo replays steps through Handle.
func runGo(t *testing.T, steps []replayStep) []*stepResult {
	t.Helper()
	outs := map[any]*json.Object{}
	results := make([]*stepResult, len(steps))
	for i, step := range steps {
		switch step.kind {
		case "append":
			if err := ledger.Append(step.entry); err != nil {
				t.Fatal(err)
			}
			continue
		case "complete":
			words, err := shlex.Split(str(t, outs[step.id], "hookSpecificOutput", "updatedInput", step.key))
			if err != nil {
				t.Fatal(err)
			}
			records := []*json.Object{toolUse(step.id, step.name, step.input), toolResult(step.id, words[2])}
			if step.host == "codex" {
				records = []*json.Object{codexCall(step.id, step.name, dumps(t, step.input)),
					codexOutput(step.id, words[2])}
			}
			stream, err := os.OpenFile(step.path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				stream.WriteString(dumps(t, record) + "\n")
			}
			stream.Close()
			continue
		}
		api := step.fake.jev()
		// The payload is decoded from its JSON text, as Python receives it.
		payload := loads(t, dumps(t, step.payload))
		started := time.Now()
		out, err := Handle(step.event, payload, step.host, api.Post)
		result := &stepResult{elapsed: time.Since(started)}
		if err != nil {
			result.errClass, result.errText = pyClass(err), err.Error()
		}
		if out != nil {
			text := dumps(t, out)
			result.out = &text
			if object, ok := step.payload.(*json.Object); ok {
				outs[object.Value("tool_use_id")] = out
			}
		}
		for _, request := range api.Requests() {
			result.requests = append(result.requests, dumps(t, request))
		}
		results[i] = result
	}
	return results
}

var receiptPatternText = regexp.MustCompile(`review_[0-9a-f]{32}`)

// normalizer replaces what the engines cannot share: receipt ids, which gates draws from its own random
// source, become ordinals in order of appearance; the *_ms timings of jev's own clock under "metrics" become
// 0, and api_calls, which parallel chunks append in any order, are sorted.
type normalizer struct {
	receipts map[string]string
}

func (n *normalizer) text(s string) string {
	return receiptPatternText.ReplaceAllStringFunc(s, func(id string) string {
		if _, ok := n.receipts[id]; !ok {
			n.receipts[id] = fmt.Sprintf("review_<%d>", len(n.receipts))
		}
		return n.receipts[id]
	})
}

// json normalizes a JSON text, dropping a ledger row's seal.
func (n *normalizer) json(t *testing.T, s string) string {
	t.Helper()
	value := loads(t, n.text(s))
	if row, ok := value.(*json.Object); ok {
		row.Delete("_ledger_seal")
	}
	text, err := json.Dumps(zeroTimings(value, false), json.Options{SortKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func zeroTimings(value any, metrics bool) any {
	switch value := value.(type) {
	case *json.Object:
		for _, key := range value.Keys() {
			item := value.Value(key)
			_, number := item.(float64)
			switch {
			case metrics && strings.HasSuffix(key, "_ms") && number:
				value.Set(key, 0.0)
			case metrics && key == "api_calls":
				calls, _ := item.([]any)
				for _, call := range calls {
					zeroTimings(call, true)
				}
				slices.SortFunc(calls, func(a, b any) int {
					x, _ := json.Dumps(a, json.Options{SortKeys: true})
					y, _ := json.Dumps(b, json.Options{SortKeys: true})
					return strings.Compare(x, y)
				})
			default:
				zeroTimings(item, metrics || key == "metrics")
			}
		}
	case []any:
		for _, item := range value {
			zeroTimings(item, metrics)
		}
	}
	return value
}

// compareResults fails on any difference between the engines' step results, after normalization.
func compareResults(t *testing.T, steps []replayStep, python, golang []*stepResult, pn, gn *normalizer) {
	t.Helper()
	for i, step := range steps {
		p, g := python[i], golang[i]
		if p == nil || g == nil {
			continue
		}
		label := fmt.Sprintf("step %d (%s %s)", i, step.event, step.host)
		if p.errClass != g.errClass {
			t.Errorf("%s: error class python %q (%s), go %q (%s)", label, p.errClass, p.errText, g.errClass, g.errText)
		} else if p.errText != g.errText {
			t.Logf("%s: %s text differs: python %q, go %q", label, p.errClass, p.errText, g.errText)
		}
		switch {
		case (p.out == nil) != (g.out == nil):
			t.Errorf("%s: response python %v, go %v", label, deref(p.out), deref(g.out))
		case p.out != nil && *p.out != *g.out && pn.text(*p.out) != gn.text(*g.out):
			t.Errorf("%s: response\npython %s\ngo     %s", label, *p.out, *g.out)
		}
		pr, gr := sortedRequests(t, pn, p.requests), sortedRequests(t, gn, g.requests)
		if !slices.Equal(pr, gr) {
			t.Errorf("%s: Jev requests differ\npython %s\ngo     %s", label, strings.Join(pr, "\n"), strings.Join(gr, "\n"))
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

// sortedRequests normalizes request bodies and sorts them, since edit chunks ask in parallel.
func sortedRequests(t *testing.T, n *normalizer, requests []string) []string {
	t.Helper()
	var normalized []string
	for _, request := range requests {
		normalized = append(normalized, n.json(t, request))
	}
	slices.Sort(normalized)
	return normalized
}

// compareStates fails on any difference between the engines' state files. Ledger lines compare byte for
// byte, seal included, unless normalization had to change them; then they compare normalized, without the
// seal, and checkSeals proves each engine's seals separately.
func compareStates(t *testing.T, python, golang map[string][]byte, pn, gn *normalizer) {
	t.Helper()
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range python {
			yield(name)
		}
	})
	for name := range golang {
		if _, ok := python[name]; !ok {
			t.Errorf("only go wrote %s", name)
		}
	}
	for _, name := range names {
		p, g := python[name], golang[name]
		if g == nil {
			t.Errorf("only python wrote %s", name)
			continue
		}
		if !strings.HasSuffix(name, ".jsonl") {
			if !bytes.Equal(p, g) && pn.text(string(p)) != gn.text(string(g)) {
				t.Errorf("%s differs\npython %q\ngo     %q", name, p, g)
			}
			continue
		}
		pl, gl := strings.Split(string(p), "\n"), strings.Split(string(g), "\n")
		if len(pl) != len(gl) {
			t.Errorf("%s: python wrote %d lines, go %d\npython %s\ngo     %s", name, len(pl)-1, len(gl)-1, p, g)
			continue
		}
		for i := range pl {
			if pl[i] == gl[i] {
				continue
			}
			if pl[i] == "" || gl[i] == "" || pn.json(t, pl[i]) != gn.json(t, gl[i]) {
				t.Errorf("%s line %d\npython %s\ngo     %s", name, i, pl[i], gl[i])
			}
		}
	}
}

// checkSeals verifies with the Go ledger that every row in the state directory carries a valid seal.
func checkSeals(t *testing.T, engine string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		if !strings.HasPrefix(name, "sessions/") {
			continue
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		session := str(t, loads(t, lines[0]), "entry", "session_id")
		rows, err := ledger.Entries(session, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(lines) {
			t.Errorf("%s: %s has %d lines, the Go ledger verifies %d", engine, name, len(lines), len(rows))
		}
	}
}

// replay runs one case through both engines and compares responses, Jev requests and state files.
func replay(t *testing.T, python, work string, c replayCase) {
	t.Helper()
	steps := c.steps(work)
	state := resetState(t)
	c.files(t, work)
	fromPython := runPython(t, python, steps)
	pythonFiles := snapshot(t, state)
	checkSeals(t, "python", pythonFiles)

	resetState(t)
	c.files(t, work)
	fromGo := runGo(t, steps)
	goFiles := snapshot(t, state)
	checkSeals(t, "go", goFiles)

	pn, gn := &normalizer{map[string]string{}}, &normalizer{map[string]string{}}
	compareResults(t, steps, fromPython, fromGo, pn, gn)
	compareStates(t, pythonFiles, goFiles, pn, gn)
	responses, errors, requests, rows := 0, 0, 0, 0
	for _, result := range fromPython {
		if result != nil {
			responses, requests = responses+btoi(result.out != nil), requests+len(result.requests)
			errors += btoi(result.errClass != "")
		}
	}
	for _, data := range pythonFiles {
		rows += bytes.Count(data, []byte("\n"))
	}
	t.Logf("compared %d steps: %d responses, %d errors, %d Jev requests, %d state files with %d lines, %d receipts",
		len(steps), responses, errors, requests, len(pythonFiles), rows, len(pn.receipts))
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- the replay corpus

const oracleContract = "Preserve source identities and required review.\n"

// oracleFiles writes the transcripts and sources that the corpus cases read.
func oracleFiles(t *testing.T, work string) {
	t.Helper()
	writeJSONL(t, work, "s1.jsonl", []*json.Object{
		human("speed up the tile cache"),
		toolUse("toolu_1", "Bash", json.NewObject("command", "trace")),
		toolResult("toolu_1", "tiles are re-read in bursts"),
		assistantText("I will compare LRU and LFU before editing parser.py."),
	}, "")
	writeJSONL(t, filepath.Join(work, "s1", "subagents"), "agent-a1.jsonl", []*json.Object{
		json.NewObject("type", "user", "isSidechain", true, "agentId", "a1",
			"message", json.NewObject("role", "user", "content", "brief: pick a cache for parser.py"))}, "")
	writeJSONL(t, work, "codex.jsonl", []*json.Object{
		codexUser("Which cache should the parser use?"),
		codexAssistant("Comparing the options."),
		codexCall("c1", "exec_command", `{"cmd": "ls"}`),
		codexOutput("c1", "parser.py\nAGENTS.md"),
	}, "")
	writeJSONL(t, work, "native.jsonl", []*json.Object{
		oracleNativeMetadata("approved-sdk", "approved-sdk", work, "", ""),
		codexUser("Continue the authorized SDK fixes."),
	}, "")
	writeJSONL(t, work, "child.jsonl", []*json.Object{
		oracleNativeMetadata("child", "other-session", work, "other-session", "/root/sdk"),
		codexUser("Continue the parser slice."),
	}, "")
	writeJSONL(t, work, "broken.jsonl", nil, "{not json\n")
	writeFile(t, filepath.Join(work, "AGENTS.md"), strings.Repeat(oracleContract, 40))
	writeFile(t, filepath.Join(work, "parser.py"), "def parse(text):\n    return text.split()\n")
}

// oracleNativeMetadata ports tests/test_receipts.py native_metadata; an empty parent is None.
func oracleNativeMetadata(threadID, sessionID, cwd, parent, agent string) *json.Object {
	var source any = "vscode"
	threadSource := "codex"
	if parent != "" {
		source = json.NewObject("subagent", json.NewObject("thread_spawn", json.NewObject(
			"parent_thread_id", parent, "depth", json.IntOf(1), "agent_path", agent,
			"agent_nickname", "Fixture", "agent_role", "default")))
		threadSource = "subagent"
	}
	return json.NewObject("type", "session_meta", "payload", json.NewObject(
		"id", threadID, "session_id", sessionID, "cwd", cwd, "source", source, "thread_source", threadSource,
		"originator", "Codex Desktop", "cli_version", "0.162.0-alpha.17.2", "model_provider", "openai",
		"multi_agent_version", "v2"))
}

func call(event, host string, payload any, fake replayFake) replayStep {
	return replayStep{kind: "handle", event: event, host: host, payload: payload, fake: fake}
}

func answers(pairs ...any) replayFake {
	values := map[string]any{}
	for i := 0; i < len(pairs); i += 2 {
		values[pairs[i].(string)] = pairs[i+1]
	}
	return replayFake{answers: values}
}

func installation(host string, extra ...any) replayStep {
	spec := with(json.NewObject("host", host, "review_mode", "required"), extra...)
	return replayStep{kind: "append", entry: json.NewObject("session_id", "installation", "gate", "installation",
		"action", "configured", "spec", spec)}
}

func complete(host, path, id, key string, input *json.Object) replayStep {
	name := "Bash"
	if host == "codex" {
		name = "exec_command"
	}
	return replayStep{kind: "complete", host: host, path: path, id: id, key: key, name: name, input: input}
}

func replayCases(t *testing.T) []replayCase {
	request := `{"decision": "Which cache should parser.py use?", "options": [{"id": "lru", "description": "LRU"}, {"id": "lfu", "description": "LFU"}]}`
	scoped := `{"decision": "Which cache should parser.py use?", "options": [{"id": "lru", "description": "LRU"}, ` +
		`{"id": "lfu", "description": "LFU"}], "contracts": ["AGENTS.md"], "scope": {"paths": ["parser.py"]}}`
	decideIn := func(r string) string { return "ts-decide " + shlex.Quote(r) }
	sure := answers("pick", "lfu", "sufficient", 0.9, "owner", "agent")
	none := replayFake{}
	malformed := `{"answers": {}}`

	claude := func(work string) []replayStep {
		main := filepath.Join(work, "s1.jsonl")
		base := json.NewObject("session_id", "s1", "transcript_path", main, "cwd", work)
		bash := func(id, command string) *json.Object {
			return with(base, "tool_name", "Bash", "tool_use_id", id,
				"tool_input", json.NewObject("command", command, "description", "decide"))
		}
		tool := func(id, name string, input any) *json.Object {
			return with(base, "tool_name", name, "tool_use_id", id, "tool_input", input)
		}
		parser := filepath.Join(work, "parser.py")
		decided := json.NewObject("command", decideIn(request), "description", "decide")
		return []replayStep{
			call("session_start", "claude", with(base, "source", "startup"), none),
			call("user_prompt_submit", "claude", with(base, "prompt", "Prefer the simpler cache."), none),
			call("pre_tool_use", "claude", bash("toolu_d", decideIn(request)), sure),
			complete("claude", main, "toolu_d", "command", decided),
			call("pre_tool_use", "claude", bash("toolu_d", decideIn(request)), sure),
			call("pre_tool_use", "claude", tool("e1", "Edit", json.NewObject("file_path", parser,
				"old_string", "text.split()", "new_string", "text.split(None)")), none),
			call("pre_tool_use", "claude", tool("e2", "Write", json.NewObject("file_path", parser,
				"content", "def parse(text):\n    return list(text)\n")), answers("needs_decision", 1.0)),
			call("pre_tool_use", "claude", tool("e3", "MultiEdit", json.NewObject("file_path", parser,
				"edits", []any{json.NewObject("old_string", "a", "new_string", "b")})), none),
			call("pre_tool_use", "claude", tool("e4", "NotebookEdit", json.NewObject("notebook_path",
				filepath.Join(work, "n.ipynb"), "new_source", "x = 1")), none),
			call("pre_tool_use", "claude", tool("e5", "mcp__filesystem__write_file", json.NewObject("path", parser,
				"content", "pass\n")), none),
			call("pre_tool_use", "claude", tool("e8", "Edit", json.NewObject("file_path", parser,
				"old_string", strings.Repeat("x", 20_000), "new_string", strings.Repeat("y", 20_000))), none),
			call("pre_tool_use", "claude", tool("p1", "ExitPlanMode", json.NewObject("plan", "1. Use LFU in parser.py")), none),
			call("pre_tool_use", "claude", tool("p2", "ExitPlanMode", json.NewObject("plan", json.IntOf(3))), none),
			call("pre_tool_use", "claude", tool("r1", "Read", json.NewObject("file_path", parser)), none),
			call("pre_tool_use", "claude", bash("g1", "cat "+filepath.Join(work, "state", "sessions", "s1.jsonl")), none),
			call("pre_tool_use", "claude", bash("m1", "ts-decide '{}' | cat"), none),
			call("pre_tool_use", "claude", bash("i1", "ts-decide 'not json'"), none),
			call("pre_tool_use", "claude", bash("i2", decideIn(`{"decision": "x", "contracts": "AGENTS.md"}`)), none),
			call("pre_tool_use", "claude", bash("toolu_s", "ts-decide <<'EOF'\n"+scoped+"\nEOF"), sure),
			complete("claude", main, "toolu_s", "command", json.NewObject("command", "ts-decide <<'EOF'\n"+scoped+"\nEOF")),
			call("pre_tool_use", "claude", tool("e6", "Edit", json.NewObject("file_path", parser,
				"old_string", "text.split()", "new_string", "text.split(' ')")), none),
			call("pre_tool_use", "claude", tool("e7", "Edit", json.NewObject("file_path", parser,
				"old_string", "a", "new_string", "b")), replayFake{raises: "unreachable"}),
			call("pre_tool_use", "claude", bash("toolu_m", decideIn(request)), replayFake{raw: &malformed}),
			call("pre_tool_use", "claude", tool("", "Edit", "not a dict"), none),
			call("stop", "claude", with(base, "last_assistant_message", "I chose LFU."), none),
			call("subagent_stop", "claude", base, none),
			call("bogus", "claude", base, none),
		}
	}
	subagent := func(work string) []replayStep {
		base := json.NewObject("session_id", "s1", "transcript_path", filepath.Join(work, "s1.jsonl"), "cwd", work,
			"agent_id", "a1")
		parser := filepath.Join(work, "parser.py")
		return []replayStep{
			call("session_start", "claude", base, none),
			call("user_prompt_submit", "claude", with(base, "prompt", "ignored for agents"), none),
			call("pre_tool_use", "claude", with(base, "tool_name", "Bash", "tool_use_id", "toolu_a",
				"tool_input", json.NewObject("command", decideIn(request))), sure),
			call("pre_tool_use", "claude", with(base, "tool_name", "Edit", "tool_use_id", "ea",
				"tool_input", json.NewObject("file_path", parser, "old_string", "a", "new_string", "b")), none),
			call("pre_tool_use", "claude", with(base, "agent_transcript_path", filepath.Join(work, "s1.jsonl"),
				"tool_name", "Edit", "tool_use_id", "eb", "tool_input", json.NewObject("file_path", parser)), none),
		}
	}
	unreadable := func(work string) []replayStep {
		var steps []replayStep
		for _, path := range []string{filepath.Join(work, "missing.jsonl"), filepath.Join(work, "broken.jsonl"), work} {
			base := json.NewObject("session_id", "s2", "transcript_path", path, "cwd", work)
			steps = append(steps,
				call("session_start", "claude", base, none),
				call("pre_tool_use", "claude", with(base, "tool_name", "Bash", "tool_use_id", "u1",
					"tool_input", json.NewObject("command", decideIn(request))), sure),
				call("pre_tool_use", "claude", with(base, "tool_name", "Edit", "tool_use_id", "u2",
					"tool_input", json.NewObject("file_path", "parser.py")), none),
			)
		}
		steps = append(steps, installation("claude", "review_mode", "availability"))
		base := json.NewObject("session_id", "s2", "transcript_path", filepath.Join(work, "missing.jsonl"), "cwd", work)
		return append(steps,
			call("pre_tool_use", "claude", with(base, "tool_name", "Bash", "tool_use_id", "u3",
				"tool_input", json.NewObject("command", decideIn(request))), sure),
			call("pre_tool_use", "claude", with(base, "tool_name", "Edit", "tool_use_id", "u4",
				"tool_input", json.NewObject("file_path", "parser.py")), none),
			call("pre_tool_use", "claude", with(base, "tool_name", "ExitPlanMode", "tool_use_id", "u5",
				"tool_input", json.NewObject("plan", "x")), none),
		)
	}
	codex := func(work string) []replayStep {
		main := filepath.Join(work, "codex.jsonl")
		base := json.NewObject("session_id", "c1", "transcript_path", main, "cwd", work)
		decided := json.NewObject("cmd", decideIn(request))
		patchText := "*** Begin Patch\n*** Update File: parser.py\n@@\n-    return text.split()\n+    return text.split(None)\n*** End Patch"
		return []replayStep{
			call("session_start", "codex", base, none),
			call("user_prompt_submit", "codex", with(base, "prompt", "Keep the cache small."), none),
			call("pre_tool_use", "codex", with(base, "tool_name", "exec_command", "tool_use_id", "x1",
				"tool_input", decided), sure),
			complete("codex", main, "x1", "cmd", decided),
			call("pre_tool_use", "codex", with(base, "tool_name", "exec_command", "tool_use_id", "x1",
				"tool_input", decided), sure),
			call("pre_tool_use", "codex", with(base, "tool_name", "apply_patch", "tool_use_id", "x2",
				"tool_input", json.NewObject("input", patchText)), none),
			call("pre_tool_use", "codex", with(base, "tool_name", "apply_patch", "tool_use_id", "x3",
				"tool_input", patchText), answers("needs_decision", 1.0)),
			call("pre_tool_use", "codex", with(base, "tool_name", "shell", "tool_use_id", "x4",
				"tool_input", json.NewObject("command", []any{"bash", "-lc", "ts-decide '{}' && ls"})), none),
			call("pre_tool_use", "codex", with(base, "tool_name", "ExitPlanMode", "tool_use_id", "x5",
				"tool_input", json.NewObject("plan", "x")), none),
			call("pre_tool_use", "codex", with(base, "tool_name", "local_shell", "tool_use_id", "x6",
				"tool_input", json.NewObject("cmd", "cat ~/.local/state/agent-decisions/ledger.key")), none),
			call("stop", "codex", base, none),
		}
	}
	native := func(work string) []replayStep {
		edit := func(session, path, id string) *json.Object {
			return json.NewObject("session_id", session, "transcript_path", filepath.Join(work, path), "cwd", work,
				"tool_name", "apply_patch", "tool_use_id", id,
				"tool_input", json.NewObject("input", "*** Begin Patch\n*** End Patch"))
		}
		return []replayStep{
			installation("codex", "session_review_exceptions", []any{"approved-sdk"}),
			call("pre_tool_use", "codex", edit("approved-sdk", "native.jsonl", "n1"), answers("needs_decision", 1.0)),
			call("pre_tool_use", "codex", edit("claimed", "native.jsonl", "n2"), answers("needs_decision", 1.0)),
			call("pre_tool_use", "codex", edit("approved-sdk", "child.jsonl", "n3"), answers("needs_decision", 1.0)),
			call("session_start", "codex", edit("approved-sdk", "native.jsonl", "n4"), none),
			call("user_prompt_submit", "codex", with(edit("approved-sdk", "native.jsonl", "n5"), "prompt", "go on"), none),
		}
	}
	malformedPayloads := func(work string) []replayStep {
		main := filepath.Join(work, "s1.jsonl")
		return []replayStep{
			call("pre_tool_use", "claude", []any{}, none),
			call("session_start", "claude", "text", none),
			call("pre_tool_use", "claude", json.NewObject("tool_name", []any{"Edit"}), none),
			call("pre_tool_use", "claude", json.NewObject("tool_name", json.IntOf(7), "tool_input", nil), none),
			call("pre_tool_use", "claude", json.NewObject("session_id", json.IntOf(5), "transcript_path", main,
				"cwd", work, "tool_name", "Edit", "tool_input", json.NewObject("file_path", "parser.py")), none),
			call("pre_tool_use", "claude", json.NewObject("session_id", "s3", "transcript_path", main,
				"cwd", json.IntOf(1), "tool_name", "Edit", "tool_input", json.NewObject("file_path", "parser.py")), none),
			call("pre_tool_use", "claude", json.NewObject("session_id", "s3", "cwd", work,
				"tool_name", "Edit", "tool_input", json.NewObject("file_path", "parser.py")), none),
			call("user_prompt_submit", "claude", json.NewObject("session_id", "s3", "prompt", json.IntOf(1)), none),
			call("user_prompt_submit", "claude", json.NewObject("prompt", "no session"), none),
			call("pre_tool_use", "claude", json.NewObject("session_id", "s3", "tool_name", "Bash", "tool_use_id", false,
				"tool_input", json.NewObject("command", "cat ~/.local/state/agent-decisions/x")), none),
		}
	}
	return []replayCase{
		{"claude", oracleFiles, claude},
		{"claude subagent", oracleFiles, subagent},
		{"unreadable transcripts", oracleFiles, unreadable},
		{"codex", oracleFiles, codex},
		{"codex native identity", oracleFiles, native},
		{"malformed payloads", oracleFiles, malformedPayloads},
	}
}

// TestHandleMatchesPython replays the corpus through hook.py handle and Go Handle.
func TestHandleMatchesPython(t *testing.T) {
	for _, c := range replayCases(t) {
		t.Run(c.name, func(t *testing.T) {
			work, python := oracleSetUp(t)
			replay(t, python, work, c)
		})
	}
}

func TestPythonReplayPreservesSourcePaths(t *testing.T) {
	for name, run := range map[string]func(*testing.T, string, []replayStep) []*stepResult{
		"oracle": runPython, "private replay": runPythonQuietly,
	} {
		t.Run(name, func(t *testing.T) {
			work, python := oracleSetUp(t)
			source := oracletest.Source(t)
			path := writeJSONL(t, work, "transcript.jsonl", []*json.Object{human("Inspect the source checkout.")}, "")
			base := json.NewObject("session_id", "paths", "cwd", work, "transcript_path", path)
			steps := []replayStep{
				call("session_start", "claude", base, replayFake{}),
				call("pre_tool_use", "claude", with(base, "tool_name", "ExitPlanMode", "tool_use_id", "plan",
					"tool_input", json.NewObject("plan", "Inspect "+source+"/notes.")), replayFake{}),
			}
			resetState(t)
			results := run(t, python, steps)
			if results[0].out == nil || !strings.Contains(*results[0].out, filepath.Join(repoRoot, "bin", "ts-decide")) {
				t.Fatal("SessionStart instructions do not use the shared code root")
			}
			if len(results[1].requests) != 1 || !strings.Contains(results[1].requests[0], source+"/notes.") {
				t.Fatal("replay changed the source path in the plan request")
			}
		})
	}
}

// TestMainMatchesPython runs hooks/hook.py and the agent-decisions binary end to end on the same stdin and
// compares exit status, stdout, stderr and the ledger. Only retired argparse presentation is normalized.
func TestMainMatchesPython(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		stdin  string
		env    []string
		policy bool // a signed availability policy for claude
	}{
		{"malformed stdin", []string{"pre_tool_use", "--host", "claude"}, "not json", nil, false},
		{"empty stdin", []string{"session_start", "--host", "codex"}, "", nil, false},
		{"off switch", []string{"pre_tool_use", "--host", "claude"}, "not json", []string{"AGENT_DECISIONS_OFF=1"}, false},
		{"off switch silent", []string{"stop", "--host", "claude"}, "{}", []string{"AGENT_DECISIONS_OFF=1"}, false},
		{"stop error", []string{"stop", "--host", "codex"}, "[]", nil, false},
		{"subagent stop error", []string{"subagent_stop", "--host", "claude"}, "7", nil, false},
		{"prompt error", []string{"user_prompt_submit", "--host", "claude"}, `"x"`, nil, false},
		{"availability error", []string{"pre_tool_use", "--host", "claude"}, "[]", nil, true},
		{"deep nesting", []string{"pre_tool_use", "--host", "claude"}, strings.Repeat("[", 200_000), nil, false},
		{"type error", []string{"pre_tool_use", "--host", "claude"}, `{"tool_name": ["Edit"]}`, nil, false},
		{"bad utf8 prompt", []string{"user_prompt_submit", "--host", "claude"},
			`{"session_id": "s1", "tool_use_id": "t1", "prompt": "Keep \ud800 the ` + "\xff" + ` cache."}`, nil, false},
		{"guard", []string{"pre_tool_use", "--host", "codex"},
			`{"session_id": "s1", "tool_use_id": "t1", "tool_name": "exec_command", "tool_input": {"cmd": "ls ~/.local/state/agent-decisions"}}`,
			nil, false},
		{"unknown event", []string{"bogus", "--host", "claude"}, "{}", nil, false},
		{"missing host", []string{"stop"}, "{}", nil, false},
		{"invalid host", []string{"stop", "--host", "both"}, "{}", nil, false},
		{"help", []string{"-h"}, "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			work, python := oracleSetUp(t)
			env := append([]string{"COLUMNS=100"}, c.env...)
			run := func(name string, args ...string) (result, map[string][]byte) {
				state := resetState(t)
				if c.policy {
					availability(t, "claude")
				}
				command := runProgram(t, []byte(c.stdin), env, name, args...)
				return command, snapshot(t, state)
			}
			py, pyFiles := run(python, append([]string{"-I", "-B", filepath.Join(oracletest.Source(t), "hooks", "hook.py")}, c.args...)...)
			golang, goFiles := run(binary, append([]string{"hook"}, c.args...)...)
			if c.name == "help" || py.code == 2 && golang.code == 2 {
				py.stdout, golang.stdout = oracletest.CLIPresentation(py.stdout), oracletest.CLIPresentation(golang.stdout)
				py.stderr, golang.stderr = oracletest.CLIPresentation(py.stderr), oracletest.CLIPresentation(golang.stderr)
			}
			if py.code != golang.code || py.stdout != golang.stdout {
				t.Errorf("python exit %d stdout %q\ngo     exit %d stdout %q", py.code, py.stdout, golang.code, golang.stdout)
			}
			// A Python traceback never reaches stderr: main catches every Exception.
			if py.stderr != golang.stderr {
				t.Errorf("stderr\npython %q\ngo     %q", py.stderr, golang.stderr)
			}
			pn, gn := &normalizer{map[string]string{}}, &normalizer{map[string]string{}}
			compareStates(t, pyFiles, goFiles, pn, gn)
			_ = work
		})
	}
}
