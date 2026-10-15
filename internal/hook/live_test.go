package hook

// Live hook cases against the real TypeSafe Jev API. They port tests/live_scoped_review.py, a scoped review
// with controlled native-shaped Codex fixtures, and run only with AGENT_DECISIONS_LIVE=1:
//
//	AGENT_DECISIONS_LIVE=1 go test -v -run Live ./internal/hook
//
// Jev is a model, so a verdict can change between runs.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

const leaseSource = `class Lease:
    def __init__(self): self.active = True
    @property
    def data(self):
        if not self.active: raise RuntimeError("released input")
        return "input"
    def release(self): self.active = False
    def __enter__(self): return self
    def __exit__(self, *args): self.release()
class Owner:
    def acquire(self): return Lease()
def decode(data): return data
`

// permission returns the hook's permissionDecision, or "" for no output or no decision.
func permission(out *json.Object) string {
	if out == nil {
		return ""
	}
	specific, _ := out.Value("hookSpecificOutput").(*json.Object)
	if specific == nil {
		return ""
	}
	value, _ := specific.Value("permissionDecision").(string)
	return value
}

// verdictOf returns the hook's permission decision ("none" for no output) and whether it is not a denial,
// which is what live_scoped_review.py accepts for an edit.
func verdictOf(t *testing.T, out *json.Object) (string, string, bool) {
	actual := permission(out)
	if actual == "" {
		actual = "none"
	}
	if actual == "deny" {
		t.Logf("response: %s", dumps(t, out))
	}
	return actual, "", actual != "deny"
}

// py: tests/live_scoped_review.py
func TestLiveScopedReview(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_LIVE") != "1" {
		t.Skip("set AGENT_DECISIONS_LIVE=1 to run against the real Jev API")
	}
	// The hook reads the key itself. Read it before setUp replaces HOME, which hides the key file, and hand
	// it to the hook through this process's environment only.
	key, err := jev.LoadKey()
	if err != nil {
		t.Fatalf("load the TypeSafe API key: %v", err)
	}
	root := setUp(t)
	t.Setenv("TYPESAFE_API_KEY", key)

	var calls atomic.Int64
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		calls.Add(1)
		return jev.Post(body, key, timeout)
	}
	passed, total := 0, 0
	t.Cleanup(func() { t.Logf("passed %d/%d, Jev calls %d", passed, total, calls.Load()) })
	// check runs one case; run returns its actual outcome and whether the case passed.
	check := func(name, expected string, run func(t *testing.T) (actual, receipt string, ok bool)) bool {
		total++
		return t.Run(name, func(t *testing.T) {
			started, before := time.Now(), calls.Load()
			actual, receipt, ok := run(t)
			if receipt == "" {
				receipt = "-"
			}
			t.Logf("%s: expected=%s actual=%s %.2fs receipt=%s calls=%d", name, expected, actual,
				time.Since(started).Seconds(), receipt, calls.Load()-before)
			if !ok {
				t.Fail()
				return
			}
			passed++
		})
	}

	writeFile(t, filepath.Join(root, "lease.py"), leaseSource)
	source := filepath.Join(root, "parser.py")
	writeFile(t, source, "from lease import decode\ndef parse(owner):\n    lease = owner.acquire()\n"+
		"    lease.release()\n    return decode(lease.data)\n")
	user := "Choose a lease handling control structure for the parser. Keep the input lease active while decode reads its data."
	rows := []*json.Object{rcNativeMetadata("live-parent", "live-parent", root, "", ""), codexUser(user)}
	parentPath := writeJSONL(t, root, "parent.jsonl", rows, "")
	request := obj(t, `{"decision": "Which control structure should parser.py use with the Lease API in lease.py?",
		"options": [{"id": "finally", "description": "Use parse with try/finally to release the acquired lease after decode."},
			{"id": "with", "description": "Use parse with the Lease context manager to release the acquired lease after decode."}],
		"scope": {"paths": ["parser.py", "tests/test_parser.py"],
			"invariants": ["The input lease stays active while decode reads its data."]}}`)
	command := "ts-decide <<'EOF'\n" + dumps(t, request) + "\nEOF"
	payload := json.NewObject("session_id", "live-parent", "tool_use_id", "review", "tool_name", "Bash",
		"tool_input", json.NewObject("command", command), "transcript_path", parentPath, "cwd", root)

	var receipt *json.Object
	ready := check("real scoped invariant review", "advisory with a receipt", func(t *testing.T) (string, string, bool) {
		rendered := verdictOutput(t, root, handle(t, "pre_tool_use", payload, "codex", post), "command")
		rows = append(rows, rcCodexCall("review", "Bash", json.NewObject("command", command)),
			codexOutput("review", rendered))
		writeJSONL(t, root, "parent.jsonl", rows, "")
		for _, row := range rcContext(t, with(payload, "tool_use_id", "context"), "codex").Entries {
			if row.Value("gate") == "decide" {
				receipt = row
				break
			}
		}
		if receipt == nil {
			t.Fatalf("no decide row; verdict:\n%s", rendered)
		}
		action, _ := receipt.Value("action").(string)
		id, _ := receipt.Value("receipt_id").(string)
		t.Logf("pick=%v eligible_options=%s", receipt.Value("pick"), dumps(t, receipt.Value("eligible_options")))
		ok := action == "advisory" && id != ""
		if !ok {
			t.Logf("verdict:\n%s", rendered)
		}
		return action, id, ok
	})
	if !ready {
		return
	}

	receiptID, pick := receipt.Value("receipt_id").(string), receipt.Value("pick").(string)
	marker := "Implement the accepted parse control structure, then correct its test assertions.\nDecision receipt: " +
		receiptID + "\nAccepted option: " + pick
	rows = append(rows, rcCodexCall("spawn", "spawn_agent", json.NewObject("message", marker, "task_name", "worker")),
		codexOutput("spawn", dumps(t, json.NewObject("task_name", "/root/worker"))))
	writeJSONL(t, root, "parent.jsonl", rows, "")
	childPath := writeJSONL(t, root, "child.jsonl", []*json.Object{
		rcNativeMetadata("live-child", "live-parent", root, "live-parent", "/root/worker"),
		codexUser("<codex_delegation>" + marker + "</codex_delegation>")}, "")
	changed := "from lease import decode\ndef parse(owner):\n    with owner.acquire() as lease:\n        return decode(lease.data)\n"
	if pick == "finally" {
		changed = "from lease import decode\ndef parse(owner):\n    lease = owner.acquire()\n    try:\n" +
			"        return decode(lease.data)\n    finally:\n        lease.release()\n"
	}
	edit := json.NewObject("session_id", "live-child", "tool_use_id", "edit", "tool_name", "Edit",
		"tool_input", json.NewObject("file_path", source, "old_string", readFile(t, source), "new_string", changed),
		"transcript_path", childPath, "cwd", root)

	check("native-shaped parent receipt inheritance", "1 inherited entry", func(t *testing.T) (string, string, bool) {
		inherited := len(rcContext(t, edit, "codex").InheritedEntries)
		return strconv.Itoa(inherited) + " inherited", "", inherited == 1
	})
	if !check("real inherited scoped implementation", "not deny", func(t *testing.T) (string, string, bool) {
		out := handle(t, "pre_tool_use", edit, "codex", post)
		return verdictOf(t, out)
	}) {
		return
	}
	writeFile(t, source, changed)
	check("fixture implementation executes", "exit 0", func(t *testing.T) (string, string, bool) {
		program := exec.Command("python3", "-B", "-c",
			"from parser import parse; from lease import Owner; assert parse(Owner()) == 'input'")
		program.Dir = root
		output, err := program.CombinedOutput()
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("python3 is not installed")
		}
		if err != nil {
			t.Logf("%v\n%s", err, output)
			return "exit " + strconv.Itoa(program.ProcessState.ExitCode()), "", false
		}
		return "exit 0", "", true
	})
	check("routine assertion correction after source changed", "not deny", func(t *testing.T) (string, string, bool) {
		out := handle(t, "pre_tool_use", with(edit, "tool_use_id", "test-edit", "tool_input", json.NewObject(
			"file_path", filepath.Join(root, "tests/test_parser.py"),
			"old_string", "assert result == None", "new_string", `assert result == "input"`)), "codex", post)
		return verdictOf(t, out)
	})

	// live_scoped_review.py also fails on any Jev error that the ledger metrics record.
	recorded := 0
	for _, row := range entries(t, "live-parent") {
		metrics, _ := row.Value("metrics").(*json.Object)
		if metrics == nil {
			continue
		}
		apiCalls, _ := metrics.Value("api_calls").([]any)
		for _, item := range apiCalls {
			recorded++
			if failure := item.(*json.Object).Value("error"); failure != nil {
				t.Errorf("Jev error in the ledger: %v", failure)
			}
		}
	}
	t.Logf("ledger api_calls %d", recorded)
}
