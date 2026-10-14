package hook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// repoRoot is the repository root: tests/helpers.py's ROOT.
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}()

// binary is the agent-decisions binary that TestMain builds; the Python tests ran hooks/hook.py instead.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-decisions-hook-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "bin", "agent-decisions")
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, "./cmd/agent-decisions")
	build.Dir, build.Env = repoRoot, append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build agent-decisions: %v\n%s", err, output)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// setUp ports tests/helpers.py StateTestCase.setUp and returns its directory. HOME is a private directory
// too, so that no test resolves paths under the real ~/.local/state/agent-decisions. The hook's code root
// is the repository, as it is for agent_decisions/hook.py.
func setUp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "Users", "me")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	t.Setenv("TYPESAFE_API_KEY", "test-key-0123456789abcdef")
	for _, name := range []string{"AGENT_DECISIONS_OFF", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	patch(t, &moduleFile, func() (string, error) { return filepath.Join(repoRoot, "bin", "agent-decisions"), nil })
	return dir
}

// patch sets *target to value for the duration of the test.
func patch[T any](t *testing.T, target *T, value T) {
	t.Helper()
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

// availability ports StateTestCase.availability: a signed policy that lets host fail open.
func availability(t *testing.T, host string) {
	t.Helper()
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation",
		"action", "configured", "spec", json.NewObject("host", host, "review_mode", "availability"))); err != nil {
		t.Fatal(err)
	}
}

// --- JSON values

func dumps(t *testing.T, v any) string {
	t.Helper()
	text, err := json.Dumps(v, json.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func loads(t *testing.T, text string) any {
	t.Helper()
	value, err := json.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// obj decodes a JSON object literal.
func obj(t *testing.T, text string) *json.Object {
	t.Helper()
	return loads(t, text).(*json.Object)
}

func equal(t *testing.T, a, b any) bool {
	t.Helper()
	same, err := json.Equal(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return same
}

func assertEqual(t *testing.T, got, want any) {
	t.Helper()
	if !equal(t, got, want) {
		t.Fatalf("got %s, want %s", dumps(t, got), dumps(t, want))
	}
}

// merge returns {**base, **over...}.
func merge(base *json.Object, over ...*json.Object) *json.Object {
	result := base.Copy()
	for _, object := range over {
		for _, key := range object.Keys() {
			result.Set(key, object.Value(key))
		}
	}
	return result
}

// with returns {**base, key: value, ...} for alternating keys and values.
func with(base *json.Object, pairs ...any) *json.Object {
	return merge(base, json.NewObject(pairs...))
}

// at follows keys through nested dicts and lists: at(v, "a", 0, "b") is v["a"][0]["b"].
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := v.(*json.Object)
			if !ok {
				t.Fatalf("%s is not a dict at %q", dumps(t, v), key)
			}
			value, ok := object.Get(key)
			if !ok {
				t.Fatalf("no key %q in %s", key, dumps(t, v))
			}
			v = value
		case int:
			list, ok := v.([]any)
			if !ok || key >= len(list) {
				t.Fatalf("no item %d in %s", key, dumps(t, v))
			}
			v = list[key]
		}
	}
	return v
}

// str returns at(...) as a string.
func str(t *testing.T, v any, path ...any) string {
	t.Helper()
	value := at(t, v, path...)
	text, ok := value.(string)
	if !ok {
		t.Fatalf("%s is not a str", dumps(t, value))
	}
	return text
}

func assertContains(t *testing.T, text, part string) {
	t.Helper()
	if !strings.Contains(text, part) {
		t.Fatalf("%q does not contain %q", text, part)
	}
}

func assertNotContains(t *testing.T, text, part string) {
	t.Helper()
	if strings.Contains(text, part) {
		t.Fatalf("%q contains %q", text, part)
	}
}

// --- tests/helpers.py record builders

func human(text string) *json.Object {
	return json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", text),
		"origin", json.NewObject("kind", "human"))
}

func peer(text string) *json.Object {
	return json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", text),
		"origin", json.NewObject("kind", "peer"))
}

func sidechainUser(text string) *json.Object {
	return json.NewObject("type", "user", "isSidechain", true, "agentId", "a1",
		"message", json.NewObject("role", "user", "content", text))
}

func toolUse(callID, name string, toolInput any) *json.Object {
	return json.NewObject("type", "assistant", "message", json.NewObject("role", "assistant", "content", []any{
		json.NewObject("type", "tool_use", "id", callID, "name", name, "input", toolInput)}))
}

func toolResult(callID string, content any) *json.Object {
	return json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", []any{
		json.NewObject("type", "tool_result", "tool_use_id", callID, "content", content)}))
}

func assistantText(text string) *json.Object {
	return json.NewObject("type", "assistant", "message", json.NewObject("role", "assistant",
		"content", []any{json.NewObject("type", "text", "text", text)}))
}

// queued ports helpers.queued with kind="human", human_turn=True, mode="prompt" unless given.
func queued(text, kind string, humanTurn bool, mode string) *json.Object {
	attachment := json.NewObject("type", "queued_command", "commandMode", mode, "prompt", text,
		"origin", json.NewObject("kind", kind))
	if humanTurn {
		attachment.Set("humanTurn", true)
	}
	return json.NewObject("type", "attachment", "attachment", attachment)
}

func ask(callID string, questions ...string) *json.Object {
	items := make([]any, len(questions))
	for i, q := range questions {
		items[i] = json.NewObject("question", q, "header", "Choice", "multiSelect", false, "options", []any{
			json.NewObject("label", "Keep it", "description", ""), json.NewObject("label", "Drop it", "description", "")})
	}
	return toolUse(callID, "AskUserQuestion", json.NewObject("questions", items))
}

// answered ports helpers.answered; an empty source is Python's None.
func answered(callID string, answers *json.Object, source string) *json.Object {
	record := toolResult(callID, "Your questions have been answered.")
	record.Set("toolUseResult", json.NewObject("questions", []any{}, "answers", answers, "annotations", &json.Object{}))
	if source != "" {
		record.Set("permissionDecision", json.NewObject("decision", "accept", "source", source))
	}
	return record
}

func codexUser(text string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject("type", "message", "role", "user",
		"content", []any{json.NewObject("type", "input_text", "text", text)}))
}

func codexAssistant(text string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject("type", "message", "role", "assistant",
		"content", []any{json.NewObject("type", "output_text", "text", text)}))
}

func codexCall(callID, name, arguments string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject("type", "function_call",
		"id", "fc_"+callID, "name", name, "arguments", arguments, "call_id", callID))
}

func codexCustom(callID, name, toolInput string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject("type", "custom_tool_call",
		"id", "ctc_"+callID, "status", "completed", "call_id", callID, "name", name, "input", toolInput))
}

func codexOutput(callID string, output any) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject("type", "function_call_output",
		"call_id", callID, "output", output))
}

// writeJSONL ports helpers.write_jsonl and returns the path.
func writeJSONL(t *testing.T, directory, name string, records []*json.Object, tail string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, record := range records {
		text.WriteString(dumps(t, record) + "\n")
	}
	if err := os.WriteFile(path, []byte(text.String()+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// --- tests/helpers.py FakeJev, make_ctx and asker

// fake ports FakeJev(**answers).
func fake(answers map[string]any) *jevtest.Fake {
	return &jevtest.Fake{Answers: answers}
}

// confident is test_hook.py's CONFIDENT.
func confident() *jevtest.Fake {
	return fake(map[string]any{"pick": "lfu", "sufficient": 0.9, "owner": "agent"})
}

type ctxOptions struct {
	transcript *transcript.Transcript
	entries    []*json.Object
	agentID    any
	host       string
	toolUseID  string
}

// makeCtx ports tests/helpers.py make_ctx.
func makeCtx(t *testing.T, o ctxOptions) gates.Context {
	t.Helper()
	tr := transcript.Transcript{UserMessages: []string{"decide the cache policy"}}
	if o.transcript != nil {
		tr = *o.transcript
	}
	host := o.host
	if host == "" {
		host = "claude"
	}
	toolUseID := o.toolUseID
	if toolUseID == "" {
		toolUseID = "toolu_now"
	}
	calls := map[string]transcript.ToolCall{}
	for _, call := range tr.ToolCalls {
		calls[call.ID] = call
	}
	var records []*json.Object
	for _, entry := range o.entries {
		id, _ := entry.Value("tool_use_id").(string)
		if call, ok := calls[id]; ok && entry.Value("gate") == "decide" {
			records = append(records, merge(json.NewObject("host", host, "verdict", call.Output), entry))
		} else {
			records = append(records, merge(json.NewObject("host", host), entry))
		}
	}
	bundle, err := evidence.Build(tr, nil, evidence.Options{Frame: 3})
	if err != nil {
		t.Fatal(err)
	}
	return gates.Context{Host: host, SessionID: "s1", AgentID: o.agentID, ToolUseID: toolUseID, Transcript: tr,
		Evidence: bundle, Entries: records}
}

// gatesAsker ports tests/helpers.py asker.
func gatesAsker(f *jevtest.Fake) gates.Ask {
	return func(state, questions *json.Object) (*json.Object, error) {
		return jev.Ask(state, questions, "k", f.Post, nil)
	}
}

// --- hook calls

// handle runs Handle and fails the test on an error.
func handle(t *testing.T, event string, payload *json.Object, host string, post jev.PostFunc) *json.Object {
	t.Helper()
	out, err := Handle(event, payload, host, post)
	if err != nil {
		t.Fatalf("Handle(%s): %v", event, err)
	}
	return out
}

// decision returns out["hookSpecificOutput"]["permissionDecision"].
func decision(t *testing.T, out *json.Object) string {
	t.Helper()
	return str(t, out, "hookSpecificOutput", "permissionDecision")
}

// reason returns out["hookSpecificOutput"]["permissionDecisionReason"].
func reason(t *testing.T, out *json.Object) string {
	t.Helper()
	return str(t, out, "hookSpecificOutput", "permissionDecisionReason")
}

// runShell runs a command with /bin/sh in dir and returns its stdout.
func runShell(t *testing.T, dir, command string) string {
	t.Helper()
	shell := exec.Command("/bin/sh", "-c", command)
	shell.Dir = dir
	output, err := shell.Output()
	if err != nil {
		t.Fatalf("%s: %v", command, err)
	}
	return string(output)
}

// verdictOutput runs the rewritten ts-decide command of an allow response and returns its stdout.
func verdictOutput(t *testing.T, dir string, out *json.Object, key string) string {
	t.Helper()
	return runShell(t, dir, str(t, out, "hookSpecificOutput", "updatedInput", key))
}

// entries returns the session's verified ledger rows.
func entries(t *testing.T, session string) []*json.Object {
	t.Helper()
	rows, err := ledger.Entries(session, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// gateRows returns the session's rows of one gate.
func gateRows(t *testing.T, session, gate string) []*json.Object {
	t.Helper()
	var rows []*json.Object
	for _, row := range entries(t, session) {
		if row.Value("gate") == gate {
			rows = append(rows, row)
		}
	}
	return rows
}

func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := policy.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// result is a finished run of the binary.
type result struct {
	code   int
	stdout string
	stderr string
}

// runBinary runs the agent-decisions binary with stdin and extra environment, as ScriptTest.run_script
// ran hooks/hook.py.
func runBinary(t *testing.T, stdin []byte, env []string, args ...string) result {
	t.Helper()
	return runProgram(t, stdin, env, binary, args...)
}

// runProgram runs a program with stdin and extra environment.
func runProgram(t *testing.T, stdin []byte, env []string, name string, args ...string) result {
	t.Helper()
	command := exec.Command(name, args...)
	command.Stdin = strings.NewReader(string(stdin))
	command.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return result{command.ProcessState.ExitCode(), stdout.String(), stderr.String()}
}
