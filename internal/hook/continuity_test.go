package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// rcContinuity is a ContinuityTest fixture: its directory, contract and rollout paths.
type rcContinuity struct {
	dir, contract, path string
}

// rcContinuitySetUp ports ContinuityTest.setUp.
func rcContinuitySetUp(t *testing.T) rcContinuity {
	t.Helper()
	dir := setUp(t)
	contract := filepath.Join(dir, "contract.md")
	writeFile(t, contract, "Retain active leases until work ends.\n")
	path := writeJSONL(t, dir, "rollout.jsonl", []*json.Object{codexUser("Implement the lease contract.")}, "")
	return rcContinuity{dir, contract, path}
}

// payload ports ContinuityTest.payload; a nil value is {"file_path": "parser.py"}.
func (c rcContinuity) payload(name string, value *json.Object, session string) *json.Object {
	if value == nil {
		value = json.NewObject("file_path", "parser.py")
	}
	return json.NewObject("session_id", session, "tool_use_id", "current", "tool_name", name,
		"tool_input", value, "transcript_path", c.path, "cwd", c.dir)
}

// completedReview ports ContinuityTest.completed_review.
func (c rcContinuity) completedReview(t *testing.T, contracts bool) *gates.Context {
	t.Helper()
	paths := []any{}
	if contracts {
		paths = []any{"contract.md"}
	}
	request := json.NewObject("decision", "Which lease representation should parsing use?", "options", []any{
		json.NewObject("id", "shared", "description", "Reference-counted lease"),
		json.NewObject("id", "owned", "description", "Single-owner lease")},
		"scope", json.NewObject("paths", []any{"parser.py"}, "invariants", []any{"Retain active leases until work ends."}),
		"contracts", paths)
	value := json.NewObject("command", "ts-decide <<'EOF'\n"+dumps(t, request)+"\nEOF")
	payload := with(c.payload("Bash", value, "s1"), "tool_use_id", "review")
	result := handle(t, "pre_tool_use", payload, "codex", fake(map[string]any{"sufficient": .95,
		"owner": "agent", "invariant_0_shared": .95, "invariant_0_owned": .95}).Post)
	rendered := verdictOutput(t, c.dir, result, "command")
	writeJSONL(t, c.dir, "rollout.jsonl", []*json.Object{codexUser("Implement the lease contract."),
		codexCall("review", "Bash", dumps(t, value)), codexOutput("review", rendered)}, "")
	return rcContext(t, c.payload("Edit", nil, "s1"), "codex")
}

func rcSha256(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(readFile(t, path)))
	return hex.EncodeToString(sum[:])
}

// rcSummaries returns gates._decision_summaries(ctx, json.dumps({"file_path": "parser.py"})). That function
// is unexported in Go; the edit gate sends exactly these summaries for that input as state["decisions"].
func rcSummaries(t *testing.T, ctx *gates.Context) []any {
	t.Helper()
	f := fake(nil)
	if _, err := gates.EditGate("Edit", json.NewObject("file_path", "parser.py"), *ctx, gatesAsker(f), gateFrame); err != nil {
		t.Fatal(err)
	}
	requests := f.Requests()
	if len(requests) == 0 {
		t.Fatal("the edit gate asked nothing")
	}
	return at(t, requests[0], "state", "decisions").([]any)
}

// rcHistory returns ctx.evidence.get("architecture_history", []).
func rcHistory(ctx *gates.Context) []any {
	history, _ := ctx.Evidence.Value("architecture_history").([]any)
	return history
}

// py: tests/test_continuity.py::test_contract_hash_is_captured_from_the_current_file
func TestContractHashIsCapturedFromTheCurrentFile(t *testing.T) {
	c := rcContinuitySetUp(t)
	ctx := c.completedReview(t, true)
	for _, entry := range ctx.Entries {
		if entry.Value("gate") == "decide" {
			assertEqual(t, entry.Value("contracts"), json.NewObject("contract.md", rcSha256(t, c.contract)))
			return
		}
	}
	t.Fatal("no decide entry")
}

// py: tests/test_continuity.py::test_completed_contract_review_becomes_durable_background_evidence
func TestCompletedContractReviewBecomesDurableBackgroundEvidence(t *testing.T) {
	c := rcContinuitySetUp(t)
	c.completedReview(t, true)
	repoID, err := repositoryID(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	path, err := ledger.ArchitecturePath(repoID)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Duration(policy.RetainDays+1) * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Prune(); err != nil {
		t.Fatal(err)
	}
	ctx := rcContext(t, c.payload("Edit", nil, "fresh"), "codex")
	history := rcHistory(ctx)
	if len(history) == 0 {
		t.Fatal("no architecture history")
	}
	assertEqual(t, at(t, history[0], "contract_status", "contract.md"), "unchanged")
	assertEqual(t, at(t, history[0], "authorizes_write"), false)
	if completed := rcConsulted(t, *ctx); len(completed) != 0 {
		t.Fatalf("consulted %s", dumps(t, completed))
	}
	certain := func(state, questions *json.Object) (*json.Object, error) {
		return json.NewObject("writes_files", json.NewObject("noul", 1.0),
			"needs_decision", json.NewObject("noul", 1.0), "covered", json.NewObject("noul", 1.0)), nil
	}
	denied, err := gates.EditGate("Edit", json.NewObject("file_path", "parser.py"), *ctx, certain, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, denied.Action, "deny")
}

// py: tests/test_continuity.py::test_changed_contract_marks_history_stale_without_hiding_the_old_hash
func TestChangedContractMarksHistoryStaleWithoutHidingTheOldHash(t *testing.T) {
	c := rcContinuitySetUp(t)
	c.completedReview(t, true)
	oldHash := rcSha256(t, c.contract)
	writeFile(t, c.contract, "Release leases after Flush.\n")
	history := rcHistory(rcContext(t, c.payload("Edit", nil, "fresh"), "codex"))
	if len(history) == 0 {
		t.Fatal("no architecture history")
	}
	assertEqual(t, at(t, history[0], "contract_status", "contract.md"), "changed")
	assertEqual(t, at(t, history[0], "contracts", "contract.md"), oldHash)
}

// py: tests/test_continuity.py::test_contract_history_is_repository_scoped
func TestContractHistoryIsRepositoryScoped(t *testing.T) {
	c := rcContinuitySetUp(t)
	c.completedReview(t, true)
	other := filepath.Join(c.dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := rcContext(t, with(c.payload("Edit", nil, "fresh"), "cwd", other), "codex")
	if history := rcHistory(ctx); len(history) != 0 {
		t.Fatalf("history %s", dumps(t, history))
	}
}

// py: tests/test_continuity.py::test_changed_contract_invalidates_the_current_approach_receipt
func TestChangedContractInvalidatesTheCurrentApproachReceipt(t *testing.T) {
	c := rcContinuitySetUp(t)
	c.completedReview(t, true)
	writeFile(t, c.contract, "Release leases after Flush.\n")
	ctx := rcContext(t, c.payload("Edit", nil, "s1"), "codex")
	if summaries := rcSummaries(t, ctx); len(summaries) != 0 {
		t.Fatalf("summaries %s", dumps(t, summaries))
	}
}

// py: tests/test_continuity.py::test_changed_scoped_source_invalidates_coverage_without_declared_contracts
func TestChangedScopedSourceInvalidatesCoverageWithoutDeclaredContracts(t *testing.T) {
	c := rcContinuitySetUp(t)
	source := filepath.Join(c.dir, "parser.py")
	writeFile(t, source, "def parse(owner): return owner.acquire()\n")
	ctx := c.completedReview(t, false)
	if summaries := rcSummaries(t, ctx); len(summaries) != 1 {
		t.Fatalf("summaries %s", dumps(t, summaries))
	}
	writeFile(t, source, "def parse(owner): return None\n")
	ctx = rcContext(t, c.payload("Edit", nil, "s1"), "codex")
	if summaries := rcSummaries(t, ctx); len(summaries) != 0 {
		t.Fatalf("summaries %s", dumps(t, summaries))
	}
	plan, err := gates.PlanGate(*ctx, nil, nil, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, plan.Action, "deny")
	f := fake(map[string]any{"made_choice": 1.0, "follows_pick": 1.0})
	outcome, err := gates.StopGate("I selected reference-counted leases.", false, *ctx, asker(f.Post, nil, planAsk), gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, outcome.Action, "block")
	assertEqual(t, at(t, f.Requests()[0], "state", "decisions"), []any{})
}

// py: tests/test_continuity.py::test_unavailable_scoped_source_is_ineligible_but_routine_work_still_uses_its_classifier
func TestUnavailableScopedSourceIsIneligibleButRoutineWorkStillUsesItsClassifier(t *testing.T) {
	c := rcContinuitySetUp(t)
	source := filepath.Join(c.dir, "parser.py")
	writeFile(t, source, "def parse(owner): return owner.acquire()\n")
	c.completedReview(t, false)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	ctx := rcContext(t, c.payload("Edit", nil, "s1"), "codex")
	if summaries := rcSummaries(t, ctx); len(summaries) != 0 {
		t.Fatalf("summaries %s", dumps(t, summaries))
	}
	f := fake(map[string]any{"needs_decision": .01, "writes_files": .99})
	outcome, err := gates.EditGate("Edit", json.NewObject("file_path", "tests/test_parser.py"), *ctx,
		asker(f.Post, nil, editAsk), gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, outcome.Action, "allow")
}

// py: tests/test_continuity.py::test_missing_contract_cannot_create_a_receipt
func TestMissingContractCannotCreateAReceipt(t *testing.T) {
	c := rcContinuitySetUp(t)
	request := json.NewObject("decision", "Which lease representation?", "contracts", []any{"missing.md"},
		"options", []any{json.NewObject("id", "shared", "description", "Reference-counted lease"),
			json.NewObject("id", "owned", "description", "Single-owner lease")})
	ctx := rcContext(t, c.payload("Edit", nil, "s1"), "codex")
	f := fake(map[string]any{"sufficient": .95})
	result, err := gates.Decide(dumps(t, request), *ctx, asker(f.Post, nil, decideAsk))
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, result.Action, "gather_evidence")
	assertContains(t, result.Message, "missing.md")
	assertContains(t, result.Message, "source root: "+dumps(t, c.dir))
	if _, ok := result.Record.Get("receipt_id"); ok {
		t.Fatalf("record has a receipt: %s", dumps(t, result.Record))
	}
	if requests := f.Requests(); len(requests) != 0 {
		t.Fatalf("%d requests", len(requests))
	}
}

// py: tests/test_continuity.py::test_historical_records_are_signed_and_tampering_is_ignored
func TestHistoricalRecordsAreSignedAndTamperingIsIgnored(t *testing.T) {
	c := rcContinuitySetUp(t)
	c.completedReview(t, true)
	repoID, err := repositoryID(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	path, err := ledger.ArchitecturePath(repoID)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(readFile(t, path), "\n"), "\n")
	row := obj(t, lines[0])
	at(t, row, "entry").(*json.Object).Set("selected_approach", "Delete all leases")
	writeFile(t, path, dumps(t, row)+"\n")
	ctx := rcContext(t, c.payload("Edit", nil, "fresh"), "codex")
	if history := rcHistory(ctx); len(history) != 0 {
		t.Fatalf("history %s", dumps(t, history))
	}
}
