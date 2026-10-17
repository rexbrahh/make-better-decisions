package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

func TestDeepScrubReturnsNoPartialValue(t *testing.T) {
	var deep any = "secret-value-0123456789"
	for range 1100 {
		deep = []any{deep}
	}
	input := json.NewObject("first", "secret-value-0123456789", "deep", deep)
	value, err := Scrub(input, []string{"secret-value-0123456789"}, 0)
	var recursion *json.RecursionError
	if value != nil || !errors.As(err, &recursion) {
		t.Fatalf("deep scrub returned a partial value or no nesting error: value present %v, error %v", value != nil, err)
	}
}

// hookFrame is the Python stack depth of build when hook.py's _context calls it.
const hookFrame = 7

// stateDir returns a fresh resolved directory, with AGENT_DECISIONS_STATE inside it, as StateTestCase sets up.
func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	return dir
}

// tr returns Transcript(users, calls, calls, "", brief), the _transcript helper of test_evidence.py.
func tr(users []string, calls []transcript.ToolCall, brief *string) transcript.Transcript {
	return transcript.Transcript{UserMessages: users, ToolCalls: calls, TurnCalls: calls, Brief: brief}
}

func build(t *testing.T, tx transcript.Transcript, secrets []string, o Options) *json.Object {
	t.Helper()
	if o.Frame == 0 {
		o.Frame = hookFrame
	}
	bundle, err := Build(tx, secrets, o)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// dumpedChars returns len(json.dumps(v)).
func dumpedChars(t *testing.T, v any, sortKeys bool) int {
	t.Helper()
	text, err := json.Dumps(v, json.Options{SortKeys: sortKeys})
	if err != nil {
		t.Fatal(err)
	}
	return pystr.Len(text)
}

func objects(bundle *json.Object, key string) []*json.Object {
	list, _ := bundle.Value(key).([]any)
	result := make([]*json.Object, len(list))
	for i, item := range list {
		result[i] = item.(*json.Object)
	}
	return result
}

func field(items []*json.Object, key string) []string {
	values := make([]string, len(items))
	for i, item := range items {
		values[i], _ = item.Value(key).(string)
	}
	return values
}

func strs(bundle *json.Object, key string) []string {
	list, _ := bundle.Value(key).([]any)
	values := make([]string, len(list))
	for i, item := range list {
		values[i] = item.(string)
	}
	return values
}

func metadataInt(t *testing.T, bundle *json.Object, key string) int64 {
	t.Helper()
	value, ok := bundle.Value("metadata").(*json.Object).Value(key).(json.Int)
	if !ok {
		t.Fatalf("metadata[%q] is not an int", key)
	}
	n, _ := value.Int64()
	return n
}

func sha256Text(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func call(id, name, input, output string) transcript.ToolCall {
	return transcript.ToolCall{ID: id, Name: name, Input: input, Output: output}
}

// py: tests/test_evidence.py::test_encoded_budget_preserves_escaped_text_and_call_order
func TestEncodedBudgetPreservesEscapedTextAndCallOrder(t *testing.T) {
	text := strings.Repeat("\"\\\nλ", 3_000)
	var calls []transcript.ToolCall
	for i := range 25 {
		calls = append(calls, call(fmt.Sprintf("call%d", i), "Read", fmt.Sprintf("source%d.py", i), text))
	}
	bundle := build(t, tr([]string{"Preserve the current instructions."}, calls, nil), nil, Options{})
	if n := dumpedChars(t, bundle, false); n > policy.EvidenceChars {
		t.Fatalf("bundle has %d characters", n)
	}
	retained := objects(bundle, "tool_calls_newest_first")
	if retained[0].Value("id") != "call24" {
		t.Fatalf("newest call is %v", retained[0].Value("id"))
	}
	for _, output := range field(retained, "output") {
		if output != text {
			t.Fatal("a retained output was changed")
		}
	}
}

// py: tests/test_evidence.py::test_large_discarded_tool_outputs_do_not_dominate_context_building
func TestLargeDiscardedToolOutputsDoNotDominateContextBuilding(t *testing.T) {
	log := strings.Repeat("unrelated historical output ", 80_000)
	var calls []transcript.ToolCall
	for i := range 30 {
		calls = append(calls, call(fmt.Sprintf("old%d", i), "Bash", "read unrelated log", log))
	}
	calls = append(calls, call("latest-check", "Bash", "run request tests", "request tests passed"))
	started := time.Now()
	bundle := build(t, tr([]string{"Verify the request interface."}, calls, nil), nil, Options{Query: "request interface"})
	elapsed := time.Since(started)
	if !slices.Contains(field(objects(bundle, "tool_calls_newest_first"), "id"), "latest-check") {
		t.Fatal("latest-check was dropped")
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("build took %v", elapsed)
	}
}

// py: tests/test_evidence.py::test_recent_diagnostic_survives_older_broad_observations
func TestRecentDiagnosticSurvivesOlderBroadObservations(t *testing.T) {
	query := "launcher startup capture stream denial JSON hook"
	var calls []transcript.ToolCall
	for i := range 80 {
		calls = append(calls, call(fmt.Sprintf("old%d", i), "Read", query, query+strings.Repeat("x", 1_500)))
	}
	calls = append(calls, call("latest", "Bash", "run comparison", "capture=valid; stream=invalid\n"+strings.Repeat("y", 5_000)))
	bundle := build(t, tr([]string{"Preserve healthy denials."}, calls, nil), nil, Options{Query: query})
	if !slices.Contains(field(objects(bundle, "tool_calls_newest_first"), "id"), "latest") {
		t.Fatal("latest was dropped")
	}
	if n := dumpedChars(t, bundle, false); n > policy.EvidenceChars {
		t.Fatalf("bundle has %d characters", n)
	}
}

// py: tests/test_evidence.py::test_budget_drops_oldest_tool_calls_and_keeps_user_messages
func TestBudgetDropsOldestToolCallsAndKeepsUserMessages(t *testing.T) {
	var users []string
	for i := range 7 {
		users = append(users, fmt.Sprintf("ask %d", i))
	}
	var calls []transcript.ToolCall
	for i := range 20 {
		calls = append(calls, call(fmt.Sprintf("t%d", i), "Bash", fmt.Sprintf("cmd %d", i), strings.Repeat("y", 15_000)))
	}
	bundle := build(t, tr(users, calls, nil), nil, Options{})
	kept := strs(bundle, "user_messages")
	for _, message := range append(users[len(users)-policy.UserMessages:], users[0]) {
		if !slices.Contains(kept, message) {
			t.Fatalf("user message %q was dropped", message)
		}
	}
	retained := objects(bundle, "tool_calls_newest_first")
	if len(retained) == 0 || len(retained) >= 20 {
		t.Fatalf("kept %d tool calls", len(retained))
	}
	newest := retained[0]
	if newest.Value("input") != "cmd 19" || newest.Value("id") != "t19" || newest.Value("trust") != "unverified_tool_observation" {
		t.Fatalf("newest call is %v %v %v", newest.Value("id"), newest.Value("input"), newest.Value("trust"))
	}
	if n := dumpedChars(t, bundle, false); n > policy.EvidenceChars {
		t.Fatalf("bundle has %d characters", n)
	}
}

// py: tests/test_evidence.py::test_leaves_out_ts_decide_calls
func TestLeavesOutTsDecideCalls(t *testing.T) {
	calls := []transcript.ToolCall{
		call("t1", "Bash", `{"command": "ts-decide '{}'"}`, "verdict"),
		call("t2", "Bash", `{"command": "ls"}`, "a.txt"),
	}
	bundle := build(t, tr([]string{"go"}, calls, nil), nil, Options{})
	if got := field(objects(bundle, "tool_calls_newest_first"), "output"); !slices.Equal(got, []string{"a.txt"}) {
		t.Fatalf("outputs are %q", got)
	}
}

// py: tests/test_evidence.py::test_clips_long_texts_including_user_messages
func TestClipsLongTextsIncludingUserMessages(t *testing.T) {
	long := "head " + strings.Repeat("m", 50_000) + " tail"
	bundle := build(t, tr([]string{long}, []transcript.ToolCall{call("t", "Read", long, long)}, nil), nil, Options{})
	for _, text := range []string{strs(bundle, "user_messages")[0], field(objects(bundle, "tool_calls_newest_first"), "output")[0]} {
		if pystr.Len(text) > policy.ClipChars+100 || !strings.HasPrefix(text, "head ") || !strings.HasSuffix(text, " tail") {
			t.Fatalf("clipped text has %d characters", pystr.Len(text))
		}
	}
}

// py: tests/test_evidence.py::test_brief_is_labelled
func TestBriefIsLabelled(t *testing.T) {
	brief := "find the leak"
	bundle := build(t, tr(nil, nil, &brief), nil, Options{})
	if bundle.Value("brief") != brief || bundle.Value("brief_trust") != "unverified_parent_agent_brief" {
		t.Fatalf("brief is %v, %v", bundle.Value("brief"), bundle.Value("brief_trust"))
	}
}

// py: tests/test_evidence.py::test_original_constraint_survives_newer_chatter
func TestOriginalConstraintSurvivesNewerChatter(t *testing.T) {
	constraint := "Preserve atomic writes in every implementation."
	users := []string{constraint}
	for i := range 40 {
		users = append(users, fmt.Sprintf("Progress update %d", i))
	}
	bundle := build(t, tr(users, nil, nil), nil, Options{})
	if !slices.Contains(strs(bundle, "user_messages"), constraint) {
		t.Fatal("the constraint was dropped")
	}
	if total, omitted := metadataInt(t, bundle, "user_messages_total"), metadataInt(t, bundle, "user_messages_omitted"); total != 41 || omitted != 0 {
		t.Fatalf("total %d, omitted %d", total, omitted)
	}
}

// py: tests/test_evidence.py::test_more_than_twenty_short_constraints_are_preserved_when_budget_fits
func TestMoreThanTwentyShortConstraintsArePreservedWhenBudgetFits(t *testing.T) {
	var users []string
	for i := range 35 {
		users = append(users, fmt.Sprintf("Constraint %d: must preserve behaviour %d.", i, i))
	}
	bundle := build(t, tr(users, nil, nil), nil, Options{})
	if got := strs(bundle, "user_messages"); !slices.Equal(got, users) {
		t.Fatalf("user messages are %q", got)
	}
	if omitted := metadataInt(t, bundle, "user_messages_omitted"); omitted != 0 {
		t.Fatalf("omitted %d", omitted)
	}
}

// py: tests/test_evidence.py::test_query_relevance_beats_newer_irrelevant_tool_spam
func TestQueryRelevanceBeatsNewerIrrelevantToolSpam(t *testing.T) {
	calls := []transcript.ToolCall{call("old", "Read", "agent_decisions/evidence.py", "atomic writes are preserved")}
	for i := range 20 {
		calls = append(calls, call(fmt.Sprintf("spam%d", i), "Bash", fmt.Sprintf("cmd %d", i), strings.Repeat("noise", 8_000)))
	}
	bundle := build(t, tr([]string{"Investigate atomic writes."}, calls, nil), nil, Options{Query: "atomic writes"})
	if !slices.Contains(field(objects(bundle, "tool_calls_newest_first"), "id"), "old") {
		t.Fatal("the relevant call was dropped")
	}
	if n := dumpedChars(t, bundle, false); n > policy.EvidenceChars {
		t.Fatalf("bundle has %d characters", n)
	}
}

// py: tests/test_evidence.py::test_redacts_secrets
func TestRedactsSecrets(t *testing.T) {
	samples := []string{
		"Authorization: Bearer " + strings.Repeat("A", 30),
		"key sk-" + strings.Repeat("b", 30),
		"token ghp_" + strings.Repeat("c", 36),
		"slack xoxb-" + "1234567890-abcdef",
		"aws AKIA" + "ABCDEFGHIJKLMNOP",
		"jwt eyJ" + strings.Repeat("a", 20) + ".eyJ" + strings.Repeat("b", 20) + "." + strings.Repeat("c", 20),
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
	}
	for _, sample := range samples {
		if !strings.Contains(Redact(sample, nil), Redacted) {
			t.Errorf("%q was not redacted", sample[:12])
		}
	}
	if got := Redact("TYPESAFE_API_KEY=abcdefgh12345", nil); got != "TYPESAFE_API_KEY="+Redacted {
		t.Errorf("assignment redacts to %q", got)
	}
	if got := Redact("plain text stays", nil); got != "plain text stays" {
		t.Errorf("plain text redacts to %q", got)
	}
}

// py: tests/test_evidence.py::test_redacts_known_secret_literals_everywhere
func TestRedactsKnownSecretLiteralsEverywhere(t *testing.T) {
	key := "zz-literal-secret-0123456789"
	bundle := build(t, tr([]string{"my key is " + key}, []transcript.ToolCall{call("t", "Bash", key, key)}, nil),
		[]string{key}, Options{})
	text, err := json.Dumps(bundle, json.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, key) {
		t.Fatal("the secret literal survived")
	}
}

func TestContainedKnownSecretsAreFullyRedacted(t *testing.T) {
	for name, short := range map[string]string{"ascii": "synthetic-key-common-0123456789", "unicode": strings.Repeat("é", 16)} {
		for position, long := range map[string]string{
			"prefix": short + "-private-suffix", "suffix": "private-prefix-" + short,
			"interior": "private-prefix-" + short + "-private-suffix",
		} {
			for order, keys := range map[string][]string{
				"short_first": {short, long}, "long_first": {long, short}, "duplicates": {short, short, long, long},
			} {
				t.Run(name+"/"+position+"/"+order, func(t *testing.T) {
					original := slices.Clone(keys)
					text := "before " + long + " after " + short + " end"
					want := "before " + Redacted + " after " + Redacted + " end"
					if got := Redact(text, keys); got != want {
						t.Fatalf("redacted text = %q, want %q", got, want)
					}
					value, err := Scrub(json.NewObject("observation", text), keys, 0)
					mustDo(t, err)
					if value.(*json.Object).Value("observation") != want {
						t.Fatal("scrub retained part of a known secret")
					}
					if !slices.Equal(keys, original) {
						t.Fatal("redaction changed the caller's key order")
					}
				})
			}
		}
	}
}

func TestKnownSecretRedactionPreservesTheCodePointThreshold(t *testing.T) {
	short := strings.Repeat("é", 15)
	long := "x" + short
	keys := []string{short, long}
	if got := Redact(short+" "+long, keys); got != short+" "+Redacted {
		t.Fatalf("redaction changed the 16-code-point threshold: %q", got)
	}
}

// writeJSONL writes records as json.dumps lines, as tests/helpers.py write_jsonl does.
func writeJSONL(t *testing.T, path string, records []any) string {
	t.Helper()
	var text strings.Builder
	for _, record := range records {
		line, err := json.Dumps(record, json.Options{})
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(line + "\n")
	}
	if err := os.WriteFile(path, []byte(text.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// codexUser is tests/helpers.py codex_user.
func codexUser(text string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "message", "role", "user", "content", []any{json.NewObject("type", "input_text", "text", text)}))
}

// py: tests/test_continuity.py::test_native_command_retains_cwd_and_exit_status_as_observations
func TestNativeCommandRetainsCwdAndExitStatusAsObservations(t *testing.T) {
	dir := stateDir(t)
	item := json.NewObject("type", "CommandExecution", "id", "check",
		"command", []any{"/bin/sh", "-c", "printf 'tests passed'"}, "cwd", "file:///tmp/repo",
		"status", "completed", "exit_code", json.IntOf(1), "aggregated_output", "tests passed")
	path := writeJSONL(t, filepath.Join(dir, "command.jsonl"), []any{codexUser("Check the tests."),
		json.NewObject("type", "event_msg", "payload", json.NewObject("type", "item_completed", "item", item))})
	loaded, err := transcript.Load(path, "codex")
	if err != nil {
		t.Fatal(err)
	}
	observation := objects(build(t, loaded, nil, Options{}), "tool_calls_newest_first")[0]
	execution := observation.Value("execution").(*json.Object)
	if execution.Value("exit_code") != json.IntOf(1) || execution.Value("cwd") != item.Value("cwd") ||
		observation.Value("trust") != "unverified_tool_observation" {
		t.Fatalf("observation is %v %v %v", execution.Value("exit_code"), execution.Value("cwd"), observation.Value("trust"))
	}
}

// git runs git in dir, as test_continuity.py's helper does, and returns its stripped stdout.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)) // ASCII
}

// commitFixture makes dir a git repository whose HEAD holds the files given.
func commitFixture(t *testing.T, dir string, files ...string) string {
	t.Helper()
	git(t, dir, "init", "-q")
	git(t, dir, append([]string{"add"}, files...)...)
	git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false",
		"commit", "-qm", "fixture")
	return git(t, dir, "rev-parse", "HEAD")
}

// py: tests/test_continuity.py::test_current_source_is_distinguished_from_its_git_baseline
func TestCurrentSourceIsDistinguishedFromItsGitBaseline(t *testing.T) {
	dir := stateDir(t)
	contract := filepath.Join(dir, "contract.md")
	if err := os.WriteFile(contract, []byte("Retain active leases until work ends.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revision := commitFixture(t, dir, "contract.md")
	baseline := sha256Text("Retain active leases until work ends.\n")
	if err := os.WriteFile(contract, []byte("Changed lease contract.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := build(t, tr([]string{"Inspect the lease contract."}, nil, nil), nil, Options{Cwd: &dir, Query: "contract.md"})
	item := firstSource(t, bundle)
	gitBaseline := item.Value("git_baseline").(*json.Object)
	if item.Value("role") != "working_tree_source" || gitBaseline.Value("revision") != revision ||
		gitBaseline.Value("sha256") != baseline || gitBaseline.Value("differs_from_worktree") != true {
		t.Fatalf("item is %v with baseline %v %v %v", item.Value("role"), gitBaseline.Value("revision"),
			gitBaseline.Value("sha256"), gitBaseline.Value("differs_from_worktree"))
	}
}
