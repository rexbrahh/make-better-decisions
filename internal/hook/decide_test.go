package hook

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/provenance"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// dcRequest is test_decide.py's REQUEST.
func dcRequest() *json.Object {
	return json.NewObject(
		"decision", "Which eviction policy for the tile cache?",
		"options", []any{json.NewObject("id", "lru", "description", "Least recently used"),
			json.NewObject("id", "lfu", "description", "Least frequently used")},
		"facts", []any{"decide the cache policy"})
}

// dcConfident returns test_decide.py's CONFIDENT answers with overrides applied.
func dcConfident(overrides map[string]any) *jevtest.Fake {
	answers := map[string]any{"pick": "lfu", "sufficient": 0.9, "missing_option": 0.1, "argued": 0.1,
		"owner": "agent"}
	for key, value := range overrides {
		answers[key] = value
	}
	return fake(answers)
}

// dcRun ports test_decide.py run with make_ctx options.
func dcRun(t *testing.T, f *jevtest.Fake, request *json.Object, o ctxOptions) gates.Outcome {
	t.Helper()
	tr := transcript.Transcript{UserMessages: []string{"decide the cache policy"}, ToolCalls: []transcript.ToolCall{
		{ID: "t1", Name: "Bash", Input: "trace", Output: "Tiles are re-read in bursts while panning"}}}
	o.transcript = &tr
	return dcDecide(t, dumps(t, request), makeCtx(t, o), gatesAsker(f))
}

func dcDecide(t *testing.T, raw string, ctx gates.Context, ask gates.Ask) gates.Outcome {
	t.Helper()
	outcome, err := gates.Decide(raw, ctx, ask)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func dcAction(t *testing.T, outcome gates.Outcome, action string) {
	t.Helper()
	if outcome.Action != action {
		t.Fatalf("got %s (%q), want %s", outcome.Action, outcome.Message, action)
	}
}

// dcFixture reads tests/fixtures/name.
func dcFixture(t *testing.T, name string) any {
	t.Helper()
	return loads(t, readFile(t, filepath.Join(repoRoot, "tests", "fixtures", name)))
}

// dcFacts returns a request's facts as the user messages of a transcript.
func dcFacts(t *testing.T, request *json.Object) *transcript.Transcript {
	t.Helper()
	var facts []string
	for _, fact := range at(t, request, "facts").([]any) {
		facts = append(facts, fact.(string))
	}
	return &transcript.Transcript{UserMessages: facts}
}

// dcCriterion returns the key of the missing_fact criterion whose text matches.
func dcCriterion(t *testing.T, questions *json.Object, match func(string) bool) string {
	t.Helper()
	criteria := at(t, questions, "missing_fact", "criteria").(*json.Object)
	for _, key := range criteria.Keys() {
		if match(criteria.Value(key).(string)) {
			return key
		}
	}
	t.Fatalf("no criterion matches in %s", dumps(t, criteria))
	return ""
}

// py: tests/test_decide.py::test_peer_gather_names_the_claim_that_needs_an_observation
func TestPeerGatherNamesTheClaimThatNeedsAnObservation(t *testing.T) {
	setUp(t)
	request := at(t, dcFixture(t, "windows-diagnostic-evidence.json"), "request").(*json.Object)
	claim := "Current AWS read says stopped and no lease."
	ask := func(state, questions *json.Object) (*json.Object, error) {
		selected := dcCriterion(t, questions, func(text string) bool { return text == claim })
		f := fake(map[string]any{"pick": "inspect_retained_native_diagnostics", "sufficient": .64,
			"unsupported": .83, "evidence_needed": "implementation", "missing_fact": selected,
			"missing_option": .36, "argued": .20, "owner": "agent"})
		return gatesAsker(f)(state, questions)
	}
	result := dcDecide(t, dumps(t, request), makeCtx(t, ctxOptions{transcript: dcFacts(t, request)}), ask)
	dcAction(t, result, "gather_evidence")
	assertContains(t, result.Message, "missing evidence: "+dumps(t, claim))
	assertContains(t, result.Message, "unsupported=0.83")
	assertContains(t, result.Message, "observed result")
}

// py: tests/test_decide.py::test_missing_fact_selection_has_no_authority_over_the_verdict
func TestMissingFactSelectionHasNoAuthorityOverTheVerdict(t *testing.T) {
	setUp(t)
	f := dcConfident(map[string]any{"missing_fact": "not_identified"})
	result := dcRun(t, f, dcRequest(), ctxOptions{})
	dcAction(t, result, "advisory")
	assertNotContains(t, result.Message, "missing evidence:")
	requests := f.Requests()
	if len(requests) != 1 {
		t.Fatalf("got %d requests", len(requests))
	}
	if _, ok := at(t, requests[0], "questions").(*json.Object).Get("missing_fact"); !ok {
		t.Fatal("missing_fact was not asked")
	}
}

// py: tests/test_decide.py::test_gather_does_not_repeat_the_route_question_as_a_missing_fact
func TestGatherDoesNotRepeatTheRouteQuestionAsAMissingFact(t *testing.T) {
	setUp(t)
	request := at(t, dcFixture(t, "windows-diagnostic-evidence.json"), "request").(*json.Object)
	f := fake(map[string]any{"pick": "inspect_retained_native_diagnostics", "sufficient": .64, "unsupported": .9})
	result := dcDecide(t, dumps(t, request), makeCtx(t, ctxOptions{transcript: dcFacts(t, request)}), gatesAsker(f))
	dcAction(t, result, "gather_evidence")
	question, _, _ := strings.Cut(str(t, request, "decision"), "?") // ASCII separator
	question += "?"
	criteria := at(t, f.Requests()[0], "questions", "missing_fact", "criteria").(*json.Object)
	for _, key := range criteria.Keys() {
		if criteria.Value(key) == question {
			t.Fatalf("the route question %q is a missing-fact criterion", question)
		}
	}
	assertNotContains(t, result.Message, "missing evidence: "+dumps(t, question))
}

// py: tests/test_decide.py::test_unidentified_missing_fact_is_explicit_without_releasing_the_guard
func TestUnidentifiedMissingFactIsExplicitWithoutReleasingTheGuard(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"unsupported": .9, "missing_fact": "not_identified"}),
		dcRequest(), ctxOptions{})
	dcAction(t, result, "gather_evidence")
	assertContains(t, result.Message, "no specific missing fact was identified")
}

// py: tests/test_decide.py::test_missing_implementation_keeps_the_helper_named_by_its_previous_sentence
func TestMissingImplementationKeepsTheHelperNamedByItsPreviousSentence(t *testing.T) {
	setUp(t)
	request := with(dcRequest(), "decision", "Which diagnostic route should run? "+
		"The helper /unobserved/qualification-gate.ps1 implements a required gate. "+
		"Its implementation and gate conditions have not been observed.")
	ask := func(state, questions *json.Object) (*json.Object, error) {
		selected := dcCriterion(t, questions, func(text string) bool { return strings.Contains(text, "Its implementation") })
		return gatesAsker(dcConfident(map[string]any{"unsupported": .9, "missing_fact": selected}))(state, questions)
	}
	result := dcDecide(t, dumps(t, request), makeCtx(t, ctxOptions{}), ask)
	dcAction(t, result, "gather_evidence")
	_, after, _ := strings.Cut(result.Message, "missing evidence:")
	assertContains(t, after, "/unobserved/qualification-gate.ps1")
}

// py: tests/test_decide.py::test_source_backed_main_agent_comparison_returns_advice_without_a_user_tiebreak
func TestSourceBackedMainAgentComparisonReturnsAdviceWithoutAUserTiebreak(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"sufficient": .68, "pattern_present": .76,
		"follows_pattern_lfu": .72}), dcRequest(), ctxOptions{})
	dcAction(t, result, "advisory")
}

// py: tests/test_decide.py::test_advisory_ownership_does_not_need_worker_binding_certainty
func TestAdvisoryOwnershipDoesNotNeedWorkerBindingCertainty(t *testing.T) {
	setUp(t)
	ask := func(state, questions *json.Object) (*json.Object, error) {
		answers, err := gatesAsker(dcConfident(map[string]any{"sufficient": .65}))(state, questions)
		if err == nil {
			at(t, answers, "owner").(*json.Object).Set("probabilities", json.NewObject("agent", .78, "user", .22))
		}
		return answers, err
	}
	dcAction(t, dcDecide(t, dumps(t, dcRequest()), makeCtx(t, ctxOptions{}), ask), "advisory")
}

// py: tests/test_decide.py::test_observed_sdk_interface_pattern_supplies_a_selection_basis
func TestObservedSdkInterfacePatternSuppliesASelectionBasis(t *testing.T) {
	setUp(t)
	requests := dcFixture(t, "sdk-save-export-requests.json").([]any)
	request := at(t, requests[len(requests)-1], "request")
	tr := &transcript.Transcript{UserMessages: []string{"Continue implementing the SDK and preserve existing behavior."}}
	f := fake(map[string]any{"pick": "named_functions", "sufficient": .52, "unsupported": .57,
		"omitted_requirement": .38, "missing_option": .26, "argued": .16,
		"pattern_present": .87, "follows_pattern_named_functions": .78})
	dcAction(t, dcDecide(t, dumps(t, request), makeCtx(t, ctxOptions{transcript: tr}), gatesAsker(f)), "advisory")
}

// py: tests/test_decide.py::test_medium_pattern_match_is_not_binding_for_a_worker
func TestMediumPatternMatchIsNotBindingForAWorker(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"sufficient": .52, "pattern_present": .87,
		"follows_pattern_lfu": .78}), dcRequest(), ctxOptions{agentID: "worker"})
	dcAction(t, result, "gather_evidence")
}

// py: tests/test_decide.py::test_medium_pattern_match_does_not_settle_uncertain_user_ownership
func TestMediumPatternMatchDoesNotSettleUncertainUserOwnership(t *testing.T) {
	setUp(t)
	ask := func(state, questions *json.Object) (*json.Object, error) {
		answers, err := gatesAsker(dcConfident(map[string]any{"sufficient": .52, "pattern_present": .87,
			"follows_pattern_lfu": .78}))(state, questions)
		if err == nil {
			owner := at(t, answers, "owner").(*json.Object)
			owner.Set("probabilities", json.NewObject("agent", .55, "user", .45))
			owner.Set("confidence", .1)
		}
		return answers, err
	}
	tr := &transcript.Transcript{UserMessages: []string{"decide the cache policy"}}
	dcAction(t, dcDecide(t, dumps(t, dcRequest()), makeCtx(t, ctxOptions{transcript: tr}), ask), "gather_evidence")
}

// py: tests/test_decide.py::test_existing_pattern_cannot_ground_a_pick_that_does_not_follow_it
func TestExistingPatternCannotGroundAPickThatDoesNotFollowIt(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"sufficient": .1, "pattern_present": .95,
		"follows_pattern_lfu": .1}), dcRequest(), ctxOptions{})
	dcAction(t, result, "gather_evidence")
}

// py: tests/test_decide.py::test_pattern_reuse_does_not_authorize_missing_required_facts_or_constraints
func TestPatternReuseDoesNotAuthorizeMissingRequiredFactsOrConstraints(t *testing.T) {
	for _, missing := range []string{"unsupported", "omitted_requirement"} {
		t.Run(missing, func(t *testing.T) {
			setUp(t)
			result := dcRun(t, dcConfident(map[string]any{"sufficient": .52, "pattern_present": .95,
				"follows_pattern_lfu": .96, missing: .95}), dcRequest(), ctxOptions{})
			dcAction(t, result, "gather_evidence")
		})
	}
}

// py: tests/test_decide.py::test_existing_helper_omission_is_composed_from_independent_conditions
func TestExistingHelperOmissionIsComposedFromIndependentConditions(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"missing_option": .58, "helper_present": .93,
		"helper_in_options": .1}), dcRequest(), ctxOptions{})
	dcAction(t, result, "refused")
	assertContains(t, result.Message, "missing_existing_helper")
}

// py: tests/test_decide.py::test_exact_user_operation_does_not_require_reusing_an_existing_helper
func TestExactUserOperationDoesNotRequireReusingAnExistingHelper(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"specified_lfu": .95, "helper_present": .93,
		"helper_in_options": .1}), dcRequest(), ctxOptions{})
	dcAction(t, result, "routine")
}

// py: tests/test_decide.py::test_pick_cannot_contradict_the_option_prescribed_by_the_user
func TestPickCannotContradictTheOptionPrescribedByTheUser(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"specified_lru": .95, "specified_lfu": .01}),
		dcRequest(), ctxOptions{})
	dcAction(t, result, "refused")
	assertContains(t, result.Message, "pick_conflicts_with_instruction")
}

// py: tests/test_decide.py::test_no_fit_due_to_missing_evidence_requests_the_missing_evidence
func TestNoFitDueToMissingEvidenceRequestsTheMissingEvidence(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"pick": "none_fit", "sufficient": .1, "unsupported": .95,
		"omitted_requirement": .95}), dcRequest(), ctxOptions{})
	dcAction(t, result, "gather_evidence")
}

// py: tests/test_decide.py::test_omitted_requirement_is_checked_independently_of_unsupported_claim
func TestOmittedRequirementIsCheckedIndependentlyOfUnsupportedClaim(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"unsupported": .01, "omitted_requirement": .95}),
		dcRequest(), ctxOptions{})
	dcAction(t, result, "gather_evidence")
}

// py: tests/test_decide.py::test_routine_operation_does_not_require_an_exact_option_instruction
func TestRoutineOperationDoesNotRequireAnExactOptionInstruction(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"sufficient": .2, "specified_lfu": .01,
		"routine_operation": .95}), dcRequest(), ctxOptions{})
	dcAction(t, result, "routine")
}

// py: tests/test_decide.py::test_authorized_routine_work_does_not_wait_for_design_sufficiency
func TestAuthorizedRoutineWorkDoesNotWaitForDesignSufficiency(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"sufficient": .56, "specified_lfu": .95}), dcRequest(), ctxOptions{})
	dcAction(t, result, "routine")
	assertContains(t, result.Message, "PROCEED")
}

// py: tests/test_decide.py::test_routine_label_cannot_authorize_unsupported_claims
func TestRoutineLabelCannotAuthorizeUnsupportedClaims(t *testing.T) {
	setUp(t)
	result := dcRun(t, dcConfident(map[string]any{"sufficient": .56, "specified_lfu": .95, "unsupported": .95}),
		dcRequest(), ctxOptions{})
	dcAction(t, result, "gather_evidence")
}

// py: tests/test_decide.py::test_main_agent_gets_an_advisory_verdict
func TestMainAgentGetsAnAdvisoryVerdict(t *testing.T) {
	setUp(t)
	outcome := dcRun(t, dcConfident(nil), dcRequest(), ctxOptions{})
	dcAction(t, outcome, "advisory")
	assertContains(t, outcome.Message, "ts-decide verdict: ADVISORY")
	assertContains(t, outcome.Message, "pick: lfu")
	assertEqual(t, at(t, outcome.Record, "pick"), "lfu")
}

// py: tests/test_decide.py::test_subagent_with_sufficient_evidence_gets_a_binding_verdict
func TestSubagentWithSufficientEvidenceGetsABindingVerdict(t *testing.T) {
	setUp(t)
	dcAction(t, dcRun(t, dcConfident(nil), dcRequest(), ctxOptions{agentID: "a1"}), "binding")
}

// py: tests/test_decide.py::test_insufficient_evidence_is_never_binding
func TestInsufficientEvidenceIsNeverBinding(t *testing.T) {
	setUp(t)
	outcome := dcRun(t, dcConfident(map[string]any{"sufficient": 0.79}), dcRequest(), ctxOptions{agentID: "a1"})
	dcAction(t, outcome, "gather_evidence")
	assertContains(t, outcome.Message, "Gather it with tool calls")
}

// py: tests/test_decide.py::test_user_owned_decision_sends_the_agent_to_the_user
func TestUserOwnedDecisionSendsTheAgentToTheUser(t *testing.T) {
	setUp(t)
	dcAction(t, dcRun(t, dcConfident(map[string]any{"owner": "user"}), dcRequest(), ctxOptions{agentID: "a1"}),
		"ask_user")
}

// py: tests/test_decide.py::test_missing_option_argued_and_none_fit_refuse
func TestMissingOptionArguedAndNoneFitRefuse(t *testing.T) {
	for _, override := range []map[string]any{{"missing_option": 0.6}, {"argued": 0.6}, {"pick": "none_fit"}} {
		t.Run(dumps(t, json.NewObject(firstPair(override)...)), func(t *testing.T) {
			setUp(t)
			outcome := dcRun(t, dcConfident(override), dcRequest(), ctxOptions{})
			dcAction(t, outcome, "refused")
			assertContains(t, outcome.Message, "attempt 1")
		})
	}
}

// firstPair returns the one key and value of a single-entry map as NewObject pairs.
func firstPair(m map[string]any) []any {
	for key, value := range m {
		return []any{key, value}
	}
	return nil
}

// py: tests/test_decide.py::test_user_owned_decision_with_only_none_fit_asks_the_user
func TestUserOwnedDecisionWithOnlyNoneFitAsksTheUser(t *testing.T) {
	setUp(t)
	outcome := dcRun(t, dcConfident(map[string]any{"owner": "user", "pick": "none_fit"}), dcRequest(), ctxOptions{})
	dcAction(t, outcome, "ask_user")
}

// py: tests/test_decide.py::test_user_owned_decision_with_a_missing_option_is_still_refused
func TestUserOwnedDecisionWithAMissingOptionIsStillRefused(t *testing.T) {
	setUp(t)
	outcome := dcRun(t, dcConfident(map[string]any{"owner": "user", "missing_option": 0.6}), dcRequest(), ctxOptions{})
	dcAction(t, outcome, "refused")
}

// py: tests/test_decide.py::test_third_attempt_still_requires_repair
func TestThirdAttemptStillRequiresRepair(t *testing.T) {
	setUp(t)
	key := provenance.Normalize(str(t, dcRequest(), "decision"))
	var prior []*json.Object
	for range policy.RefusalCap {
		prior = append(prior, json.NewObject("gate", "decide", "action", "refused", "decision_key", key))
	}
	outcome := dcRun(t, dcConfident(map[string]any{"argued": 0.9}), dcRequest(), ctxOptions{entries: prior})
	dcAction(t, outcome, "refused")
	assertContains(t, outcome.Message, "REQUEST STILL NEEDS REPAIR")
}

// py: tests/test_decide.py::test_state_carries_verified_user_facts_and_policy
func TestStateCarriesVerifiedUserFactsAndPolicy(t *testing.T) {
	setUp(t)
	f := dcConfident(nil)
	dcRun(t, f, dcRequest(), ctxOptions{})
	request := f.Requests()[0]
	state := at(t, request, "state")
	assertEqual(t, at(t, state, "facts"), at(t, dcRequest(), "facts"))
	assertEqual(t, at(t, state, "unverified_claims"), []any{})
	assertEqual(t, at(t, state, "policy"), policy.Policy)
	if _, ok := at(t, state, "evidence").(*json.Object).Get("tool_calls_newest_first"); !ok {
		t.Fatal("evidence has no tool_calls_newest_first")
	}
	if _, ok := at(t, request, "questions", "pick", "criteria").(*json.Object).Get("none_fit"); !ok {
		t.Fatal("pick has no none_fit criterion")
	}
	found := false
	for _, fact := range at(t, state, "facts").([]any) {
		found = found || fact == "decide the cache policy"
	}
	if !found {
		t.Fatal("facts do not carry the user's words")
	}
}

// py: tests/test_decide.py::test_invalid_requests_are_rejected_without_a_jev_call
func TestInvalidRequestsAreRejectedWithoutAJevCall(t *testing.T) {
	options := at(t, dcRequest(), "options").([]any)
	first := options[0]
	bad := []string{"not json", "[]",
		dumps(t, with(dcRequest(), "decision", " ")),
		dumps(t, with(dcRequest(), "options", options[:1])),
		dumps(t, with(dcRequest(), "options", []any{json.NewObject("id", "none_fit", "description", "x"), first})),
		dumps(t, with(dcRequest(), "options", []any{first, first})),
		dumps(t, with(dcRequest(), "options", []any{json.NewObject("id", "Bad Id", "description", "x"), first})),
		dumps(t, with(dcRequest(), "facts", "one fact"))}
	for _, raw := range bad {
		t.Run(raw[:min(40, len(raw))], func(t *testing.T) {
			setUp(t)
			f := fake(nil)
			outcome := dcDecide(t, raw, makeCtx(t, ctxOptions{}), gatesAsker(f))
			dcAction(t, outcome, "invalid")
			assertContains(t, outcome.Message, "usage: ts-decide")
			if outcome.Record != nil {
				t.Fatalf("record %s", dumps(t, outcome.Record))
			}
			if len(f.Requests()) != 0 {
				t.Fatal("Jev was called")
			}
		})
	}
}

// py: tests/test_decide.py::test_jev_failure_is_an_unverified_decision
func TestJevFailureIsAnUnverifiedDecision(t *testing.T) {
	setUp(t)
	outcome := dcRun(t, &jevtest.Fake{Raises: &jev.JevError{Reason: "timeout"}}, dcRequest(), ctxOptions{})
	dcAction(t, outcome, "unverified")
	assertContains(t, outcome.Message, "decision unverified (jev timeout)")
	assertEqual(t, at(t, outcome.Record, "gate"), "decide")
}

// py: tests/test_decide.py::test_only_ledger_entries_backed_by_a_ts_decide_call_this_turn_count
func TestOnlyLedgerEntriesBackedByATsDecideCallThisTurnCount(t *testing.T) {
	setUp(t)
	turn := []transcript.ToolCall{
		{ID: "toolu_a", Name: "Bash", Input: `{"command": "ts-decide '{}'"}`, Output: "ts-decide verdict: ADVISORY"},
		{ID: "toolu_b", Name: "Bash", Input: `{"command": "cat /x/verdicts/toolu_b.txt"}`, Output: "ts-decide verdict: ADVISORY"},
		{ID: "toolu_c", Name: "Bash", Input: `{"command": "ls"}`}}
	var rows []*json.Object
	for _, id := range []string{"toolu_a", "toolu_b", "toolu_c", "toolu_forged"} {
		rows = append(rows, json.NewObject("gate", "decide", "tool_use_id", id, "action", "advisory"))
	}
	tr := &transcript.Transcript{UserMessages: []string{"go"}, ToolCalls: turn, TurnCalls: turn}
	consulted, err := gates.Consulted(makeCtx(t, ctxOptions{transcript: tr, entries: rows}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []any
	for _, entry := range consulted {
		ids = append(ids, entry.Value("tool_use_id"))
	}
	assertEqual(t, ids, []any{"toolu_a", "toolu_b"})
}
