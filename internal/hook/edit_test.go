package hook

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// --- tests/test_edit.py EditGateTest

// edContext ports EditGateTest.context: one completed ts-decide call and its ledger row.
func edContext(t *testing.T, action string, agentID any) gates.Context {
	t.Helper()
	call := transcript.ToolCall{ID: "decide1", Name: "Bash", Input: dumps(t, json.NewObject("command", "ts-decide '{}'")),
		Output: "ts-decide verdict: " + strings.ReplaceAll(strings.ToUpper(action), "_", " ")} // ASCII actions
	tr := transcript.Transcript{UserMessages: []string{"Fix the parser."}, ToolCalls: []transcript.ToolCall{call},
		TurnCalls: []transcript.ToolCall{call}}
	entry := json.NewObject("gate", "decide", "action", action, "tool_use_id", call.ID, "agent_id", agentID,
		"decision", "How should the parser read input?", "pick", "stream",
		"state", json.NewObject("options", json.NewObject("stream", "Read chunks")))
	return makeCtx(t, ctxOptions{transcript: &tr, entries: []*json.Object{entry}})
}

// edRunGate ports EditGateTest.run_gate.
func edRunGate(t *testing.T, ctx *gates.Context, name string, answers map[string]any) gates.Outcome {
	t.Helper()
	if ctx == nil {
		fresh := makeCtx(t, ctxOptions{})
		ctx = &fresh
	}
	if name == "" {
		name = "Edit"
	}
	return edEditGate(t, name, json.NewObject("file_path", "parser.py", "new_string", "read_chunks()"), *ctx,
		gatesAsker(fake(answers)))
}

func edEditGate(t *testing.T, name string, toolInput *json.Object, ctx gates.Context, ask gates.Ask) gates.Outcome {
	t.Helper()
	outcome, err := gates.EditGate(name, toolInput, ctx, ask, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func edAction(t *testing.T, outcome gates.Outcome, action string) {
	t.Helper()
	if outcome.Action != action {
		t.Fatalf("got %s (%q), want %s", outcome.Action, outcome.Message, action)
	}
}

// py: tests/test_edit.py::test_discretionary_edit_without_consultation_is_denied_on_every_attempt
func TestDiscretionaryEditWithoutConsultationIsDeniedOnEveryAttempt(t *testing.T) {
	setUp(t)
	ctx := makeCtx(t, ctxOptions{})
	for range 2 {
		outcome := edRunGate(t, &ctx, "", map[string]any{"needs_decision": 0.95})
		edAction(t, outcome, "deny")
		assertContains(t, outcome.Message, "ts-decide")
		ctx = makeCtx(t, ctxOptions{entries: append(ctx.Entries, with(outcome.Record, "action", outcome.Action))})
	}
}

// py: tests/test_edit.py::test_exact_user_instruction_is_judged_and_allowed
func TestExactUserInstructionIsJudgedAndAllowed(t *testing.T) {
	setUp(t)
	api := fake(map[string]any{"needs_decision": 0.01})
	outcome := edEditGate(t, "Write", json.NewObject("file_path", "README.md", "content", "Exact text"),
		makeCtx(t, ctxOptions{}), gatesAsker(api))
	edAction(t, outcome, "allow")
	assertEqual(t, outcome.Record.Value("gate"), "edit")
	assertEqual(t, at(t, api.Requests()[0], "state", "pending_tool"), "Write")
}

// py: tests/test_edit.py::test_covered_edit_is_allowed_but_unrelated_edit_is_denied
func TestCoveredEditIsAllowedButUnrelatedEditIsDenied(t *testing.T) {
	setUp(t)
	ctx := edContext(t, "advisory", nil)
	edAction(t, edRunGate(t, &ctx, "", map[string]any{"needs_decision": 0.95, "covered": 0.95}), "allow")
	edAction(t, edRunGate(t, &ctx, "", map[string]any{"needs_decision": 0.95, "covered": 0.05}), "deny")
}

// py: tests/test_edit.py::test_main_agent_advice_covers_a_follow_up_without_binding_certainty
func TestMainAgentAdviceCoversAFollowUpWithoutBindingCertainty(t *testing.T) {
	setUp(t)
	ctx := edContext(t, "advisory", nil)
	edAction(t, edRunGate(t, &ctx, "", map[string]any{"needs_decision": .95, "covered": .76}), "allow")
	worker := edContext(t, "binding", "worker")
	worker.AgentID = "worker"
	consulted, err := gates.Consulted(worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(consulted) != 1 {
		t.Fatalf("consulted %d verdicts, want 1", len(consulted))
	}
	edAction(t, edRunGate(t, &worker, "", map[string]any{"needs_decision": .95, "covered": .76}), "deny")
}

// py: tests/test_edit.py::test_unread_or_inactive_verdict_cannot_authorize_an_edit
func TestUnreadOrInactiveVerdictCannotAuthorizeAnEdit(t *testing.T) {
	for _, output := range []string{"", "agent-decisions hook inactive: decision unverified",
		"agent-decisions did not intercept this call: decision unverified", "ts-decide verdict: ASK USER"} {
		t.Run(output, func(t *testing.T) {
			setUp(t)
			ctx := edContext(t, "advisory", nil)
			call := ctx.Transcript.ToolCalls[0]
			call.Output = output
			ctx.Transcript.ToolCalls = []transcript.ToolCall{call}
			ctx.Transcript.TurnCalls = []transcript.ToolCall{call}
			api := fake(map[string]any{"needs_decision": 0.95, "covered": 0.99})
			outcome := edEditGate(t, "Edit", &json.Object{}, ctx, gatesAsker(api))
			edAction(t, outcome, "deny")
			assertEqual(t, at(t, api.Requests()[0], "state", "decisions"), []any{})
		})
	}
}

// py: tests/test_edit.py::test_parent_or_unresolved_verdict_cannot_authorize_an_edit
func TestParentOrUnresolvedVerdictCannotAuthorizeAnEdit(t *testing.T) {
	setUp(t)
	for _, c := range []struct {
		action string
		agent  any
	}{{"advisory", "other-agent"}, {"ask_user", nil}, {"refused", nil}, {"gather_evidence", nil}} {
		api := fake(map[string]any{"needs_decision": 0.95, "covered": 0.99})
		outcome := edEditGate(t, "Edit", &json.Object{}, edContext(t, c.action, c.agent), gatesAsker(api))
		edAction(t, outcome, "deny")
		assertEqual(t, at(t, api.Requests()[0], "state", "decisions"), []any{})
	}
}

// py: tests/test_edit.py::test_shell_write_is_classified_and_gated
func TestShellWriteIsClassifiedAndGated(t *testing.T) {
	setUp(t)
	edAction(t, edRunGate(t, nil, "Bash", map[string]any{"writes_files": 0.95, "needs_decision": 0.95}), "deny")
	edAction(t, edRunGate(t, nil, "Bash", map[string]any{"writes_files": 0.01, "needs_decision": 0.95}), "allow")
}

// py: tests/test_edit.py::test_failure_blocks_action_under_required_policy
func TestFailureBlocksActionUnderRequiredPolicy(t *testing.T) {
	setUp(t)
	outcome := edEditGate(t, "Edit", &json.Object{}, makeCtx(t, ctxOptions{}),
		gatesAsker(&jevtest.Fake{Raises: &jev.JevError{Reason: "timeout"}}))
	edAction(t, outcome, "deny")
	assertContains(t, outcome.Message, "timeout")
}

// --- tests/test_edit.py EditHookTest

// edHook ports EditHookTest.setUp: one transcript per host.
type edHook struct {
	dir, path, codexPath string
}

func edSetUp(t *testing.T) edHook {
	t.Helper()
	dir := setUp(t)
	return edHook{dir: dir,
		path:      writeJSONL(t, dir, "transcript.jsonl", []*json.Object{human("Fix the parser.")}, ""),
		codexPath: writeJSONL(t, dir, "rollout.jsonl", []*json.Object{codexUser("Fix the parser.")}, "")}
}

// payload ports EditHookTest.payload.
func (h edHook) payload(name string, value any, host string) *json.Object {
	path := h.path
	if host == "codex" {
		path = h.codexPath
	}
	return json.NewObject("session_id", "s1", "tool_use_id", "edit1", "tool_name", name, "tool_input", value,
		"transcript_path", path)
}

// py: tests/test_edit.py::test_each_host_edit_shape_reaches_gate
func TestEachHostEditShapeReachesGate(t *testing.T) {
	h := edSetUp(t)
	for _, c := range []struct {
		host, name string
		value      any
	}{
		{"claude", "Edit", json.NewObject("file_path", "parser.py")},
		{"claude", "Write", json.NewObject("file_path", "README.md")},
		{"codex", "apply_patch", json.NewObject("command", "*** Begin Patch\n*** End Patch")},
		{"codex", "apply_patch", "*** Begin Patch\n*** End Patch"},
	} {
		out := handle(t, "pre_tool_use", h.payload(c.name, c.value, c.host), c.host,
			fake(map[string]any{"needs_decision": 0.95}).Post)
		if got := decision(t, out); got != "deny" {
			t.Fatalf("%s %s: got %s", c.host, c.name, got)
		}
	}
	if rows := gateRows(t, "s1", "edit"); len(rows) != 4 {
		t.Fatalf("got %d edit rows, want 4", len(rows))
	}
}

// py: tests/test_edit.py::test_guard_takes_precedence_without_jev
func TestGuardTakesPrecedenceWithoutJev(t *testing.T) {
	h := edSetUp(t)
	api := fake(nil)
	out := handle(t, "pre_tool_use", h.payload("Write", json.NewObject("file_path", "~/.local/state/agent-decisions/x"),
		"claude"), "claude", api.Post)
	assertEqual(t, decision(t, out), "deny")
	if len(api.Requests()) != 0 {
		t.Fatal("the guard asked Jev")
	}
}

// py: tests/test_edit.py::test_ordinary_shell_and_reads_use_host_controls
func TestOrdinaryShellAndReadsUseHostControls(t *testing.T) {
	h := edSetUp(t)
	api := fake(map[string]any{"writes_files": 0.95, "needs_decision": 0.95})
	if out := handle(t, "pre_tool_use", h.payload("Bash", json.NewObject("command", "sed -i '' s/old/new/ parser.py"),
		"codex"), "codex", api.Post); out != nil {
		t.Fatalf("got %s for a shell call", dumps(t, out))
	}
	if out := handle(t, "pre_tool_use", h.payload("Read", json.NewObject("file_path", "parser.py"), "claude"),
		"claude", api.Post); out != nil {
		t.Fatalf("got %s for a read", dumps(t, out))
	}
	if len(api.Requests()) != 0 {
		t.Fatal("ordinary tools asked Jev")
	}
}

// py: tests/test_edit.py::test_unreadable_transcript_fails_open_in_availability_mode
func TestUnreadableTranscriptFailsOpenInAvailabilityMode(t *testing.T) {
	h := edSetUp(t)
	availability(t, "claude")
	payload := with(h.payload("Edit", &json.Object{}, "claude"), "transcript_path", filepath.Join(h.dir, "missing"))
	out := handle(t, "pre_tool_use", payload, "claude", fake(map[string]any{"needs_decision": 1.0}).Post)
	assertContains(t, str(t, out, "systemMessage"), "transcript unreadable")
}

// py: tests/test_edit.py::test_empty_or_unparseable_transcript_fails_open_in_availability_mode_without_jev
func TestEmptyOrUnparseableTranscriptFailsOpenInAvailabilityModeWithoutJev(t *testing.T) {
	h := edSetUp(t)
	for _, host := range []string{"claude", "codex"} {
		availability(t, host)
		for _, content := range []string{"", "not json\n", "{}\n"} {
			path := h.path
			if host == "codex" {
				path = h.codexPath
			}
			writeFile(t, path, content)
			api := fake(map[string]any{"needs_decision": 1.0})
			out := handle(t, "pre_tool_use", h.payload("Edit", &json.Object{}, host), host, api.Post)
			message, _ := out.Value("systemMessage").(string)
			if !strings.Contains(message, "transcript unreadable") || len(api.Requests()) != 0 {
				t.Fatalf("%s %q: got %s with %d requests", host, content, dumps(t, out), len(api.Requests()))
			}
		}
	}
}

// py: tests/test_edit.py::test_codex_question_tools_use_host_controls_without_jev
func TestCodexQuestionToolsUseHostControlsWithoutJev(t *testing.T) {
	h := edSetUp(t)
	for _, name := range []string{"request_user_input", "request_user_input_async"} {
		payload := with(h.payload(name, json.NewObject("questions", []any{
			json.NewObject("question", "Which database?", "options", []any{})}), "codex"), "session_id", name)
		api := fake(map[string]any{"stated_0": 0.95, "owner_0": "user"})
		if out := handle(t, "pre_tool_use", payload, "codex", api.Post); out != nil || len(api.Requests()) != 0 {
			t.Fatalf("%s: got %v with %d requests", name, out, len(api.Requests()))
		}
	}
}
