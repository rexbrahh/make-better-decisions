package transcript

import (
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// The record builders below are the ones in tests/helpers.py.

func human(text string) *json.Object {
	return json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", text),
		"origin", json.NewObject("kind", "human"))
}

func peer(text string) *json.Object {
	return json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", text),
		"origin", json.NewObject("kind", "peer"))
}

func sidechainUser(text string) *json.Object {
	return json.NewObject("type", "user", "isSidechain", true, "agentId", "a1",
		"message", json.NewObject("role", "user", "content", text))
}

func toolUse(callID, name string, input any) *json.Object {
	return json.NewObject("type", "assistant", "message", json.NewObject("role", "assistant", "content", []any{
		json.NewObject("type", "tool_use", "id", callID, "name", name, "input", input)}))
}

func toolResult(callID string, content any) *json.Object {
	return json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", []any{
		json.NewObject("type", "tool_result", "tool_use_id", callID, "content", content)}))
}

func assistantText(text string) *json.Object {
	return json.NewObject("type", "assistant", "message", json.NewObject("role", "assistant", "content", []any{
		json.NewObject("type", "text", "text", text)}))
}

func codexUser(text string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "message", "role", "user", "content", []any{json.NewObject("type", "input_text", "text", text)}))
}

func codexAssistant(text string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "message", "role", "assistant", "content", []any{json.NewObject("type", "output_text", "text", text)}))
}

func codexCall(callID, name, arguments string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "function_call", "id", "fc_"+callID, "name", name, "arguments", arguments, "call_id", callID))
}

func codexCustom(callID, name, input string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "custom_tool_call", "id", "ctc_"+callID, "status", "completed", "call_id", callID, "name", name, "input", input))
}

func codexOutput(callID, output, kind string) *json.Object {
	return json.NewObject("type", "response_item", "payload", json.NewObject("type", kind, "call_id", callID, "output", output))
}

func commandEvent(callID string, command []any, output string) *json.Object {
	return json.NewObject("type", "event_msg", "payload", json.NewObject("type", "item_completed", "item", json.NewObject(
		"type", "CommandExecution", "id", callID, "command", command, "aggregated_output", output, "status", "completed")))
}

func writeJSONL(t *testing.T, dir, name string, records []*json.Object, tail string) string {
	t.Helper()
	var data strings.Builder
	for _, record := range records {
		data.WriteString(dumps(record, json.Options{}) + "\n")
	}
	data.WriteString(tail)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, path, host string) Transcript {
	t.Helper()
	parsed, err := Load(path, host)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return parsed
}

func ids(calls []ToolCall) []string {
	var found []string
	for _, call := range calls {
		found = append(found, call.ID)
	}
	return found
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestAsyncReplyRejectsEchoSplitInsideUTF8Character(t *testing.T) {
	title := strings.Repeat("a", 511) + "é" + strings.Repeat("b", 11)
	call := ToolCall{ID: "q1", Name: "request_user_input_async", Output: "Question shown.",
		Input: dumps(json.NewObject("questions", []any{json.NewObject("title", title)}), json.Options{})}
	reply := json.NewObject("questionItemId", `["request_user_input_async","q1",0]`, "question", title[:512])
	matched, err := replyAsked(reply, map[string]ToolCall{"q1": call})
	if err != nil || matched {
		t.Fatalf("split UTF-8 echo: matched %v, error %v; want false, nil", matched, err)
	}
}

func TestCodexAsyncReplyHostEcho(t *testing.T) {
	title := strings.Repeat("a", 508) + "éé" + strings.Repeat("b", 12)
	echo := title[:512]
	asciiTitle := strings.Repeat("a", 524)
	multibyteTitle := strings.Repeat("é", 256) + strings.Repeat("b", 12)
	invalidEcho := strings.Repeat("a", 509) + "\xed\xa0\x80"
	for _, tc := range []struct {
		name       string
		title      any
		echo       string
		tool       string
		identity   string
		origin     string
		unfinished bool
		want       bool
	}{
		{name: "observed byte lengths", title: title, echo: echo, want: true},
		{name: "ASCII prefix", title: asciiTitle, echo: asciiTitle[:512], want: true},
		{name: "title below 512 code points", title: multibyteTitle, echo: multibyteTitle[:512], want: true},
		{name: "full long title", title: title, echo: title, want: true},
		{name: "full short title", title: "Which format applies?", echo: "Which format applies?", want: true},
		{name: "synchronous exact title", title: title, echo: title, tool: "request_user_input", want: true},
		{name: "Claude tool exact title", title: title, echo: title, tool: "AskUserQuestion", want: true},
		{name: "short prefix", title: asciiTitle, echo: asciiTitle[:511]},
		{name: "long prefix", title: asciiTitle, echo: asciiTitle[:513]},
		{name: "empty echo", title: title},
		{name: "nonmatching echo", title: title, echo: strings.Repeat("z", 512)},
		{name: "no title truncation", title: echo, echo: strings.Repeat("z", 512)},
		{name: "invalid UTF-8 prefix", title: invalidEcho + "suffix", echo: invalidEcho},
		{name: "non-string title", title: json.Int("524"), echo: echo},
		{name: "synchronous tool", title: title, echo: echo, tool: "request_user_input"},
		{name: "Claude question tool", title: title, echo: echo, tool: "AskUserQuestion"},
		{name: "no completion", title: title, echo: echo, unfinished: true},
		{name: "wrong call", title: title, echo: echo, identity: `["request_user_input_async","other",0]`},
		{name: "wrong tool", title: title, echo: echo, identity: `["request_user_input","q1",0]`},
		{name: "wrong index", title: title, echo: echo, identity: `["request_user_input_async","q1",1]`},
		{name: "negative index", title: title, echo: echo, identity: `["request_user_input_async","q1",-1]`},
		{name: "boolean index", title: title, echo: echo, identity: `["request_user_input_async","q1",true]`},
		{name: "malformed identity", title: title, echo: echo, identity: `not JSON`},
		{name: "agent origin", title: title, echo: echo, origin: "agent"},
		{name: "peer origin", title: title, echo: echo, origin: "peer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := tc.tool
			if tool == "" {
				tool = "request_user_input_async"
			}
			identity := tc.identity
			if identity == "" {
				identity = dumps([]any{tool, "q1", json.Int("0")}, json.Options{})
			}
			reply := "<send_user_message_question_reply>" + dumps([]any{json.NewObject(
				"questionItemId", identity, "question", tc.echo, "answer", "Preserve the existing format.",
			)}, json.Options{}) + "</send_user_message_question_reply>"
			records := []*json.Object{codexCall("q1", tool, dumps(json.NewObject(
				"questions", []any{json.NewObject("title", tc.title)}), json.Options{}))}
			if !tc.unfinished {
				records = append(records, codexOutput("q1", "Question shown.", "function_call_output"))
			}
			message := codexUser(reply)
			if tc.origin != "" {
				message.Value("payload").(*json.Object).Set("origin", json.NewObject("kind", tc.origin))
			}
			records = append(records, message)
			observed := mustLoad(t, writeJSONL(t, t.TempDir(), "reply.jsonl", records, ""), "codex")
			if got := len(observed.UserMessages) == 1; got != tc.want {
				t.Fatalf("captured answer = %v, want %v", got, tc.want)
			}
			if tc.want {
				equal(t, observed.UserMessages, []string{reply})
			}
		})
	}
}

// py: tests/test_transcript.py::TranscriptMemoryTest::test_codex_passes_use_the_same_snapshot_when_the_file_grows
func TestCodexPassesUseTheSameSnapshotWhenTheFileGrows(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "snapshot.jsonl", []*json.Object{codexUser("Original instruction.")}, "")
	loadIdentity = func(records iter.Seq2[*json.Object, error]) (*HostIdentity, error) {
		identity, err := codexIdentity(records)
		stream, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer stream.Close()
		stream.WriteString(dumps(codexUser("Instruction appended between passes."), json.Options{}) + "\n")
		return identity, err
	}
	t.Cleanup(func() { loadIdentity = codexIdentity })
	equal(t, mustLoad(t, path, "codex").UserMessages, []string{"Original instruction."})
}

// py: tests/test_transcript.py::TranscriptMemoryTest::test_ignored_records_do_not_accumulate_in_memory
func TestIgnoredRecordsDoNotAccumulateInMemory(t *testing.T) {
	// Python bounds tracemalloc's peak. Go has no peak counter, so this bounds the heap that the runtime
	// takes back from the OS during Load, with a low GC target so the heap stays near its live size. Memory
	// that an earlier test left idle can hide growth, so a pass is weaker evidence than a failure.
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	padding := strings.Repeat("x", 1_000_000)
	for _, host := range []string{"claude", "codex"} {
		t.Run(host, func(t *testing.T) {
			user := map[string]func(string) *json.Object{"claude": human, "codex": codexUser}[host]
			records := []*json.Object{user("Preserve the complete user instruction.")}
			for range 16 {
				records = append(records, json.NewObject("type", "ignored", "padding", padding))
			}
			path := writeJSONL(t, t.TempDir(), host+".jsonl", records, "")
			records = nil
			var before, after runtime.MemStats
			debug.FreeOSMemory()
			runtime.ReadMemStats(&before)
			parsed := mustLoad(t, path, host)
			runtime.ReadMemStats(&after)
			equal(t, parsed.UserMessages, []string{"Preserve the complete user instruction."})
			grown := int64(after.HeapSys-after.HeapReleased) - int64(before.HeapSys-before.HeapReleased)
			t.Logf("heap grew by %d bytes", grown)
			if grown >= 16_000_000 { // streaming measures about 8 MB; keeping the 16 records adds 16 MB
				t.Errorf("heap grew by %d bytes while loading 16 ignored 1 MB records", grown)
			}
		})
	}
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_reads_user_words_tool_calls_and_turn
func TestReadsUserWordsToolCallsAndTurn(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "s.jsonl", []*json.Object{
		human("first ask"),
		toolUse("toolu_1", "Bash", json.NewObject("command", "ls")),
		toolResult("toolu_1", "a.txt"),
		peer("peer note"),
		json.NewObject("type", "user", "isCompactSummary", true, "origin", json.NewObject("kind", "human"),
			"message", json.NewObject("role", "user", "content", "summary of earlier work")),
		human("second ask"),
		toolUse("toolu_2", "Read", json.NewObject("file_path", "/x")),
		toolResult("toolu_2", []any{json.NewObject("type", "text", "text", "line 1")}),
		assistantText("done"),
	}, "")
	parsed := mustLoad(t, path, "claude")
	equal(t, parsed.UserMessages, []string{"first ask", "second ask"})
	equal(t, ids(parsed.ToolCalls), []string{"toolu_1", "toolu_2"})
	input, err := json.Loads(parsed.ToolCalls[0].Input)
	if same, eqErr := json.Equal(input, json.NewObject("command", "ls")); err != nil || eqErr != nil || !same {
		t.Errorf("input %q", parsed.ToolCalls[0].Input)
	}
	equal(t, parsed.ToolCalls[0].Output, "a.txt")
	equal(t, parsed.ToolCalls[1].Output, "line 1")
	equal(t, ids(parsed.TurnCalls), []string{"toolu_2"})
	equal(t, parsed.LastAssistantText, "done")
	if parsed.Brief != nil {
		t.Errorf("brief %q", *parsed.Brief)
	}
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_partial_last_line_is_skipped
func TestPartialLastLineIsSkipped(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "s.jsonl", []*json.Object{human("ask")}, `{"type": "user", "mess`)
	equal(t, mustLoad(t, path, "claude").UserMessages, []string{"ask"})
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_headless_prompt_without_origin_is_the_users
func TestHeadlessPromptWithoutOriginIsTheUsers(t *testing.T) {
	// `claude -p` writes the prompt with no `origin` (observed 2026-10-10).
	headless := json.NewObject("type", "user", "isSidechain", false, "message", json.NewObject("role", "user", "content", "probe ask"))
	meta := headless.Copy()
	meta.Set("isMeta", true)
	meta.Set("message", json.NewObject("role", "user", "content", "skill text"))
	echo := headless.Copy()
	echo.Set("message", json.NewObject("role", "user", "content", "<bash-stdout>x</bash-stdout>"))
	path := writeJSONL(t, t.TempDir(), "s.jsonl", []*json.Object{headless, meta, echo}, "")
	equal(t, mustLoad(t, path, "claude").UserMessages, []string{"probe ask"})
}

func sidechain(record *json.Object) *json.Object {
	record.Set("isSidechain", true)
	return record
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_subagent_brief_starts_the_turn
func TestSubagentBriefStartsTheTurn(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "agent-a1.jsonl", []*json.Object{
		sidechainUser("Find why tiles reload"),
		sidechain(toolUse("toolu_3", "Grep", json.NewObject("pattern", "reload"))),
		sidechain(toolResult("toolu_3", "cache.py:10")),
	}, "")
	parsed := mustLoad(t, path, "claude")
	if parsed.Brief == nil || *parsed.Brief != "Find why tiles reload" {
		t.Errorf("brief %v", parsed.Brief)
	}
	equal(t, len(parsed.UserMessages), 0)
	equal(t, ids(parsed.TurnCalls), []string{"toolu_3"})
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_turn_key_changes_only_with_a_new_user_message
func TestTurnKeyChangesOnlyWithANewUserMessage(t *testing.T) {
	a := Transcript{UserMessages: []string{"one"}}
	b := Transcript{UserMessages: []string{"one"}, ToolCalls: []ToolCall{{ID: "x", Name: "Bash", Input: "ls"}}}
	c := Transcript{UserMessages: []string{"one", "two"}}
	if a.TurnKey() != b.TurnKey() || a.TurnKey() == c.TurnKey() {
		t.Errorf("turn keys %s %s %s", a.TurnKey(), b.TurnKey(), c.TurnKey())
	}
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_lone_surrogate_escape_becomes_a_replacement_character
func TestLoneSurrogateEscapeBecomesAReplacementCharacter(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "surrogate.jsonl", []*json.Object{human("Keep \xed\xa0\x80 the cache.")}, "")
	parsed := mustLoad(t, path, "claude")
	equal(t, parsed.UserMessages, []string{"Keep \xef\xbf\xbd the cache."})
	equal(t, len(parsed.TurnKey()), 16)
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_large_transcript_loads_in_under_two_seconds
func TestLargeTranscriptLoadsInUnderTwoSeconds(t *testing.T) {
	records := []*json.Object{human("go")}
	output := strings.Repeat("x", 200)
	for i := range 25_000 {
		id := "toolu_" + string(json.IntOf(int64(i)))
		records = append(records, toolUse(id, "Bash", json.NewObject("command", "echo "+string(json.IntOf(int64(i))))),
			toolResult(id, output))
	}
	path := writeJSONL(t, t.TempDir(), "big.jsonl", records, "")
	start := time.Now()
	parsed := mustLoad(t, path, "claude")
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("load took %v", elapsed)
	}
	equal(t, len(parsed.ToolCalls), 25_000)
}

func isDecide(t *testing.T, call ToolCall) bool {
	t.Helper()
	decided, err := IsDecide(call)
	if err != nil {
		t.Fatalf("IsDecide(%q): %v", call.Input, err)
	}
	return decided
}

// py: tests/test_transcript.py::ClaudeTranscriptTest::test_is_decide_matches_original_and_rewritten_commands
func TestIsDecideMatchesOriginalAndRewrittenCommands(t *testing.T) {
	if !isDecide(t, ToolCall{ID: "a", Name: "Bash", Input: `{"command": "ts-decide '{}'"}`}) ||
		!isDecide(t, ToolCall{ID: "b", Name: "Bash", Input: `{"command": "cat /s/verdicts/b.txt"}`}) ||
		isDecide(t, ToolCall{ID: "c", Name: "Bash", Input: `{"command": "ls"}`}) {
		t.Error("IsDecide misclassified a command")
	}
}

// py: tests/test_transcript.py::DecideCommandTest::test_observed_codex_heredoc_does_not_scan_the_script_body
func TestObservedCodexHeredocDoesNotScanTheScriptBody(t *testing.T) {
	// The 520 MB E2E transcript records long scripts as CommandExecution events.
	command := "python3 - <<'PY'\n" + strings.Repeat("print('local diagnostic')\n", 2_000) + "PY\n"
	record := commandEvent("exec-heredoc", []any{"/bin/zsh", "-c", command}, "diagnostic complete")
	parsed := mustLoad(t, writeJSONL(t, t.TempDir(), "heredoc.jsonl", []*json.Object{record}, ""), "codex")
	var streams []*shlex.StringStream
	newLexer = func(command string) *shlex.Lexer {
		stream := shlex.NewStringStream(command)
		streams = append(streams, stream)
		return shlex.NewStream(stream, true, "")
	}
	t.Cleanup(func() { newLexer = func(command string) *shlex.Lexer { return shlex.New(command, true, "") } })
	if isDecide(t, parsed.ToolCalls[0]) {
		t.Error("a heredoc counted as a decision")
	}
	equal(t, len(streams), 1)
	if read := streams[0].Tell(); read > int64(pystr.Len("python3 ")) {
		t.Errorf("lexer read %d characters", read)
	}
}

// py: tests/test_transcript.py::DecideCommandTest::test_candidates_keep_full_validation_and_exact_transport_matching
func TestCandidatesKeepFullValidationAndExactTransportMatching(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{"'/plugin with spaces/bin/ts-decide' <<'EOF'\n{}\nEOF", true},
		{"/plugin/bin/ts-decide 'unterminated", false},
		{`printf '%s\n' 'ts-decide verdict: ADVISORY'`, true},
		{`printf '%s\n' 'ts-decide: decision unverified (timeout)'`, true},
		{`printf '%s\n' 'ts-decide verdict: ADVISORY' extra`, false},
		{"cat '/state with spaces/verdicts/receipt.txt'", true},
		{"cat /state/verdicts/receipt.txt extra", false},
		{"cat /state/verdicts/receipt.txt 'unterminated", false},
		{"python3 - <<'PY'\nprint('ts-decide verdict: ADVISORY')\nPY", false},
		{"grep source.py # ts-decide", false},
		{"", false},
	}
	for _, c := range cases {
		call := ToolCall{ID: "call", Name: "exec_command", Input: dumps(json.NewObject("cmd", c.command), json.Options{})}
		if got := isDecide(t, call); got != c.want {
			t.Errorf("IsDecide(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}

// py: tests/test_transcript.py::CodexTranscriptTest::test_late_metadata_preserves_worker_brief_and_rejects_conflicts
func TestLateMetadataPreservesWorkerBriefAndRejectsConflicts(t *testing.T) {
	dir := t.TempDir()
	payload := json.NewObject("id", "worker", "cwd", dir, "source", json.NewObject("subagent", json.NewObject(
		"thread_spawn", json.NewObject("parent_thread_id", "parent", "agent_path", "/root/worker", "depth", json.IntOf(1)))))
	metadata := json.NewObject("type", "session_meta", "payload", payload)
	brief := codexUser("Complete the assigned parser repair.")
	brief.Value("payload").(*json.Object).Set("origin", json.NewObject("kind", "agent"))
	path := writeJSONL(t, dir, "worker.jsonl", []*json.Object{brief, metadata, metadata}, "")
	parsed := mustLoad(t, path, "codex")
	if parsed.Brief == nil || *parsed.Brief != "Complete the assigned parser repair." {
		t.Errorf("brief %v", parsed.Brief)
	}
	equal(t, len(parsed.UserMessages), 0)
	other := payload.Copy()
	other.Set("id", "other")
	writeJSONL(t, dir, "worker.jsonl", []*json.Object{brief, metadata, json.NewObject("type", "session_meta", "payload", other)}, "")
	_, err := Load(path, "codex")
	if e, ok := err.(*pyerr.OSError); !ok || e.PyType() != "OSError" || !strings.Contains(e.Error(), "conflicting host identity") {
		t.Errorf("Load error %v", err)
	}
}

// py: tests/test_transcript.py::CodexTranscriptTest::test_hook_feedback_is_not_user_authority_or_a_new_turn
func TestHookFeedbackIsNotUserAuthorityOrANewTurn(t *testing.T) {
	feedback := codexUser(`<hook_prompt hook_run_id="stop:14:/cache/codex/hooks.json">missing hook.py</hook_prompt>`)
	feedback.Value("payload").(*json.Object).Set("internal_chat_message_metadata_passthrough",
		json.NewObject("turn_id", "turn-1", "content_item_kinds", []any{"unknown"}))
	records := []*json.Object{codexUser("Repair the plugin."), codexCall("read", "exec_command", `{"cmd":"pwd"}`),
		feedback, codexOutput("read", "/project", "function_call_output")}
	parsed := mustLoad(t, writeJSONL(t, t.TempDir(), "feedback.jsonl", records, ""), "codex")
	equal(t, parsed.UserMessages, []string{"Repair the plugin."})
	equal(t, ids(parsed.TurnCalls), []string{"read"})
}

// py: tests/test_transcript.py::CodexTranscriptTest::test_code_mode_completed_events_preserve_hook_ids_and_outputs
func TestCodeModeCompletedEventsPreserveHookIdsAndOutputs(t *testing.T) {
	records := []*json.Object{
		codexUser("Fix the parser."), codexCustom("outer", "exec", `text(await tools.exec_command({cmd:"read"}));`),
		codexOutput("outer", "agent printed an invented result", "custom_tool_call_output"),
		commandEvent("exec-observed", []any{"/bin/zsh", "-c", "cat parser.py"}, "input.read_all()"),
		json.NewObject("type", "event_msg", "payload", json.NewObject("type", "item_completed", "item", json.NewObject(
			"type", "McpToolCall", "id", "mcp-observed", "server", "docs", "tool", "read",
			"arguments", json.NewObject("url", "https://example.com"),
			"result", json.NewObject("content", []any{json.NewObject("type", "text", "text", "official docs")})))),
	}
	parsed := mustLoad(t, writeJSONL(t, t.TempDir(), "code.jsonl", records, ""), "codex")
	equal(t, ids(parsed.ToolCalls), []string{"exec-observed", "mcp-observed"})
	equal(t, parsed.ToolCalls[0].Output, "input.read_all()")
	if !strings.Contains(parsed.ToolCalls[1].Output, "official docs") {
		t.Errorf("MCP output %q", parsed.ToolCalls[1].Output)
	}
}

// py: tests/test_transcript.py::CodexTranscriptTest::test_duplicate_classic_and_event_calls_are_not_repeated
func TestDuplicateClassicAndEventCallsAreNotRepeated(t *testing.T) {
	event := commandEvent("call1", []any{"zsh", "-c", "cat parser.py"}, "source text")
	event.Value("payload").(*json.Object).Value("item").(*json.Object).Delete("status")
	records := []*json.Object{codexUser("Read the parser."), codexCall("call1", "exec_command", `{"cmd":"cat parser.py"}`), event}
	parsed := mustLoad(t, writeJSONL(t, t.TempDir(), "direct.jsonl", records, ""), "codex")
	equal(t, len(parsed.ToolCalls), 1)
	equal(t, parsed.ToolCalls[0].Output, "source text")
}

// py: tests/test_transcript.py::CodexTranscriptTest::test_skips_injected_context_and_matches_outputs
func TestSkipsInjectedContextAndMatchesOutputs(t *testing.T) {
	path := writeJSONL(t, t.TempDir(), "rollout.jsonl", []*json.Object{
		codexUser("# AGENTS.md instructions for /x\n..."),
		codexUser("<environment_context>cwd</environment_context>"),
		codexUser("fix the build"),
		codexCall("call_1", "exec_command", `{"cmd": "make"}`),
		codexOutput("call_1", "error: missing header", "function_call_output"),
		codexCustom("call_2", "apply_patch", "*** Begin Patch"),
		codexOutput("call_2", "Success", "custom_tool_call_output"),
		codexAssistant("patched"),
		json.NewObject("type", "event_msg", "payload", json.NewObject("type", "token_count")),
	}, "")
	parsed := mustLoad(t, path, "codex")
	equal(t, parsed.UserMessages, []string{"fix the build"})
	var summary [][3]string
	for _, call := range parsed.ToolCalls {
		summary = append(summary, [3]string{call.ID, call.Name, call.Output})
	}
	equal(t, summary, [][3]string{{"call_1", "exec_command", "error: missing header"}, {"call_2", "apply_patch", "Success"}})
	equal(t, parsed.ToolCalls[0].Input, `{"cmd": "make"}`)
	equal(t, len(parsed.TurnCalls), 2)
	equal(t, parsed.LastAssistantText, "patched")
}

// dumps returns json.Dumps(v, o) for a test value, which never nests deep enough to fail.
func dumps(v any, o json.Options) string {
	text, err := json.Dumps(v, o)
	if err != nil {
		panic(err)
	}
	return text
}
