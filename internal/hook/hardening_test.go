package hook

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// edSecret is test_hardening.py's SECRET.
const edSecret = "sk-abcdefghijklmnopqrstuvwx"

// edHardeningRequest is test_hardening.py's REQUEST.
func edHardeningRequest() *json.Object {
	return json.NewObject("decision", "Which cache?", "options", []any{
		json.NewObject("id", "lru", "description", "LRU"), json.NewObject("id", "lfu", "description", "LFU")},
		"facts", []any{})
}

// --- GuardTest

// py: tests/test_hardening.py::test_indirect_and_case_variant_paths_are_denied
func TestIndirectAndCaseVariantPathsAreDenied(t *testing.T) {
	setUp(t)
	commands := []string{"cd ~/.local/state && cat agent-decisions/sessions/*", "ls ~/.local/state/agent*"}
	if runtime.GOOS == "darwin" { // only darwin casefolds: a case variant is another directory on linux
		commands = append(commands, "cat ~/.Local/State/Agent-Decisions/sessions/x")
	}
	for _, command := range commands {
		outcome, err := gates.Guard(json.NewObject("command", command), nil, guardFrame)
		if err != nil {
			t.Fatal(err)
		}
		if outcome == nil {
			t.Errorf("%q was not denied", command)
		}
	}
}

// py: tests/test_hardening.py::test_read_tools_are_guarded
func TestReadToolsAreGuarded(t *testing.T) {
	setUp(t)
	payload := json.NewObject("session_id", "s1", "tool_name", "Read", "tool_use_id", "toolu_r",
		"tool_input", json.NewObject("file_path", stateDir(t)+"/sessions/s1.jsonl"))
	assertEqual(t, decision(t, handle(t, "pre_tool_use", payload, "claude", fake(nil).Post)), "deny")
}

// py: tests/test_hardening.py::test_denial_survives_an_unwritable_ledger
//
// Python patches ledger.append to raise OSError; Go makes the state directory a regular file, so that
// ledger.Append raises NotADirectoryError.
func TestDenialSurvivesAnUnwritableLedger(t *testing.T) {
	dir := setUp(t)
	state := filepath.Join(dir, "state-file")
	writeFile(t, state, "")
	t.Setenv("AGENT_DECISIONS_STATE", state)
	if err := ledger.Append(json.NewObject("session_id", "s1")); !errors.As(err, new(*pyerr.OSError)) {
		t.Fatalf("ledger.Append returned %v, want an OSError", err)
	}
	payload := json.NewObject("session_id", "s1", "tool_name", "Bash", "tool_use_id", "toolu_g",
		"tool_input", json.NewObject("command", "cat ~/.local/state/agent-decisions/sessions/s1.jsonl"))
	assertEqual(t, decision(t, handle(t, "pre_tool_use", payload, "claude", fake(nil).Post)), "deny")
}

// --- LedgerEncodingTest

// py: tests/test_hardening.py::test_lone_surrogate_is_written_and_read_back
func TestLoneSurrogateIsWrittenAndReadBack(t *testing.T) {
	setUp(t)
	const surrogate = "\xed\xa0\x80" // "\ud800" in WTF-8
	if err := ledger.Append(json.NewObject("session_id", "s1", "gate", "decide", "text", surrogate)); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, entries(t, "s1")[0].Value("text"), surrogate)
	path, err := ledger.WriteVerdict("toolu_s", "x "+surrogate)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, readFile(t, path), `\ud800`)
}

// --- RedactionTest

// py: tests/test_hardening.py::test_option_descriptions_are_redacted_in_questions_and_verdict
func TestOptionDescriptionsAreRedactedInQuestionsAndVerdict(t *testing.T) {
	dir := setUp(t)
	path := writeJSONL(t, dir, "s1.jsonl", []*json.Object{human("Choose a config reader.")}, "")
	request := json.NewObject("decision", "Which config reader?", "options", []any{
		json.NewObject("id", "a", "description", "Use token "+edSecret),
		json.NewObject("id", "b", "description", "Use environment")}, "facts", []any{})
	api := fake(map[string]any{"pick": "b", "sufficient": 0.9, "owner": "agent"})
	out := handle(t, "pre_tool_use", json.NewObject("session_id", "s1", "transcript_path", path,
		"tool_name", "Bash", "tool_use_id", "redact1",
		"tool_input", json.NewObject("command", "ts-decide "+shlex.Quote(dumps(t, request)))), "claude", api.Post)
	requests := make([]any, 0)
	for _, r := range api.Requests() {
		requests = append(requests, r)
	}
	assertNotContains(t, dumps(t, requests), edSecret)
	words, err := shlex.Split(str(t, out, "hookSpecificOutput", "updatedInput", "command"))
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, words[2], edSecret)
}

// py: tests/test_hardening.py::test_json_style_secret_is_redacted
func TestJsonStyleSecretIsRedacted(t *testing.T) {
	assertNotContains(t, evidence.Redact(`{"api_key": "abcdefghijkl1234"}`, nil), "abcdefghijkl1234")
}

// py: tests/test_hardening.py::test_unterminated_key_blocks_redact_in_linear_time
func TestUnterminatedKeyBlocksRedactInLinearTime(t *testing.T) {
	text := strings.Repeat("-----BEGIN PRIVATE KEY-----\nabc\n", 20_000)
	start := time.Now()
	evidence.Redact(text, nil)
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("redact took %s", elapsed)
	}
}

// py: tests/test_hardening.py::test_agent_written_facts_are_redacted_before_jev_and_the_ledger
func TestAgentWrittenFactsAreRedactedBeforeJevAndTheLedger(t *testing.T) {
	dir := setUp(t)
	path := writeJSONL(t, dir, "s1.jsonl", []*json.Object{
		human("speed up the tile cache"),
		toolUse("toolu_1", "Bash", json.NewObject("command", "env")),
		toolResult("toolu_1", "export TOKEN "+edSecret),
	}, "")
	api := fake(map[string]any{"pick": "lfu", "sufficient": 0.9, "owner": "agent"})
	handle(t, "pre_tool_use", json.NewObject("session_id", "s1", "transcript_path", path, "tool_name", "Bash",
		"tool_use_id", "toolu_d",
		"tool_input", json.NewObject("command", "ts-decide "+shlex.Quote(dumps(t, edHardeningRequest())))),
		"claude", api.Post)
	requests := make([]any, 0)
	for _, r := range api.Requests() {
		requests = append(requests, r)
	}
	assertNotContains(t, dumps(t, requests), edSecret)
	assertNotContains(t, readFile(t, filepath.Join(stateDir(t), "sessions", "s1.jsonl")), edSecret)
}

// --- ConsultedTest

// py: tests/test_hardening.py::test_a_comment_naming_ts_decide_does_not_hide_a_call
func TestACommentNamingTsDecideDoesNotHideACall(t *testing.T) {
	isDecide, err := transcript.IsDecide(transcript.ToolCall{ID: "a", Name: "Bash",
		Input: `{"command": "grep -rn foo src  # ts-decide"}`})
	if err != nil {
		t.Fatal(err)
	}
	if isDecide {
		t.Fatal("a comment made a ts-decide call")
	}
}

// py: tests/test_hardening.py::test_only_a_real_verdict_counts_as_consulted
func TestOnlyARealVerdictCountsAsConsulted(t *testing.T) {
	setUp(t)
	ctx := func(action string) gates.Context {
		output := "ts-decide verdict: " + strings.ReplaceAll(strings.ToUpper(action), "_", " ") // ASCII actions
		if action == "unverified" {
			output = "ts-decide: decision unverified (jev timeout)"
		}
		call := transcript.ToolCall{ID: "toolu_d", Name: "Bash", Input: `{"command": "ts-decide '{}'"}`, Output: output}
		tr := transcript.Transcript{UserMessages: []string{"fix"}, ToolCalls: []transcript.ToolCall{call},
			TurnCalls: []transcript.ToolCall{call}}
		return makeCtx(t, ctxOptions{transcript: &tr, entries: []*json.Object{
			json.NewObject("gate", "decide", "tool_use_id", "toolu_d", "action", action)}})
	}
	for _, c := range []struct {
		action string
		counts bool
	}{{"refused", false}, {"gather_evidence", false}, {"unverified", true}, {"advisory", true},
		{"binding", true}, {"ask_user", true}} {
		consulted, err := gates.Consulted(ctx(c.action))
		if err != nil {
			t.Fatal(err)
		}
		if (len(consulted) > 0) != c.counts {
			t.Errorf("%s: consulted %d, want counts=%v", c.action, len(consulted), c.counts)
		}
	}
}

// --- StopPerAgentTest

// py: tests/test_hardening.py::test_a_subagent_block_does_not_spend_the_main_agents_block
func TestASubagentBlockDoesNotSpendTheMainAgentsBlock(t *testing.T) {
	setUp(t)
	tr := transcript.Transcript{UserMessages: []string{"fix the cache"}}
	rows := []*json.Object{json.NewObject("gate", "stop", "action", "block", "turn", tr.TurnKey(), "agent_id", "a1")}
	outcome, err := gates.StopGate("I chose LRU over LFU.", false, makeCtx(t, ctxOptions{transcript: &tr, entries: rows}),
		gatesAsker(fake(map[string]any{"made_choice": 0.9})), gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	edAction(t, outcome, "block")
}
