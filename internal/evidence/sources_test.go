package evidence

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
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

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dumps(t *testing.T, v any) string {
	t.Helper()
	text, err := json.Dumps(v, json.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func sourcePaths(bundle *json.Object) []string {
	return field(objects(bundle, "source_files"), "path")
}

// firstSource returns bundle["source_files"][0], and fails the test when there is none.
func firstSource(t *testing.T, bundle *json.Object) *json.Object {
	t.Helper()
	sources := objects(bundle, "source_files")
	if len(sources) == 0 {
		t.Fatalf("no source files; metadata %s", dumps(t, bundle.Value("metadata")))
	}
	return sources[0]
}

func hashesByPath(bundle *json.Object) map[string]string {
	hashes := map[string]string{}
	for _, item := range objects(bundle, "source_files") {
		hashes[item.Value("path").(string)] = item.Value("sha256").(string)
	}
	return hashes
}

func checkBudget(t *testing.T, bundle *json.Object, sortKeys bool) {
	t.Helper()
	if n := dumpedChars(t, bundle, sortKeys); n > policy.EvidenceChars {
		t.Fatalf("bundle has %d characters", n)
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// patch replaces *target with value for the test, as mock.patch.object does.
func patch[T any](t *testing.T, target *T, value T) {
	original := *target
	*target = value
	t.Cleanup(func() { *target = original })
}

// py: tests/test_sources.py::test_all_requested_contracts_survive_a_full_evidence_budget
func TestAllRequestedContractsSurviveAFullEvidenceBudget(t *testing.T) {
	dir := stateDir(t)
	names := []string{"docs/architecture.md", "docs/distribution.md", "app/CMakeLists.txt",
		"ci/package-windows.ps1", "ci/test-installer-contract.ps1", "tests/shader.cmake"}
	hashes := map[string]string{}
	for _, name := range names {
		text := "Contract: " + name + "\n" + strings.Repeat("Preserve the existing behavior.\n", 900)
		writeFile(t, filepath.Join(dir, name), text)
		hashes[name] = sha256Text(text)
	}
	request := dumps(t, json.NewObject("decision", "Choose the shader producer wiring.", "contracts", names))
	users := []string{"Preserve source and outputs. " + strings.Repeat("u", 13_000),
		"Choose the existing producer wiring. " + strings.Repeat("v", 13_000)}
	bundle := build(t, transcript.Transcript{UserMessages: users}, nil, Options{Query: request, Cwd: &dir, Contracts: names})
	if got := hashesByPath(bundle); !maps.Equal(got, hashes) {
		t.Fatalf("source hashes are %v", got)
	}
	for _, trust := range field(objects(bundle, "source_files"), "trust") {
		if trust != "verified_file_observation" {
			t.Fatalf("trust is %q", trust)
		}
	}
	checkBudget(t, bundle, true)

	// Freshness metadata is added after the initial evidence build.
	observations := &json.Object{}
	for _, name := range names {
		observations.Set(name, json.NewObject("sha256", hashes[name]))
	}
	bundle.Set("contract_observations", observations)
	bundle.Set("file_observations", observations.Copy())
	mustDo(t, TrimToBudget(bundle, names, request, 0))
	if got := hashesByPath(bundle); !maps.Equal(got, hashes) {
		t.Fatalf("source hashes after trimming are %v", got)
	}
	checkBudget(t, bundle, true)
}

// py: tests/test_sources.py::test_requested_contract_precedes_candidate_limit_and_helper_observations
func TestRequestedContractPrecedesCandidateLimitAndHelperObservations(t *testing.T) {
	dir := stateDir(t)
	contract := strings.Repeat("Preserve every required interface.\n", 800)
	writeFile(t, filepath.Join(dir, "review-contract.md"), contract)
	var names []string
	for index := range 24 {
		name := fmt.Sprintf("helper_%d.cc", index)
		names = append(names, name)
		writeFile(t, filepath.Join(dir, name), "void helper();\n")
	}
	var calls []transcript.ToolCall
	for index := range 8 {
		calls = append(calls, call(fmt.Sprint(index), "exec_command", "cat "+strings.Join(names, " "),
			strings.Repeat("native helper output\n", 800)))
	}
	query := dumps(t, json.NewObject("decision", "Which route uses "+strings.Join(names, " "),
		"contracts", []any{"review-contract.md"}))
	tx := transcript.Transcript{UserMessages: []string{"Preserve all required interfaces."}, ToolCalls: calls, TurnCalls: calls}
	probes := 0
	original := resolveSource
	patch(t, &resolveSource, func(root, raw string) (string, bool, error) {
		probes++
		return original(root, raw)
	})
	bundle := build(t, tx, nil, Options{Query: query, Cwd: &dir, Contracts: []string{"review-contract.md"}})
	if probes > maxSourceCandidateProbes {
		t.Fatalf("%d probes", probes)
	}
	item := firstSource(t, bundle)
	if item.Value("path") != "review-contract.md" || item.Value("sha256") != sha256Text(contract) ||
		item.Value("trust") != "verified_file_observation" {
		t.Fatalf("first source is %v %v", item.Value("path"), item.Value("trust"))
	}
	checkBudget(t, bundle, true)
}

// py: tests/test_sources.py::test_peer_helper_reads_survive_history_and_a_different_consultation_root
func TestPeerHelperReadsSurviveHistoryAndADifferentConsultationRoot(t *testing.T) {
	dir := stateDir(t)
	data, err := os.ReadFile("../../tests/fixtures/windows-diagnostic-evidence.json")
	mustDo(t, err)
	loaded, err := json.Loads(string(data))
	mustDo(t, err)
	fixture := loaded.(*json.Object)
	worktree := filepath.Join(dir, "qualification")
	root := filepath.Join(dir, "main")
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		writeFile(t, filepath.Join(root, name), "Preserve project constraints.\n"+strings.Repeat("x", 16_000))
	}
	var records []any
	for _, record := range fixture.Value("observations").([]any) {
		replaced, err := json.Loads(strings.ReplaceAll(dumps(t, record), "{worktree}", worktree))
		mustDo(t, err)
		records = append(records, replaced)
	}
	codex, err := transcript.Load(writeJSONL(t, filepath.Join(dir, "codex.jsonl"), records), "codex")
	mustDo(t, err)
	calls := append(slices.Clone(codex.ToolCalls),
		call("unrelated-linux", "Bash", "run linux qualification", strings.Repeat("x", 12_000)),
		call("unrelated-log", "Bash", "read linux build log", strings.Repeat("z", 12_000)))
	request := fixture.Value("request").(*json.Object)
	users := []string{"Preserve source and diagnostics."}
	for range 20 {
		users = append(users, "Constraint: preserve interfaces. "+strings.Repeat("y", 8_000))
	}
	users = append(users, request.Value("facts").([]any)[0].(string))
	query := strings.ReplaceAll(dumps(t, request), "{worktree}", worktree)

	bundle := build(t, transcript.Transcript{UserMessages: users, ToolCalls: calls, TurnCalls: calls}, nil,
		Options{Query: query, Cwd: &root})

	retained := map[string]*json.Object{}
	for _, item := range objects(bundle, "tool_calls_newest_first") {
		retained[item.Value("id").(string)] = item
	}
	if retained["exec-f2c0383f-45bb-4eff-85ef-85843c71000e"] == nil {
		t.Fatal("the helper read was dropped")
	}
	journal := retained["exec-981692d5-493d-4910-8ff9-41656dad227f"]
	if journal == nil {
		t.Fatal("the journal read was dropped")
	}
	if !strings.Contains(journal.Value("output").(string), "observability/journal") ||
		journal.Value("execution").(*json.Object).Value("cwd") != "file://"+worktree ||
		journal.Value("trust") != "unverified_tool_observation" {
		t.Fatalf("journal execution is %v", dumps(t, journal.Value("execution")))
	}
	kept := strs(bundle, "user_messages")
	if kept[0] != users[0] || kept[len(kept)-1] != users[len(users)-1] {
		t.Fatal("the first or latest user message was dropped")
	}
	checkBudget(t, bundle, true)
}

// py: tests/test_sources.py::test_helper_at_the_end_of_a_decision_question_is_read
func TestHelperAtTheEndOfADecisionQuestionIsRead(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "lease.py"), "class Lease: pass\n")
	request := json.NewObject("decision", "Which control structure should parser.py use with the Lease API in lease.py?",
		"options", []any{json.NewObject("id", "finally", "description", "Use try/finally"),
			json.NewObject("id", "with", "description", "Use a context manager")})
	query := dumps(t, json.NewObject("command", "ts-decide <<'EOF'\n"+dumps(t, request)+"\nEOF"))
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Choose a lease control structure."}}, nil,
		Options{Query: query, Cwd: &dir})
	if !slices.Contains(sourcePaths(bundle), "lease.py") {
		t.Fatalf("sources are %q", sourcePaths(bundle))
	}
}

// py: tests/test_sources.py::test_e2e_helper_in_the_middle_keeps_head_tail_hash_and_redaction
func TestE2eHelperInTheMiddleKeepsHeadTailHashAndRedaction(t *testing.T) {
	dir := stateDir(t)
	symbol := "NumericalPolicyResolutionInputJson"
	secret := "sk-" + strings.Repeat("q", 40)
	text := "HEAD\n" + strings.Repeat("x", 8_995) + secret + strings.Repeat("x", 13_029-9_000-len(secret))
	text += "\nvoid " + symbol + "() { serialize_inputs(); }\n" + strings.Repeat("y", 32_000) + "\nTAIL"
	writeFile(t, filepath.Join(dir, "protocol_adapter.cc"), text)

	bundle := build(t, transcript.Transcript{UserMessages: []string{"Inspect the serialization helper."}},
		[]string{secret}, Options{Query: "Inspect " + symbol + " in protocol_adapter.cc.", Cwd: &dir})

	item := firstSource(t, bundle)
	got := item.Value("text").(string)
	if !strings.Contains(got, symbol) || !strings.HasPrefix(got, "HEAD\n") || !strings.HasSuffix(got, "\nTAIL") ||
		strings.Contains(got, secret) || strings.Contains(got, strings.Repeat("q", 20)) {
		t.Fatal("the clipped source lost the symbol, an end, or its redaction")
	}
	if item.Value("sha256") != sha256Text(text) || item.Value("truncated") != true || pystr.Len(got) > policy.ClipChars+100 {
		t.Fatalf("source has %d characters, truncated %v", pystr.Len(got), item.Value("truncated"))
	}
}

func TestSourceTruncationMatchesRedactedView(t *testing.T) {
	for _, name := range []string{"jev", "subq"} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			provider, err := policy.Provider(name)
			mustDo(t, err)
			limit := provider.Evidence.TextChars
			expanding := "\nTOKEN=12345678\n"
			shrinking := "\nTOKEN=" + strings.Repeat("s", 128) + "\n"
			for _, test := range []struct {
				name, text string
				truncated  bool
			}{
				{"plain", strings.Repeat("z", limit), false},
				{"expanding", strings.Repeat("z", limit-len(expanding)) + expanding, true},
				{"shrinking", strings.Repeat("z", limit+1-len(shrinking)) + shrinking, false},
				{"oversized", strings.Repeat("z", limit+1), true},
			} {
				t.Run(test.name, func(t *testing.T) {
					writeFile(t, filepath.Join(dir, "source.txt"), test.text)
					bundle := build(t, transcript.Transcript{}, nil, Options{Query: "source.txt", Cwd: &dir,
						Contracts: []string{"source.txt"}, Profile: provider.Evidence})
					item := firstSource(t, bundle)
					if item.Value("sha256") != sha256Text(test.text) || item.Value("trust") != "verified_file_observation" {
						t.Fatal("source observation identity changed")
					}
					if item.Value("truncated") != test.truncated {
						t.Errorf("truncated = %v, want %v", item.Value("truncated"), test.truncated)
					}
					count := json.Int("0")
					if test.truncated {
						count = json.Int("1")
					}
					if got := bundle.Value("metadata").(*json.Object).Value("source_files_truncated"); got != count {
						t.Errorf("source_files_truncated = %v, want %v", got, count)
					}
					if strings.Contains(item.Value("text").(string), "characters cut") != test.truncated {
						t.Error("retained source clipping marker does not match the expected truncation")
					}
				})
			}
		})
	}
}

// py: tests/test_sources.py::test_project_rules_precede_optional_old_paths_after_requested_helper
func TestProjectRulesPrecedeOptionalOldPathsAfterRequestedHelper(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Preserve all compatibility constraints.\n")
	writeFile(t, filepath.Join(dir, "helper.cc"), "void SerializationHelper();\n")
	var calls []transcript.ToolCall
	for index := range 8 {
		name := fmt.Sprintf("old_%d.cc", index)
		writeFile(t, filepath.Join(dir, name), "void OlderHelper();\n")
		calls = append(calls, call(fmt.Sprint(index), "Read", dumps(t, json.NewObject("file_path", name)), ""))
	}
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Inspect the requested helper."}, ToolCalls: calls},
		nil, Options{Query: "Inspect helper.cc.", Cwd: &dir})
	paths := sourcePaths(bundle)
	if len(paths) != maxSourceFiles || !slices.Equal(paths[:2], []string{"helper.cc", "AGENTS.md"}) {
		t.Fatalf("sources are %q", paths)
	}
}

// py: tests/test_sources.py::test_e2e_helper_and_recent_check_survive_large_user_history
func TestE2eHelperAndRecentCheckSurviveLargeUserHistory(t *testing.T) {
	dir := stateDir(t)
	// The native E2E follow-up supplied this worktree path, but retained no observations.
	relative := ".worktrees/workflow-v1-review-corpus-20261010/app/editing/protocol_adapter.cc"
	writeFile(t, filepath.Join(dir, relative), "void NumericalPolicyResolutionInputJson();\n"+strings.Repeat("// helper detail\n", 3_000))
	original := "Preserve portable coverage and the existing adapter interface."
	latest := "Read the implementation helper before choosing the coverage repair."
	users := []string{original}
	for range 100 {
		users = append(users, "Constraint: preserve the adapter behavior. "+strings.Repeat("x", 8_000))
	}
	users = append(users, latest)
	check := call("helper-read", "Bash", "read "+relative, "serialization digest check\n"+strings.Repeat("y", 8_000))
	calls := []transcript.ToolCall{check}
	tx := transcript.Transcript{UserMessages: users, ToolCalls: calls, TurnCalls: calls}

	bundle := build(t, tx, nil, Options{Query: "NumericalPolicyResolutionInputJson helper: " + relative + ".", Cwd: &dir})
	bundle.Value("metadata").(*json.Object).Set("user_history_source", "signed_host_context")
	mustDo(t, TrimToBudget(bundle, nil, "", 0))

	kept := strs(bundle, "user_messages")
	if kept[0] != original || kept[len(kept)-1] != latest {
		t.Fatal("the original or latest user message was dropped")
	}
	if !slices.Contains(sourcePaths(bundle), relative) ||
		!slices.Contains(field(objects(bundle, "tool_calls_newest_first"), "id"), "helper-read") {
		t.Fatal("the helper source or its check was dropped")
	}
	if metadataInt(t, bundle, "user_messages_omitted") <= 0 {
		t.Fatal("no user message was omitted")
	}
	checkBudget(t, bundle, false)
}

// py: tests/test_sources.py::test_sdk_request_path_followed_by_sentence_period_is_read
func TestSdkRequestPathFollowedBySentencePeriodIsRead(t *testing.T) {
	dir := stateDir(t)
	data, err := os.ReadFile("../../tests/fixtures/sdk-save-export-requests.json")
	mustDo(t, err)
	loaded, err := json.Loads(string(data))
	mustDo(t, err)
	requests := loaded.([]any)
	request := requests[len(requests)-1].(*json.Object).Value("request").(*json.Object)
	writeFile(t, filepath.Join(dir, "libisf", "src", "request_open.h"), "void RequestOpen(RequestSink& sink);\n")
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Continue SDK extraction."}}, nil,
		Options{Query: request.Value("decision").(string), Cwd: &dir})
	if !slices.Contains(sourcePaths(bundle), "libisf/src/request_open.h") {
		t.Fatalf("sources are %q", sourcePaths(bundle))
	}
}

// py: tests/test_sources.py::test_source_discovery_does_not_parse_large_history_after_requested_paths_fill_probe_budget
func TestSourceDiscoveryDoesNotParseLargeHistoryAfterRequestedPathsFillProbeBudget(t *testing.T) {
	dir := stateDir(t)
	var names []string
	for i := range 16 {
		name := fmt.Sprintf("source_%d.cc", i)
		names = append(names, name)
		writeFile(t, filepath.Join(dir, name), "void Request() {}\n")
	}
	old := call("old-sdk-log", "Bash", "cat old.cc", strings.Repeat("unrelated ", 200_000))
	calls := []transcript.ToolCall{old}
	tx := transcript.Transcript{UserMessages: []string{"Inspect the requested sources."}, ToolCalls: calls, TurnCalls: calls}
	started := time.Now()
	items, _, _, _, err := sourceFiles(tx, strings.Join(names, " "), &dir, nil, nil, hookFrame+1)
	elapsed := time.Since(started)
	mustDo(t, err)
	if got := field(items, "path"); !slices.Equal(got, names[:8]) {
		t.Fatalf("sources are %q", got)
	}
	if elapsed >= time.Second {
		t.Fatalf("source discovery took %v", elapsed)
	}
}

// py: tests/test_sources.py::test_sdk_helper_read_survives_older_current_turn_path_noise
func TestSdkHelperReadSurvivesOlderCurrentTurnPathNoise(t *testing.T) {
	dir := stateDir(t)
	text := "void RequestOpen(RequestSink& sink) { sink.EnsureStarted(); }\n"
	writeFile(t, filepath.Join(dir, "libisf", "src", "request_open.cc"), text)
	var calls []transcript.ToolCall
	for i := range 30 {
		calls = append(calls, call(fmt.Sprintf("old%d", i), "Bash", fmt.Sprintf("cat missing_%d.cc", i), "unrelated source"))
	}
	calls = append(calls, call("sdk-helper", "Bash", "cat libisf/src/request_open.cc", text))
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Continue SDK extraction."}, ToolCalls: calls, TurnCalls: calls},
		nil, Options{Query: "Which API should implement S5 save/export extraction?", Cwd: &dir})
	if !slices.Contains(sourcePaths(bundle), "libisf/src/request_open.cc") {
		t.Fatalf("sources are %q", sourcePaths(bundle))
	}
}

// py: tests/test_sources.py::test_explicit_sdk_helper_keeps_space_ahead_of_large_project_rule_files
func TestExplicitSdkHelperKeepsSpaceAheadOfLargeProjectRuleFiles(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Project rules.\n"+strings.Repeat("a", 16_000))
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "Additional project rules.\n"+strings.Repeat("b", 12_000))
	writeFile(t, filepath.Join(dir, "request_driver.h"), "class RequestSink {};\n"+strings.Repeat("c", 5_000))
	calls := []transcript.ToolCall{call("read", "Bash", "read baseline", strings.Repeat("d", 16_000)),
		call("check", "Bash", "run request tests", strings.Repeat("e", 16_000))}
	tx := transcript.Transcript{UserMessages: []string{"Preserve SDK compatibility. " + strings.Repeat("f", 16_000)},
		ToolCalls: calls, TurnCalls: calls}
	bundle := build(t, tx, nil, Options{Query: "Reuse request_driver.h for the request interface?", Cwd: &dir})
	if !slices.Contains(sourcePaths(bundle), "request_driver.h") {
		t.Fatalf("sources are %q", sourcePaths(bundle))
	}
	checkBudget(t, bundle, false)
}

// py: tests/test_sources.py::test_json_and_shell_quoted_paths_with_spaces_are_read
func TestJSONAndShellQuotedPathsWithSpacesAreRead(t *testing.T) {
	dir := stateDir(t)
	source := filepath.Join(dir, "project with spaces", "config.py")
	writeFile(t, source, "VALUE = 1\n")
	for _, query := range []string{dumps(t, json.NewObject("file_path", source)),
		dumps(t, json.NewObject("command", "cat '"+source+"'"))} {
		bundle := build(t, transcript.Transcript{UserMessages: []string{"Inspect configuration."}}, nil,
			Options{Query: query, Cwd: &dir})
		if got := sourcePaths(bundle); !slices.Equal(got, []string{"project with spaces/config.py"}) {
			t.Errorf("query %s: sources are %q", query, got)
		}
	}
}

// py: tests/test_sources.py::test_reads_query_paths_as_verified_source_observations
func TestReadsQueryPathsAsVerifiedSourceObservations(t *testing.T) {
	dir := stateDir(t)
	text := "def parse():\n    return 'ok'\n"
	writeFile(t, filepath.Join(dir, "agent_decisions", "parser.py"), text)

	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read agent_decisions/parser.py"}}, nil,
		Options{Query: "Check agent_decisions/parser.py", Cwd: &dir})

	if got := sourcePaths(bundle); !slices.Equal(got, []string{"agent_decisions/parser.py"}) {
		t.Fatalf("sources are %q", got)
	}
	item := firstSource(t, bundle)
	if item.Value("source") != "hook_file_read" || item.Value("trust") != "verified_file_observation" ||
		item.Value("sha256") != sha256Text(text) || item.Value("text") != text || item.Value("truncated") != false {
		t.Fatalf("source item is %s", dumps(t, item))
	}
	checkBudget(t, bundle, false)
}

// py: tests/test_sources.py::test_includes_project_agent_rules_when_present
func TestIncludesProjectAgentRulesWhenPresent(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Always inspect code before acting.\n")
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "Do not trust shell echoes.\n")
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Decide safely."}}, nil, Options{Cwd: &dir})
	if got := sourcePaths(bundle); !slices.Equal(got, []string{"AGENTS.md", "CLAUDE.md"}) {
		t.Fatalf("sources are %q", got)
	}
}

// py: tests/test_sources.py::test_excludes_private_and_out_of_root_paths
func TestExcludesPrivateAndOutOfRootPaths(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=secret-secret-secret\n")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "outside\n")
	mustDo(t, os.Symlink(outside, filepath.Join(dir, "leak.txt")))
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Check .env and leak.txt"}}, nil,
		Options{Query: ".env leak.txt", Cwd: &dir})
	if got := sourcePaths(bundle); len(got) != 0 {
		t.Fatalf("sources are %q", got)
	}
	if omitted := metadataInt(t, bundle, "source_files_omitted"); omitted != 2 {
		t.Fatalf("omitted %d", omitted)
	}
}

// py: tests/test_sources.py::test_source_text_is_bounded
func TestSourceTextIsBounded(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "large.py"), strings.Repeat("x", policy.ClipChars+500))
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read large.py"}}, nil, Options{Query: "large.py", Cwd: &dir})
	item := firstSource(t, bundle)
	if item.Value("path") != "large.py" || item.Value("truncated") != true || pystr.Len(item.Value("text").(string)) > policy.ClipChars+100 {
		t.Fatalf("source %v truncated %v", item.Value("path"), item.Value("truncated"))
	}
}

// py: tests/test_sources.py::test_too_large_source_is_omitted_without_hashing_contents
func TestTooLargeSourceIsOmittedWithoutHashingContents(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "huge.py"), "")
	mustDo(t, os.Truncate(filepath.Join(dir, "huge.py"), 2_000_000))
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read huge.py"}}, nil, Options{Query: "huge.py", Cwd: &dir})
	if got := sourcePaths(bundle); len(got) != 0 {
		t.Fatalf("sources are %q", got)
	}
	if n := metadataInt(t, bundle, "source_files_too_large"); n != 1 {
		t.Fatalf("too large %d", n)
	}
}

// py: tests/test_sources.py::test_output_path_spam_cannot_force_unbounded_source_probes
func TestOutputPathSpamCannotForceUnboundedSourceProbes(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "wanted.py"), "wanted = True\n")
	var spam []string
	for i := range 200 {
		spam = append(spam, fmt.Sprintf("missing_%d.py", i))
	}
	calls := []transcript.ToolCall{call("spam", "Bash", "cat wanted.py", strings.Join(spam, " "))}
	var probes []string
	original := resolveSource
	patch(t, &resolveSource, func(root, raw string) (string, bool, error) {
		probes = append(probes, raw)
		return original(root, raw)
	})
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Inspect wanted.py"}, ToolCalls: calls, TurnCalls: calls},
		nil, Options{Query: "wanted.py", Cwd: &dir})
	if got := sourcePaths(bundle); !slices.Equal(got, []string{"wanted.py"}) {
		t.Fatalf("sources are %q", got)
	}
	if len(probes) > 16 {
		t.Fatalf("%d probes", len(probes))
	}
}

// py: tests/test_sources.py::test_policy_state_dir_is_excluded_without_banning_project_state_sources
func TestPolicyStateDirIsExcludedWithoutBanningProjectStateSources(t *testing.T) {
	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "state", "foo.py"), "ok = True\n")
	writeFile(t, filepath.Join(dir, "runtime-private", "secret.py"), "hidden = True\n")
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "runtime-private"))
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read state/foo.py and runtime-private/secret.py"}}, nil,
		Options{Query: "state/foo.py runtime-private/secret.py", Cwd: &dir})
	if got := sourcePaths(bundle); !slices.Equal(got, []string{"state/foo.py"}) {
		t.Fatalf("sources are %q", got)
	}
}

// py: tests/test_sources.py::test_project_local_campaign_journal_is_allowed
func TestProjectLocalCampaignJournalIsAllowed(t *testing.T) {
	dir := stateDir(t)
	text := "{\"event\": \"append-only campaign\"}\n"
	writeFile(t, filepath.Join(dir, ".local", "v45-campaign", "journal.jsonl"), text)
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read .local/v45-campaign/journal.jsonl"}}, nil,
		Options{Query: ".local/v45-campaign/journal.jsonl", Cwd: &dir})
	if got := sourcePaths(bundle); !slices.Equal(got, []string{".local/v45-campaign/journal.jsonl"}) {
		t.Fatalf("sources are %q", got)
	}
	item := firstSource(t, bundle)
	if item.Value("sha256") != sha256Text(text) || item.Value("text") != text {
		t.Fatalf("source item is %s", dumps(t, item))
	}
}

// py: tests/test_sources.py::test_bounded_read_preserves_actual_tail_before_clipping
func TestBoundedReadPreservesActualTailBeforeClipping(t *testing.T) {
	dir := stateDir(t)
	text := "HEAD" + strings.Repeat("m", 80_000) + "ACTUAL_TAIL"
	writeFile(t, filepath.Join(dir, "tail.py"), text)
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read tail.py"}}, nil, Options{Query: "tail.py", Cwd: &dir})
	item := firstSource(t, bundle)
	if item.Value("truncated") != true || !strings.HasSuffix(item.Value("text").(string), "ACTUAL_TAIL") ||
		item.Value("sha256") != sha256Text(text) {
		t.Fatal("the bounded read lost the tail or the hash")
	}
}

// growingFile reads remaining bytes of "x" and has no stable descriptor.
type growingFile struct{ remaining int }

func (f *growingFile) Read(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), f.remaining)
	for i := range n {
		p[i] = 'x'
	}
	f.remaining -= n
	return n, nil
}

func (f *growingFile) Close() error { return nil }

func (f *growingFile) Stat() (fs.FileInfo, error) { return nil, errors.New("no stable descriptor") }

// py: tests/test_sources.py::test_file_growth_after_stat_is_treated_as_too_large
func TestFileGrowthAfterStatIsTreatedAsTooLarge(t *testing.T) {
	dir := stateDir(t)
	path := filepath.Join(dir, "growing.py")
	writeFile(t, path, "small\n")
	original := openSourceFile
	patch(t, &openSourceFile, func(candidate, root string) (sourceFile, error) {
		if candidate == path {
			return &growingFile{maxSourceBytes + 1}, nil
		}
		return original(candidate, root)
	})
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read growing.py"}}, nil, Options{Query: "growing.py", Cwd: &dir})
	if got := sourcePaths(bundle); len(got) != 0 {
		t.Fatalf("sources are %q", got)
	}
	if n := metadataInt(t, bundle, "source_files_too_large"); n != 1 {
		t.Fatalf("too large %d", n)
	}
}

// hookedFile runs after once, when its first Read returns.
type hookedFile struct {
	sourceFile
	after func()
}

func (f *hookedFile) Read(p []byte) (int, error) {
	n, err := f.sourceFile.Read(p)
	if f.after != nil {
		f.after()
		f.after = nil
	}
	return n, err
}

// py: tests/test_sources.py::test_source_changed_during_read_is_not_used
func TestSourceChangedDuringReadIsNotUsed(t *testing.T) {
	dir := stateDir(t)
	path := filepath.Join(dir, "changing.py")
	writeFile(t, path, "first = True\n")
	original := openSourceFile
	patch(t, &openSourceFile, func(candidate, root string) (sourceFile, error) {
		f, err := original(candidate, root)
		if err != nil || candidate != path {
			return f, err
		}
		return &hookedFile{f, func() { writeFile(t, path, "second = True\n") }}, nil
	})
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read changing.py"}}, nil, Options{Query: "changing.py", Cwd: &dir})
	if got := sourcePaths(bundle); len(got) != 0 {
		t.Fatalf("sources are %q", got)
	}
	if n := metadataInt(t, bundle, "source_files_omitted"); n < 1 {
		t.Fatalf("omitted %d", n)
	}
}

// py: tests/test_sources.py::test_final_symlink_swap_is_not_read
func TestFinalSymlinkSwapIsNotRead(t *testing.T) {
	dir := stateDir(t)
	target := filepath.Join(dir, "victim.py")
	writeFile(t, target, "safe = True\n")
	outside := filepath.Join(t.TempDir(), "outside-race.py")
	writeFile(t, outside, "stolen = True\n")
	swapped := false
	original := resolveSource
	patch(t, &resolveSource, func(root, raw string) (string, bool, error) {
		resolved, ok, err := original(root, raw)
		if raw == "victim.py" && ok && !swapped {
			mustDo(t, os.Remove(target))
			mustDo(t, os.Symlink(outside, target))
			swapped = true
		}
		return resolved, ok, err
	})
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read victim.py"}}, nil, Options{Query: "victim.py", Cwd: &dir})
	if got := sourcePaths(bundle); len(got) != 0 {
		t.Fatalf("sources are %q", got)
	}
	if n := metadataInt(t, bundle, "source_files_omitted"); n < 1 {
		t.Fatalf("omitted %d", n)
	}
}

// py: tests/test_sources.py::test_ancestor_symlink_swap_is_not_read_even_if_path_is_restored
func TestAncestorSymlinkSwapIsNotReadEvenIfPathIsRestored(t *testing.T) {
	dir := stateDir(t)
	realDir := filepath.Join(dir, "pkg")
	target := filepath.Join(realDir, "victim.py")
	writeFile(t, target, "safe = True\n")
	outsideDir := filepath.Join(t.TempDir(), "outside-pkg")
	writeFile(t, filepath.Join(outsideDir, "victim.py"), "stolen = True\n")
	swapped := false
	originalResolve, originalOpen := resolveSource, openSourceFile
	patch(t, &resolveSource, func(root, raw string) (string, bool, error) {
		resolved, ok, err := originalResolve(root, raw)
		if raw == "pkg/victim.py" && ok && !swapped {
			mustDo(t, os.Remove(target))
			mustDo(t, os.Remove(realDir))
			mustDo(t, os.Symlink(outsideDir, realDir))
			swapped = true
		}
		return resolved, ok, err
	})
	patch(t, &openSourceFile, func(candidate, root string) (sourceFile, error) {
		f, err := originalOpen(candidate, root)
		if err != nil || candidate != target {
			return f, err
		}
		return &hookedFile{f, func() {
			mustDo(t, os.Remove(realDir))
			writeFile(t, target, "safe = True\n")
		}}, nil
	})
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read pkg/victim.py"}}, nil,
		Options{Query: "pkg/victim.py", Cwd: &dir})
	if got := sourcePaths(bundle); len(got) != 0 {
		t.Fatalf("sources are %q", got)
	}
	if n := metadataInt(t, bundle, "source_files_omitted"); n < 1 {
		t.Fatalf("omitted %d", n)
	}
}
