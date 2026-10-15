package ledger

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// oracleScript runs a list of ledger operations with the Python ledger and prints one outcome per
// operation: ["ok", json.dumps(result)] or [error class, str(error)]. The request and the reply are
// ASCII JSON, so lone surrogates cross as \udXXX escapes.
const oracleScript = `
import json, os, sys
sys.path.insert(0, sys.argv[1])
from agent_decisions import ledger

def run(op, state, *args):
    os.environ["AGENT_DECISIONS_STATE"] = state
    if op == "append":
        return ledger.append(args[0])
    if op == "entries":
        return ledger.entries(args[0], cache_limit=args[1])
    if op == "remember":
        return ledger.remember_architecture(args[0], args[1])
    if op == "architecture_entries":
        return ledger.architecture_entries(args[0])
    if op == "architecture_path":
        return os.path.relpath(ledger.architecture_path(args[0]), state)
    if op == "write_verdict":
        path = ledger.write_verdict(args[0], args[1])
        return [os.path.relpath(path, state), path.read_bytes().hex()]
    raise AssertionError(op)

replies = []
for op in json.load(open(sys.argv[2], encoding="ascii")):
    try:
        replies.append(["ok", json.dumps(run(*op), ensure_ascii=True)])
    except Exception as error:
        replies.append([type(error).__name__, str(error)])
print(json.dumps(replies, ensure_ascii=True))
`

// runPython runs ops through the Python ledger. It skips the test when AGENT_DECISIONS_PYTHON is unset.
func runPython(t *testing.T, ops [][]any) []any {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot := oracletest.Source(t)
	request := filepath.Join(t.TempDir(), "ops.json")
	if err := os.WriteFile(request, []byte(dumps(t, toAny(ops), json.Options{})), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-I", "-c", oracleScript, repoRoot, request)
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("python oracle: %v", err)
	}
	replies, err := json.Loads(string(output))
	if err != nil {
		t.Fatalf("python oracle reply: %v", err)
	}
	return replies.([]any)
}

// runGo runs ops through this package, with the same outcome shape as the oracle.
func runGo(t *testing.T, ops [][]any) []any {
	t.Helper()
	var replies []any
	for _, op := range ops {
		state := op[1].(string)
		t.Setenv("AGENT_DECISIONS_STATE", state)
		var result any
		var err error
		switch op[0] {
		case "append":
			err = Append(op[2].(*json.Object))
		case "entries":
			var cacheLimit *int
			if n, isInt := op[3].(json.Int); isInt {
				value, _ := n.Int64()
				cacheLimit = limit(int(value))
			}
			var rows []*json.Object
			rows, err = Entries(op[2].(string), cacheLimit)
			result = toAny(rows)
		case "remember":
			var decisions []*json.Object
			for _, decision := range op[3].([]any) {
				decisions = append(decisions, decision.(*json.Object))
			}
			err = RememberArchitecture(op[2].(string), decisions)
		case "architecture_entries":
			var rows []*json.Object
			rows, err = ArchitectureEntries(op[2].(string))
			result = toAny(rows)
		case "architecture_path":
			var path string
			path, err = ArchitecturePath(op[2].(string))
			result = strings.TrimPrefix(path, state+"/")
		case "write_verdict":
			var path string
			path, err = WriteVerdict(op[2].(string), op[3].(string))
			if err == nil {
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				result = []any{strings.TrimPrefix(path, state+"/"), hex.EncodeToString(data)}
			}
		default:
			t.Fatalf("unknown op %v", op[0])
		}
		if err != nil {
			pyErr, isPy := err.(interface{ PyType() string })
			if !isPy {
				t.Fatalf("%v: error without a Python class: %v", op[0], err)
			}
			replies = append(replies, []any{pyErr.PyType(), err.Error()})
		} else {
			replies = append(replies, []any{"ok", dumps(t, result, json.Options{})})
		}
	}
	return replies
}

func toAny[T any](items []T) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// compareOutcomes runs ops through both ledgers and fails on the first differing outcome.
func compareOutcomes(t *testing.T, ops [][]any, pyOps [][]any) {
	t.Helper()
	want := runPython(t, pyOps)
	got := runGo(t, ops)
	if len(want) != len(got) {
		t.Fatalf("python gave %d outcomes, go %d", len(want), len(got))
	}
	for i := range got {
		if dumps(t, got[i], json.Options{}) != dumps(t, want[i], json.Options{}) {
			t.Errorf("op %d %v:\n go %s\n py %s", i, ops[i][0], dumps(t, got[i], json.Options{}),
				dumps(t, want[i], json.Options{}))
		}
	}
}

// withState returns ops with every state path moved from one root to another.
func withState(ops [][]any, from, to string) [][]any {
	moved := make([][]any, len(ops))
	for i, op := range ops {
		moved[i] = append([]any{op[0], strings.Replace(op[1].(string), from, to, 1)}, op[2:]...)
	}
	return moved
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// rowEntries are ledger entries that stress the canonical JSON: non-ASCII text and keys, lone and paired
// surrogates, floats at the edges of repr, integers beyond int64, nesting, control characters and a
// duplicate key.
var rowEntries = []string{
	`{"session_id": "s", "gate": "decide", "text": "h\u00e9llo \u2014 \u65e5\u672c \ud83c\udf89",
	  "\u00e9": 1, "e": 2, "e\u0301": 3, "Z": 4, "\ud83c\udf89": 5}`,
	`{"session_id": "s", "gate": "edit", "lone": "\ud800 x \udfff", "reversed": "\udc00\ud800",
	  "state": {"x": 1}, "chunks": [1]}`,
	`{"session_id": "s", "gate": "stop", "floats": [0.1, 1e300, -0.0, 1.5e-7, 123456789.123, 5e-324,
	  1.7976931348623157e308, 1e16, 1e-5, 0.0001, 2.5, NaN, Infinity, -Infinity, 1E400], "state": {"y": 2}}`,
	`{"session_id": "s", "gate": "plan", "big": 123456789012345678901234567890,
	  "neg": -98765432109876543210, "zero": -0, "state": 1}`,
	`{"session_id": "s", "nested": {"b": {"c": [{"z": 1, "a": [null, true, false, [], {}]}]}, "a": {}},
	  "control": "\u0000\u001f\t\n\r\"\\/\u007f\u0080\u2028\ufeff"}`,
	`{"session_id": "s", "gate": "question", "dup": 1, "dup": 2, "state": []}`,
}

// TestOracleRowsAreByteIdentical writes the same entries with both ledgers under one key, compares the
// files byte for byte, damages both files the same way, and reads each ledger's rows with the other.
func TestOracleRowsAreByteIdentical(t *testing.T) {
	root := t.TempDir()
	goRoot, pyRoot := filepath.Join(root, "go"), filepath.Join(root, "py")
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(goRoot, "rows"))
	var entries []*json.Object
	for _, text := range rowEntries {
		entries = append(entries, object(t, text))
	}
	mustAppend(t, object(t, `{"session_id": "seed"}`)) // creates the key that both state directories share
	shareKey(t, filepath.Join(goRoot, "rows"), filepath.Join(pyRoot, "rows"))

	var writes [][]any
	for _, entry := range entries {
		writes = append(writes, []any{"append", filepath.Join(goRoot, "rows"), entry})
	}
	compareOutcomes(t, writes, withState(writes, goRoot, pyRoot))

	goFile, pyFile := filepath.Join(goRoot, "rows/sessions/s.jsonl"), filepath.Join(pyRoot, "rows/sessions/s.jsonl")
	if written := readFile(t, goFile); !bytes.Equal(written, readFile(t, pyFile)) {
		t.Fatalf("rows differ:\n go %q\n py %q", written, readFile(t, pyFile))
	}

	lines := strings.SplitAfter(string(readFile(t, goFile)), "\n")
	damage := strings.Join([]string{
		`{"broken` + "\n",
		"\xff\xfe{\"entry\": 1}\n",
		"[1]\n", "\"str\"\n", "null\n",
		strings.Repeat("1", 5_000) + "\n",
		strings.TrimSuffix(lines[0], "\n") + "\r",
		strings.TrimSuffix(lines[1], "\n") + "\r\n",
		strings.Replace(lines[2], `"_ledger_seal": "hmac-sha256-v1:`, `"_ledger_seal": "hmac-sha256-v1:0`, 1),
		strings.Replace(lines[3], `"session_id": "s"`, "\"session_id\": \"s\xff\"", 1),
		`{"entry": {"session_id": "s"}, "_ledger_seal": 5}` + "\n",
		lines[4][:len(lines[4])/2],
	}, "")
	appendText(t, goFile, damage)
	appendText(t, pyFile, damage)

	var reads [][]any
	for _, cacheLimit := range []any{nil, json.IntOf(0), json.IntOf(1), json.IntOf(-1)} {
		reads = append(reads, []any{"entries", filepath.Join(pyRoot, "rows"), "s", cacheLimit})
	}
	compareOutcomes(t, reads, withState(reads, pyRoot, goRoot))
}

// TestOracleReadErrors compares rows that make Python's entries raise instead of skipping the line.
func TestOracleReadErrors(t *testing.T) {
	state := filepath.Join(t.TempDir(), "errors")
	t.Setenv("AGENT_DECISIONS_STATE", state)
	mustAppend(t, object(t, `{"session_id": "gatelist", "gate": ["edit"]}`))
	mustAppend(t, object(t, `{"session_id": "gatedict", "gate": {"edit": 1}}`))
	for session, row := range map[string]string{
		"nonascii":  `{"entry": {"session_id": "nonascii"}, "_ledger_seal": "\u00e9"}`,
		"surrogate": `{"entry": {"session_id": "surrogate"}, "_ledger_seal": "\ud800"}`,
		"elsewhere": `{"entry": {"session_id": "x"}, "_ledger_seal": "\u00e9"}`,
	} {
		if err := os.WriteFile(filepath.Join(state, "sessions", session+".jsonl"), []byte(row+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var ops [][]any
	for _, session := range []string{"gatelist", "gatedict", "nonascii", "surrogate", "elsewhere"} {
		ops = append(ops, []any{"entries", state, session, nil}, []any{"entries", state, session, json.IntOf(3)})
	}
	compareOutcomes(t, ops, ops)
}

// TestOracleArchitecture compares remember_architecture, its errors, and the files it writes.
func TestOracleArchitecture(t *testing.T) {
	root := t.TempDir()
	goState, pyState := filepath.Join(root, "go"), filepath.Join(root, "py")
	t.Setenv("AGENT_DECISIONS_STATE", goState)
	mustAppend(t, object(t, `{"session_id": "seed"}`))
	shareKey(t, goState, pyState)

	first := object(t, `{"action": "advisory", "contracts": ["c1"], "receipt_id": "r1", "repo_id": "repo",
		"time": "2026-10-14T00:00:00Z", "pick": "a", "state": {"options": {}, "scope": {"invariants": "abcdefghijk"}}}`)
	first.Set("decision", strings.Repeat("\u00e9\U0001F389", 300))
	state := first.Value("state").(*json.Object)
	state.Value("options").(*json.Object).Set("a", strings.Repeat("x\u2014", 600))
	var paths []any
	for i := range 10 {
		paths = append(paths, strings.Repeat(string(rune('a'+i)), 150*i))
	}
	paths[3] = []any{1, 2}
	state.Value("scope").(*json.Object).Set("paths", paths)

	batch := []any{first}
	for _, text := range []string{
		`{"action": "binding", "contracts": {"k": 1}, "receipt_id": "r2", "repo_id": "repo", "pick": "zz",
		  "state": {"options": {"a": "x"}}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "r1", "repo_id": "repo", "pick": "a"}`,
		`{"action": "refused", "contracts": ["c"], "receipt_id": "r3", "repo_id": "repo", "pick": "a"}`,
		`{"action": "advisory", "contracts": [], "receipt_id": "r4", "repo_id": "repo", "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "", "repo_id": "repo", "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "r5", "repo_id": "other", "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": 1, "repo_id": "repo", "pick": 1,
		  "decision": ["l1", "l2"], "state": {"options": {"1": "one"}}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": 1.0, "repo_id": "repo", "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": true, "repo_id": "repo", "pick": "a"}`,
	} {
		batch = append(batch, object(t, text))
	}
	ops := [][]any{
		{"remember", goState, "repo", batch},
		{"remember", goState, "repo", batch},
		{"remember", goState, "repo", []any{object(t, `{"action": "advisory", "contracts": []}`)}},
	}
	for _, text := range []string{
		`{"action": ["advisory"], "contracts": ["c"]}`,
		`{"action": {"advisory": 1}, "contracts": ["c"]}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": ["x"], "repo_id": "repo"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e1", "repo_id": "repo", "state": null, "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e2", "repo_id": "repo", "decision": 5, "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e3", "repo_id": "repo", "decision": {}, "pick": "a"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e4", "repo_id": "repo", "decision": null}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e5", "repo_id": "repo"}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e6", "repo_id": "repo", "pick": "a", "state": {"options": []}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e7", "repo_id": "repo", "pick": ["a"], "state": {"options": {}}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e8", "repo_id": "repo", "pick": ["a"], "state": {"options": 3}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e9", "repo_id": "repo", "pick": "a", "state": {"scope": {"paths": 5}}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e10", "repo_id": "repo", "pick": "a", "state": {"scope": {"paths": [{}]}}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e11", "repo_id": "repo", "pick": "a", "state": {"scope": []}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e12", "repo_id": "repo", "pick": "a", "state": {"options": {"a": 7}}}`,
		`{"action": "advisory", "contracts": ["c"], "receipt_id": "e13", "repo_id": "repo", "pick": "a", "state": {"scope": {"invariants": {"k": 1}}}}`,
	} {
		ops = append(ops, []any{"remember", goState, "repo", []any{object(t, text)}})
	}
	for _, repo := range []string{"repo", "\u00e9", wtf8(t, `\ud800`), wtf8(t, `a\udc80\udc81b\ud800`), wtf8(t, `x\udfffy`)} {
		ops = append(ops, []any{"architecture_path", goState, repo},
			[]any{"remember", goState, repo, []any{object(t, `{"contracts": ["c"]}`)}})
	}
	ops = append(ops, []any{"architecture_entries", goState, "repo"})
	compareOutcomes(t, ops, withState(ops, goState, pyState))

	path, err := ArchitecturePath("repo")
	if err != nil {
		t.Fatal(err)
	}
	relative := strings.TrimPrefix(path, goState)
	if written := readFile(t, path); !bytes.Equal(written, readFile(t, pyState+relative)) {
		t.Fatalf("architecture rows differ:\n go %q\n py %q", written, readFile(t, pyState+relative))
	}
}

// TestOracleVerdicts compares verdict paths and file bytes, including lone surrogates in the text.
func TestOracleVerdicts(t *testing.T) {
	state := filepath.Join(t.TempDir(), "verdicts")
	var ops [][]any
	for _, call := range [][2]string{
		{"../../etc/x", wtf8(t, `verdict \ud800 text \u00e9\n\udfff`)},
		{wtf8(t, `\u00e9/\ud83c\udf89 \ud800`), "plain"},
		{"", ""},
		{strings.Repeat("a", 200), "long id"},
	} {
		ops = append(ops, []any{"write_verdict", state, call[0], call[1]})
	}
	compareOutcomes(t, ops, ops)
}

// shareKey copies the ledger key of from into the state directory to.
func shareKey(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "ledger.key"), readFile(t, filepath.Join(from, "ledger.key")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// wtf8 returns the Python str that a JSON string body spells, so a Go test can hold lone surrogates.
func wtf8(t *testing.T, body string) string {
	t.Helper()
	value, err := json.Loads(`"` + body + `"`)
	if err != nil {
		t.Fatal(err)
	}
	return value.(string)
}
