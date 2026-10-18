package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
)

// hookRequest is test_hook.py's REQUEST.
const hookRequest = `{"decision": "Which cache?", "options": [{"id": "lru", "description": "LRU"}, {"id": "lfu", "description": "LFU"}]}`

// hookSetUp ports HookTest.setUp and returns the test directory and the transcript path.
func hookSetUp(t *testing.T) (string, string) {
	t.Helper()
	dir := setUp(t)
	path := writeJSONL(t, dir, "s1.jsonl", []*json.Object{
		human("speed up the tile cache"),
		toolUse("toolu_1", "Bash", json.NewObject("command", "trace")),
		toolResult("toolu_1", "tiles are re-read in bursts"),
	}, "")
	return dir, path
}

// hookPayload ports HookTest.payload.
func hookPayload(path, command string, extra ...any) *json.Object {
	return with(json.NewObject("session_id", "s1", "transcript_path", path, "tool_name", "Bash",
		"tool_use_id", "toolu_d", "tool_input", json.NewObject("command", command, "description", "decide")), extra...)
}

// decideCommand is f"ts-decide {shlex.quote(json.dumps(REQUEST))}".
func decideCommand(t *testing.T, request string) string {
	return "ts-decide " + shlex.Quote(dumps(t, loads(t, request)))
}

// py: tests/test_hook.py::test_claude_ts_decide_returns_literal_verdict_stdout
func TestClaudeTsDecideReturnsLiteralVerdictStdout(t *testing.T) {
	dir, path := hookSetUp(t)
	out := handle(t, "pre_tool_use", hookPayload(path, decideCommand(t, hookRequest)), "claude", confident().Post)
	if decision(t, out) != "allow" {
		t.Fatalf("got %s", dumps(t, out))
	}
	assertEqual(t, at(t, out, "hookSpecificOutput", "updatedInput", "description"), "decide")
	output := verdictOutput(t, dir, out, "command")
	assertContains(t, output, "ts-decide verdict: ADVISORY")
	rows := gateRows(t, "s1", "decide")
	if len(rows) != 1 {
		t.Fatalf("got %d decide rows", len(rows))
	}
	entry := rows[0]
	assertEqual(t, []any{entry.Value("gate"), entry.Value("action"), entry.Value("tool_use_id")},
		[]any{"decide", "advisory", "toolu_d"})
	assertEqual(t, entry.Value("host"), "claude")
	if entry.Value("turn") == nil {
		t.Fatal("turn is None")
	}
	if output != str(t, entry, "verdict")+"\n" {
		t.Fatalf("output %q is not the recorded verdict", output)
	}
}

// py: tests/test_hook.py::test_codex_ts_decide_returns_literal_verdict_stdout
func TestCodexTsDecideReturnsLiteralVerdictStdout(t *testing.T) {
	dir, path := hookSetUp(t)
	payload := with(hookPayload(path, ""), "tool_name", "exec_command",
		"tool_input", json.NewObject("cmd", decideCommand(t, hookRequest)),
		"transcript_path", writeJSONL(t, dir, "codex.jsonl", []*json.Object{codexUser("Which cache?")}, ""))
	out := handle(t, "pre_tool_use", payload, "codex", confident().Post)
	if decision(t, out) != "allow" {
		t.Fatalf("got %s", dumps(t, out))
	}
	assertContains(t, verdictOutput(t, dir, out, "cmd"), "ts-decide verdict")
}

// py: tests/test_hook.py::test_nested_native_reads_cannot_displace_a_requested_contract
func TestNestedNativeReadsCannotDisplaceARequestedContract(t *testing.T) {
	dir, path := hookSetUp(t)
	// Observed peer shape: functions.exec wraps exec_command; the host records its complete native cat
	// output before a separate ts-decide.
	contract := strings.Repeat("Preserve source identities and required review.\n", 600)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), contract)
	records := []*json.Object{codexUser("Choose the isolated diagnostic route under AGENTS.md.")}
	for index := range 8 {
		n := string(json.IntOf(int64(index)))
		records = append(records,
			json.NewObject("type", "response_item", "payload", json.NewObject("type", "function_call",
				"name", "functions.exec", "call_id", "outer-"+n, "arguments", dumps(t, json.NewObject(
					"code", "text(await tools.exec_command({cmd: 'cat AGENTS.md'}));")))),
			json.NewObject("type", "event_msg", "payload", json.NewObject("type", "item_completed", "item",
				json.NewObject("type", "CommandExecution", "id", "native-"+n,
					"command", []any{"/bin/zsh", "-lc", "cat AGENTS.md"}, "cwd", "file://"+dir,
					"status", "completed", "exit_code", json.IntOf(0), "aggregated_output", contract))))
	}
	request := with(obj(t, hookRequest), "contracts", []any{"AGENTS.md"})
	command := "ts-decide <<'EOF'\n" + dumps(t, request) + "\nEOF"
	payload := with(hookPayload(path, ""), "tool_name", "exec_command", "cwd", dir,
		"tool_input", json.NewObject("cmd", command, "workdir", dir),
		"transcript_path", writeJSONL(t, dir, "native-contract.jsonl", records, ""))
	api := confident()
	out := handle(t, "pre_tool_use", payload, "codex", api.Post)
	assertContains(t, verdictOutput(t, dir, out, "cmd"), "ADVISORY")
	bundle := at(t, api.Requests()[0], "state", "evidence")
	assertEqual(t, at(t, bundle, "source_files", 0, "path"), "AGENTS.md")
	assertEqual(t, at(t, bundle, "source_files", 0, "trust"), "verified_file_observation")
	sorted, err := json.Dumps(bundle, json.Options{SortKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(sorted)); n > policy.EvidenceChars {
		t.Fatalf("evidence is %d characters", n)
	}
}

// py: tests/test_hook.py::test_misuse_and_invalid_requests_are_denied_with_usage
func TestMisuseAndInvalidRequestsAreDeniedWithUsage(t *testing.T) {
	_, path := hookSetUp(t)
	for _, command := range []string{"ts-decide '{}' | cat", "ts-decide 'not json'"} {
		t.Run(command, func(t *testing.T) {
			out := handle(t, "pre_tool_use", hookPayload(path, command), "claude", fake(nil).Post)
			if decision(t, out) != "deny" {
				t.Fatalf("got %s", dumps(t, out))
			}
			assertContains(t, reason(t, out), "usage: ts-decide")
		})
	}
}

// py: tests/test_hook.py::test_malformed_answer_and_missing_transcript_give_unverified_verdicts
func TestMalformedAnswerAndMissingTranscriptGiveUnverifiedVerdicts(t *testing.T) {
	dir, path := hookSetUp(t)
	command := decideCommand(t, hookRequest)
	cases := []struct {
		payload *json.Object
		api     *jevtest.Fake
		reason  string
	}{
		{hookPayload(path, command), &jevtest.Fake{Raw: []byte(`{"answers": {}}`)}, "jev malformed"},
		{hookPayload(path, command, "transcript_path", filepath.Join(dir, "missing.jsonl")), fake(nil),
			"transcript unreadable"},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			output := verdictOutput(t, dir, handle(t, "pre_tool_use", c.payload, "claude", c.api.Post), "command")
			assertContains(t, output, "decision unverified")
			assertContains(t, output, c.reason)
		})
	}
}

func TestMalformedMissingFactChoiceRemainsUnverified(t *testing.T) {
	for _, host := range []string{"claude", "codex"} {
		for _, check := range []string{"total", "selected probability"} {
			t.Run(host+"/"+check, func(t *testing.T) {
				dir, path := hookSetUp(t)
				key := "command"
				payload := hookPayload(path, decideCommand(t, hookRequest))
				if host == "codex" {
					key = "cmd"
					payload.Set("tool_name", "exec_command")
					payload.Set("tool_input", json.NewObject(key, decideCommand(t, hookRequest)))
					payload.Set("transcript_path", writeJSONL(t, dir, "codex.jsonl", []*json.Object{codexUser("Which cache?")}, ""))
				}
				api := confident()
				post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
					raw, err := api.Post(body, key, timeout)
					if err != nil {
						return nil, err
					}
					response := obj(t, string(raw))
					answer := at(t, response, "answers", "missing_fact").(*json.Object)
					probabilities := answer.Value("probabilities").(*json.Object)
					ids := probabilities.Keys()
					if len(ids) < 2 {
						t.Fatal("expected at least two missing-fact criteria")
					}
					for _, id := range ids {
						probabilities.Set(id, 0.0)
					}
					answer.Set("choice", ids[0])
					if check == "total" {
						probabilities.Set(ids[0], 0.8)
						probabilities.Set(ids[1], 0.8)
					} else {
						probabilities.Set(ids[0], 0.1)
						probabilities.Set(ids[1], 0.9)
					}
					response.Set("private_response", "private-response-text")
					return []byte(dumps(t, response)), nil
				}
				out := handle(t, "pre_tool_use", payload, host, post)
				output := verdictOutput(t, dir, out, key)
				assertContains(t, output, "decision unverified (jev malformed: missing_fact: choice distribution)")
				assertNotContains(t, output, "ts-decide verdict:")
				assertNotContains(t, output, "private-response-text")
				assertEqual(t, len(api.Requests()), 1)
				rows := gateRows(t, "s1", "decide")
				if len(rows) != 1 {
					t.Fatalf("expected one decision row, got %d", len(rows))
				}
				assertEqual(t, rows[0].Value("action"), "unverified")
				assertEqual(t, rows[0].Value("receipt_id"), nil)
				assertNotContains(t, dumps(t, rows[0]), "private-response-text")
				calls := at(t, rows[0], "metrics", "api_calls").([]any)
				if len(calls) != 1 {
					t.Fatalf("expected one review metric, got %d", len(calls))
				}
				assertEqual(t, calls[0].(*json.Object).Value("error"), "malformed")
				assertEqual(t, calls[0].(*json.Object).Value("error_detail"), "missing_fact: choice distribution")
			})
		}
	}
}

// py: tests/test_hook.py::test_guard_denies_and_records
func TestGuardDeniesAndRecords(t *testing.T) {
	_, path := hookSetUp(t)
	out := handle(t, "pre_tool_use", hookPayload(path, "cat "+stateDir(t)+"/sessions/s1.jsonl"), "claude",
		fake(nil).Post)
	if decision(t, out) != "deny" {
		t.Fatalf("got %s", dumps(t, out))
	}
	assertEqual(t, entries(t, "s1")[0].Value("gate"), "guard")
}

// py: tests/test_hook.py::test_ordinary_tool_calls_pass_silently
func TestOrdinaryToolCallsPassSilently(t *testing.T) {
	_, path := hookSetUp(t)
	if out := handle(t, "pre_tool_use", hookPayload(path, "ls"), "claude", fake(nil).Post); out != nil {
		t.Fatalf("got %s", dumps(t, out))
	}
	read := with(hookPayload(path, "ls"), "tool_name", "Read", "tool_input", json.NewObject("file_path", "/x"))
	if out := handle(t, "pre_tool_use", read, "claude", fake(nil).Post); out != nil {
		t.Fatalf("got %s", dumps(t, out))
	}
}

// py: tests/test_hook.py::test_heredoc_diagnostic_needs_no_transcript_or_remote_review
func TestHeredocDiagnosticNeedsNoTranscriptOrRemoteReview(t *testing.T) {
	dir, path := hookSetUp(t)
	payload := hookPayload(path, "python3 - <<'PY'\nprint('ts-decide')\nPY\n")
	payload.Set("transcript_path", filepath.Join(dir, "missing.jsonl"))
	api := &jevtest.Fake{Raises: &pyerr.Error{Class: "RuntimeError", Msg: "Ordinary diagnostic reached remote review"}}
	if out := handle(t, "pre_tool_use", payload, "claude", api.Post); out != nil {
		t.Fatalf("got %s", dumps(t, out))
	}
	if len(api.Requests()) != 0 {
		t.Fatal("ordinary diagnostic reached remote review")
	}
}

// py: tests/test_hook.py::test_legacy_stop_does_not_block_or_require_context
func TestLegacyStopDoesNotBlockOrRequireContext(t *testing.T) {
	dir, path := hookSetUp(t)
	payload := json.NewObject("session_id", "s1", "transcript_path", path, "stop_hook_active", false,
		"last_assistant_message", "I chose LRU over LFU.")
	api := fake(map[string]any{"made_choice": 0.9})
	if out := handle(t, "stop", payload, "claude", api.Post); out != nil {
		t.Fatalf("got %s", dumps(t, out))
	}
	gone := with(payload, "transcript_path", filepath.Join(dir, "gone.jsonl"))
	if out := handle(t, "stop", gone, "claude", api.Post); out != nil {
		t.Fatalf("got %s", dumps(t, out))
	}
	if len(api.Requests()) != 0 {
		t.Fatal("stop reached Jev")
	}
}

// py: tests/test_hook.py::test_subagent_reads_its_own_transcript_and_the_users_words
func TestSubagentReadsItsOwnTranscriptAndTheUsersWords(t *testing.T) {
	dir, path := hookSetUp(t)
	writeJSONL(t, filepath.Join(dir, "s1", "subagents"), "agent-a1.jsonl", []*json.Object{
		json.NewObject("type", "user", "isSidechain", true,
			"message", json.NewObject("role", "user", "content", "brief: pick a cache"))}, "")
	api := confident()
	out := handle(t, "pre_tool_use", hookPayload(path, decideCommand(t, hookRequest), "agent_id", "a1"), "claude",
		api.Post)
	assertContains(t, verdictOutput(t, dir, out, "command"), "BINDING")
	bundle := at(t, api.Requests()[0], "state", "evidence")
	assertEqual(t, at(t, bundle, "brief"), "brief: pick a cache")
	assertEqual(t, at(t, bundle, "user_messages"), []any{"speed up the tile cache"})
}

// py: tests/test_hook.py::test_session_start_prunes
func TestSessionStartPrunes(t *testing.T) {
	setUp(t)
	if _, err := ledger.WriteVerdict("old", "v"); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(stateDir(t), "verdicts", "old.txt")
	if err := os.Chtimes(old, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	out := handle(t, "session_start", json.NewObject("session_id", "s1"), "claude", nil)
	assertEqual(t, at(t, out, "hookSpecificOutput", "hookEventName"), "SessionStart")
	assertContains(t, str(t, out, "hookSpecificOutput", "additionalContext"), filepath.Join(repoRoot, "bin/ts-decide"))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old verdict remains: %v", err)
	}
}

// --- ScriptTest: the Python tests ran hooks/hook.py; these run the agent-decisions binary.

// py: tests/test_hook.py::test_garbage_input_denies_required_review
func TestGarbageInputDeniesRequiredReview(t *testing.T) {
	setUp(t)
	run := runBinary(t, []byte("not json"), nil, "hook", "pre_tool_use", "--host", "claude")
	if run.code != 0 {
		t.Fatalf("exit %d: %s", run.code, run.stderr)
	}
	response := loads(t, run.stdout)
	assertEqual(t, at(t, response, "hookSpecificOutput", "permissionDecision"), "deny")
	assertContains(t, str(t, response, "hookSpecificOutput", "permissionDecisionReason"), "agent-decisions: hook error")
}

// py: tests/test_hook.py::test_off_switch_is_visible_and_does_not_disable_review
func TestOffSwitchIsVisibleAndDoesNotDisableReview(t *testing.T) {
	setUp(t)
	run := runBinary(t, []byte("not json"), []string{"AGENT_DECISIONS_OFF=1"}, "hook", "pre_tool_use", "--host", "claude")
	if run.code != 0 {
		t.Fatalf("exit %d: %s", run.code, run.stderr)
	}
	response := loads(t, run.stdout)
	assertEqual(t, at(t, response, "hookSpecificOutput", "permissionDecision"), "deny")
	assertContains(t, str(t, response, "systemMessage"), "AGENT_DECISIONS_OFF ignored")
}

// py: tests/test_hook.py::test_bad_utf8_and_lone_surrogates_in_stdin_become_replacement_characters
func TestBadUtf8AndLoneSurrogatesInStdinBecomeReplacementCharacters(t *testing.T) {
	setUp(t)
	stdin := []byte(`{"session_id": "s1", "tool_use_id": "t1", "prompt": "Keep \ud800 the ` + "\xff" + ` cache."}`)
	run := runBinary(t, stdin, nil, "hook", "user_prompt_submit", "--host", "claude")
	if run.code != 0 || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", run.code, run.stdout, run.stderr)
	}
	var prompts []any
	for _, entry := range entries(t, "s1") {
		prompts = append(prompts, entry.Value("prompt"))
	}
	assertEqual(t, prompts, []any{"Keep \ufffd the \ufffd cache."})
}

// py: tests/test_hook.py::test_stub_reports_an_unintercepted_call_without_approving_it
//
// The stub is the bin/ts-decide shell script, which the switch keeps; this runs it as the Python test did.
func TestStubReportsAnUninterceptedCallWithoutApprovingIt(t *testing.T) {
	stub := runProgram(t, nil, nil, filepath.Join(repoRoot, "bin", "ts-decide"), "{}")
	if stub.code != 3 {
		t.Fatalf("exit %d", stub.code)
	}
	for _, part := range []string{"did not intercept this call: decision unverified", "as its own shell command",
		"restart or resume", "Decide yourself"} {
		assertContains(t, stub.stderr, part)
	}
}

// A Go panic takes the fail-closed path of a Python exception, as RuntimeError, with the panic and its
// stack on stderr. Python has no counterpart.
func TestMainRecoversAPanic(t *testing.T) {
	setUp(t)
	patch(t, &newID, func() string { panic("forced") })
	var stdout, stderr strings.Builder
	code := Main([]string{"pre_tool_use", "--host", "claude"}, strings.NewReader(`{"tool_name": "Read"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := `{"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny", ` +
		`"permissionDecisionReason": "agent-decisions: hook error (RuntimeError); required review unavailable, action not run"}}`
	if stdout.String() != want {
		t.Fatalf("stdout %s", stdout.String())
	}
	assertContains(t, stderr.String(), "panic: forced\n")
	assertContains(t, stderr.String(), "goroutine ")
}
