package hook

import (
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// dcStopDecide is test_stop.py's DECIDE.
var dcStopDecide = transcript.ToolCall{ID: "toolu_d", Name: "Bash", Input: `{"command": "ts-decide '{}'"}`,
	Output: "ts-decide verdict: ADVISORY"}

// dcBinding returns test_stop.py's BINDING.
func dcBinding() *json.Object {
	return json.NewObject("gate", "decide", "tool_use_id", "toolu_d", "action", "binding", "decision", "Which cache?",
		"pick", "lfu", "state", json.NewObject("options", json.NewObject("lru", "LRU", "lfu", "LFU")))
}

// dcCtxWith ports test_stop.py ctx_with.
func dcCtxWith(t *testing.T, calls []transcript.ToolCall, rows []*json.Object) gates.Context {
	t.Helper()
	tr := &transcript.Transcript{UserMessages: []string{"fix the cache"}, ToolCalls: calls, TurnCalls: calls}
	return makeCtx(t, ctxOptions{transcript: tr, entries: rows})
}

func dcStop(t *testing.T, final string, active bool, ctx gates.Context, ask gates.Ask) gates.Outcome {
	t.Helper()
	outcome, err := gates.StopGate(final, active, ctx, ask, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

// py: tests/test_stop.py::test_uncovered_choice_identifies_the_checked_section_and_receipt_count
func TestUncoveredChoiceIdentifiesTheCheckedSectionAndReceiptCount(t *testing.T) {
	setUp(t)
	final := "The parser tests passed.\n\nI selected PostgreSQL for application storage."
	outcome := dcStop(t, final, false, makeCtx(t, ctxOptions{}),
		gatesAsker(fake(map[string]any{"made_choice": .95, "uncovered_section": "section_1"})))
	dcAction(t, outcome, "block")
	assertContains(t, outcome.Message, "Eligible receipts: 0")
	assertContains(t, outcome.Message, "section_1")
	assertContains(t, outcome.Message, "PostgreSQL")
}

// py: tests/test_stop.py::test_observed_quoted_verdict_is_not_a_question_to_the_user
func TestObservedQuotedVerdictIsNotAQuestionToTheUser(t *testing.T) {
	setUp(t)
	final := str(t, dcFixture(t, "peer-ergonomics.json"), "e2e_final")
	var current string
	// The gate can ask from other goroutines, so the checks report with Errorf.
	ask := func(state, questions *json.Object) (*json.Object, error) {
		if state.Value("final_message") != current {
			t.Errorf("final_message %q, want %q", state.Value("final_message"), current)
		}
		question, _ := state.Value("question_message").(string)
		if strings.Contains(question, "ts-decide verdict:") {
			t.Errorf("question_message carries the verdict: %q", question)
		}
		for _, key := range []string{"asks_user", "stated", "owner"} {
			instructions, _ := at(t, questions, key, "instructions").(string)
			if !strings.Contains(instructions, "question_message") {
				t.Errorf("%s instructions do not name question_message", key)
			}
		}
		asks := .01
		if strings.Contains(question, "Should I") {
			asks = .95
		}
		return gatesAsker(fake(map[string]any{"asks_user": asks}))(state, questions)
	}
	current = final
	dcAction(t, dcStop(t, current, false, dcCtxWith(t, nil, nil), ask), "allow")
	current = final + "\nShould I replace the test framework?"
	dcAction(t, dcStop(t, current, false, dcCtxWith(t, nil, nil), ask), "block")
}

// py: tests/test_stop.py::test_a_choice_inside_fenced_code_still_requires_a_consultation
func TestAChoiceInsideFencedCodeStillRequiresAConsultation(t *testing.T) {
	setUp(t)
	final := "```text\nI chose PostgreSQL instead of SQLite.\n```"
	f := fake(map[string]any{"made_choice": .95})
	dcAction(t, dcStop(t, final, false, dcCtxWith(t, nil, nil), gatesAsker(f)), "block")
	assertEqual(t, at(t, f.Requests()[0], "state", "final_message"), final)
}

// py: tests/test_stop.py::test_stop_hook_active_still_checks_a_new_choice
func TestStopHookActiveStillChecksANewChoice(t *testing.T) {
	setUp(t)
	f := fake(map[string]any{"made_choice": 1.0})
	dcAction(t, dcStop(t, "I chose LRU.", true, dcCtxWith(t, nil, nil), gatesAsker(f)), "block")
	if len(f.Requests()) != 1 {
		t.Fatalf("got %d requests", len(f.Requests()))
	}
}

// py: tests/test_stop.py::test_unconsulted_choice_blocks
func TestUnconsultedChoiceBlocks(t *testing.T) {
	setUp(t)
	outcome := dcStop(t, "I chose LRU over LFU.", false, dcCtxWith(t, nil, nil),
		gatesAsker(fake(map[string]any{"made_choice": 0.7})))
	dcAction(t, outcome, "block")
	assertContains(t, outcome.Message, "without a relevant ts-decide")
}

// py: tests/test_stop.py::test_consulted_choice_passes
func TestConsultedChoicePasses(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, []transcript.ToolCall{dcStopDecide},
		[]*json.Object{json.NewObject("gate", "decide", "tool_use_id", "toolu_d", "action", "advisory")})
	outcome := dcStop(t, "I chose LRU.", false, ctx,
		gatesAsker(fake(map[string]any{"made_choice": 0.9, "uncovered_choice": .05})))
	dcAction(t, outcome, "allow")
}

// py: tests/test_stop.py::test_an_additional_choice_blocks_even_when_other_choices_follow_bindings
func TestAnAdditionalChoiceBlocksEvenWhenOtherChoicesFollowBindings(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, []transcript.ToolCall{dcStopDecide},
		[]*json.Object{json.NewObject("gate", "decide", "tool_use_id", dcStopDecide.ID, "action", "advisory")})
	outcome := dcStop(t, "I used LRU and PostgreSQL.", false, ctx,
		gatesAsker(fake(map[string]any{"made_choice": .95, "uncovered_choice": .76})))
	dcAction(t, outcome, "block")
}

// py: tests/test_stop.py::test_question_the_user_already_answered_blocks
func TestQuestionTheUserAlreadyAnsweredBlocks(t *testing.T) {
	setUp(t)
	outcome := dcStop(t, "Should I use Postgres?", false, dcCtxWith(t, nil, nil),
		gatesAsker(fake(map[string]any{"asks_user": 0.8, "stated": 0.9, "owner": "user"})))
	dcAction(t, outcome, "block")
	assertContains(t, outcome.Message, "already answer")
}

// py: tests/test_stop.py::test_question_the_agent_owns_blocks
func TestQuestionTheAgentOwnsBlocks(t *testing.T) {
	setUp(t)
	outcome := dcStop(t, "Dict or list?", false, dcCtxWith(t, nil, nil),
		gatesAsker(fake(map[string]any{"asks_user": 0.8, "owner": "agent"})))
	dcAction(t, outcome, "block")
}

// py: tests/test_stop.py::test_waiting_after_a_question_tool_passes
func TestWaitingAfterAQuestionToolPasses(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, []transcript.ToolCall{{ID: "toolu_q", Name: "AskUserQuestion", Input: "{}"}}, nil)
	outcome := dcStop(t, "Waiting for your answer.", false, ctx, gatesAsker(fake(map[string]any{"asks_user": 0.0})))
	dcAction(t, outcome, "allow")
}

// py: tests/test_stop.py::test_prior_question_tool_does_not_exempt_a_different_final_question
func TestPriorQuestionToolDoesNotExemptADifferentFinalQuestion(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, []transcript.ToolCall{{ID: "toolu_q", Name: "AskUserQuestion", Input: "{}"}}, nil)
	outcome := dcStop(t, "Should I use Postgres?", false, ctx,
		gatesAsker(fake(map[string]any{"asks_user": .95, "stated": .99, "owner": "user"})))
	dcAction(t, outcome, "block")
}

// dcBindingCall is replace(DECIDE, output="ts-decide verdict: BINDING").
func dcBindingCall() transcript.ToolCall {
	call := dcStopDecide
	call.Output = "ts-decide verdict: BINDING"
	return call
}

// py: tests/test_stop.py::test_exact_user_instruction_takes_precedence_over_binding
func TestExactUserInstructionTakesPrecedenceOverBinding(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, []transcript.ToolCall{dcBindingCall()}, []*json.Object{dcBinding()})
	outcome := dcStop(t, "Done: implemented LRU as you instructed.", false, ctx,
		gatesAsker(fake(map[string]any{"made_choice": .99, "settled_choice": .99, "follows_pick": .01})))
	dcAction(t, outcome, "allow")
}

// py: tests/test_stop.py::test_binding_pick_not_followed_blocks_and_restates_the_pick
func TestBindingPickNotFollowedBlocksAndRestatesThePick(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, []transcript.ToolCall{dcBindingCall()}, []*json.Object{dcBinding()})
	f := fake(map[string]any{"follows_pick": 0.4})
	outcome := dcStop(t, "Done: implemented LRU.", false, ctx, gatesAsker(f))
	dcAction(t, outcome, "block")
	assertContains(t, outcome.Message, "`lfu`")
	assertEqual(t, at(t, f.Requests()[0], "state", "binding_picks", 0, "pick"), "lfu")
}

// py: tests/test_stop.py::test_previous_block_does_not_authorize_a_choice
func TestPreviousBlockDoesNotAuthorizeAChoice(t *testing.T) {
	setUp(t)
	ctx := dcCtxWith(t, nil, nil)
	prior := json.NewObject("gate", "stop", "action", "block", "turn", ctx.Transcript.TurnKey())
	outcome := dcStop(t, "I chose LRU.", false, dcCtxWith(t, nil, []*json.Object{prior}),
		gatesAsker(fake(map[string]any{"made_choice": 1.0})))
	dcAction(t, outcome, "block")
}

// py: tests/test_stop.py::test_empty_final_message_passes_without_a_call
func TestEmptyFinalMessagePassesWithoutACall(t *testing.T) {
	setUp(t)
	f := fake(nil)
	dcAction(t, dcStop(t, "  ", false, dcCtxWith(t, nil, nil), gatesAsker(f)), "allow")
	if len(f.Requests()) != 0 {
		t.Fatal("Jev was called")
	}
}

// py: tests/test_stop.py::test_jev_failure_blocks_under_required_policy
func TestJevFailureBlocksUnderRequiredPolicy(t *testing.T) {
	setUp(t)
	outcome := dcStop(t, "I chose LRU.", false, dcCtxWith(t, nil, nil),
		gatesAsker(&jevtest.Fake{Raises: &jev.JevError{Reason: "timeout"}}))
	if outcome.Action != "block" || !strings.Contains(outcome.Message, "timeout") {
		t.Fatalf("got %s (%q)", outcome.Action, outcome.Message)
	}
}
