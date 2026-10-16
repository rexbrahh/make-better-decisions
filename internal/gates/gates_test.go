package gates

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// The Python frames of the gates when hook.py calls them.
const (
	guardFrame = 5 // <module>, main, handle, _pre_tool_use, guard
	gateFrame  = 7 // ... _pre_tool_use, _gate, the lambda, edit_gate or plan_gate
)

// setUp ports tests/helpers.py StateTestCase.setUp. HOME is a private directory too, so that guard tests
// never resolve paths under the real ~/.local/state/agent-decisions. Like a real home, its parent does not
// hold the state directory.
func setUp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "Users", "me")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	t.Setenv("TYPESAFE_API_KEY", "test-key-0123456789abcdef")
	for _, name := range []string{"AGENT_DECISIONS_OFF", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return dir
}

// availability ports StateTestCase.availability: a signed policy that lets host fail open.
func availability(t *testing.T, host string) {
	t.Helper()
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", host, "review_mode", "availability"))); err != nil {
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

func loads(t *testing.T, text string) any {
	t.Helper()
	value, err := json.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func obj(t *testing.T, text string) *json.Object {
	t.Helper()
	return loads(t, text).(*json.Object)
}

func equal(t *testing.T, a, b any) bool {
	t.Helper()
	same, err := json.Equal(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return same
}

func assertEqual(t *testing.T, got, want any) {
	t.Helper()
	if !equal(t, got, want) {
		t.Fatalf("got %s, want %s", dumps(t, got), dumps(t, want))
	}
}

// merge returns {**base, **over}.
func merge(base *json.Object, over ...*json.Object) *json.Object {
	result := base.Copy()
	for _, object := range over {
		for _, key := range object.Keys() {
			result.Set(key, object.Value(key))
		}
	}
	return result
}

// entryOf returns {**record, **fields}: a ledger line made from an outcome's record.
func entryOf(record *json.Object, pairs ...any) *json.Object {
	return merge(record, json.NewObject(pairs...))
}

type ctxOptions struct {
	transcript *transcript.Transcript
	entries    []*json.Object
	agentID    any
	host       string
}

// makeCtx ports tests/helpers.py make_ctx.
func makeCtx(t *testing.T, o ctxOptions) Context {
	t.Helper()
	tr := transcript.Transcript{UserMessages: []string{"decide the cache policy"}}
	if o.transcript != nil {
		tr = *o.transcript
	}
	host := o.host
	if host == "" {
		host = "claude"
	}
	calls := map[string]transcript.ToolCall{}
	for _, call := range tr.ToolCalls {
		calls[call.ID] = call
	}
	var records []*json.Object
	for _, entry := range o.entries {
		id, _ := entry.Value("tool_use_id").(string)
		if call, ok := calls[id]; ok && entry.Value("gate") == "decide" {
			records = append(records, merge(json.NewObject("host", host, "verdict", call.Output), entry))
		} else {
			records = append(records, merge(json.NewObject("host", host), entry))
		}
	}
	bundle, err := evidence.Build(tr, nil, evidence.Options{Frame: 3})
	if err != nil {
		t.Fatal(err)
	}
	return Context{Host: host, SessionID: "s1", AgentID: o.agentID, ToolUseID: "toolu_now", Transcript: tr,
		Evidence: bundle, Entries: records}
}

func fake(answers map[string]any) *jevtest.Fake {
	return &jevtest.Fake{Answers: answers}
}

// asker ports tests/helpers.py asker.
func asker(fake *jevtest.Fake) Ask {
	return func(state, questions *json.Object) (*json.Object, error) {
		return jev.Ask(state, questions, "k", fake.Post, nil)
	}
}

// hookAsker ports hook._asker(post) without metrics: it reviews a scrubbed copy of state and then makes
// state that copy, as the review saw it.
func hookAsker(post jev.PostFunc) Ask {
	return func(state, questions *json.Object) (*json.Object, error) {
		key, err := jev.LoadKey()
		if err != nil {
			return nil, err
		}
		scrubbed, err := evidence.Scrub(state, []string{key}, 10)
		if err != nil {
			return nil, err
		}
		requestState := scrubbed.(*json.Object)
		defer func() {
			for _, name := range state.Keys() {
				state.Delete(name)
			}
			for _, name := range requestState.Keys() {
				state.Set(name, requestState.Value(name))
			}
		}()
		scrubbedQuestions, err := evidence.Scrub(questions, []string{key}, 10)
		if err != nil {
			return nil, err
		}
		return jev.Ask(requestState, scrubbedQuestions.(*json.Object), key, post, nil)
	}
}

func jevError(reason string) *jev.JevError {
	return &jev.JevError{Reason: reason}
}

func decide(t *testing.T, request any, ctx Context, ask Ask) Outcome {
	t.Helper()
	raw, ok := request.(string)
	if !ok {
		raw = dumps(t, request)
	}
	outcome, err := Decide(raw, ctx, ask)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func editGate(t *testing.T, name string, toolInput *json.Object, ctx Context, ask Ask) Outcome {
	t.Helper()
	outcome, err := EditGate(name, toolInput, ctx, ask, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func guard(t *testing.T, toolInput *json.Object, cwd any) *Outcome {
	t.Helper()
	outcome, err := Guard(toolInput, cwd, guardFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func questionGate(t *testing.T, toolInput *json.Object, ctx Context, ask Ask) Outcome {
	t.Helper()
	outcome, err := QuestionGate(toolInput, ctx, ask)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func planGate(t *testing.T, ctx Context, ask Ask) Outcome {
	t.Helper()
	outcome, err := PlanGate(ctx, ask, nil, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func stopGate(t *testing.T, final string, active bool, ctx Context, ask Ask) Outcome {
	t.Helper()
	outcome, err := StopGate(final, active, ctx, ask, gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := policy.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func cmd(command string) *json.Object {
	return json.NewObject("command", command)
}

func assertContains(t *testing.T, text, part string) {
	t.Helper()
	if !strings.Contains(text, part) {
		t.Fatalf("%q does not contain %q", text, part)
	}
}

func assertNotContains(t *testing.T, text, part string) {
	t.Helper()
	if strings.Contains(text, part) {
		t.Fatalf("%q contains %q", text, part)
	}
}

func assertAction(t *testing.T, outcome Outcome, action string) {
	t.Helper()
	if outcome.Action != action {
		t.Fatalf("got %s (%q), want %s", outcome.Action, outcome.Message, action)
	}
}

// --- tests/test_gates.py ClassifyTest

const gatesRequest = `{"decision": "Bob's café: which cache?", "options": [
    {"id": "a", "description": "it's ü"}, {"id": "b", "description": "line\nbreak"}]}`

func assertClassify(t *testing.T, command, kind, request string) {
	t.Helper()
	if gotKind, gotRequest := Classify(command); gotKind != kind || gotRequest != request {
		t.Errorf("Classify(%q) = (%q, %q), want (%q, %q)", command, gotKind, gotRequest, kind, request)
	}
}

// py: tests/test_gates.py::test_complete_non_shell_heredocs_keep_command_mentions_as_data
func TestCompleteNonShellHeredocsKeepCommandMentionsAsData(t *testing.T) {
	for _, command := range []string{
		"python3 - <<'PY'\nif ('ts-decide' in text):\n    print(text)\nPY\n",
		"python3 - <<'PY'\nprint('ts-decide')\nPY\n",
		"node - <<'JS'\nconsole.log('ts-decide');\nJS\n",
		"cat <<'LOG'\n(ts-decide) is the diagnostic subject\nLOG\n",
		"# diagnostic\npython3 - <<'PY'\nprint('ts-decide')\nPY\n# finished\n",
	} {
		assertClassify(t, command, "other", "")
	}
}

// py: tests/test_gates.py::test_newline_and_shell_heredoc_consultations_remain_misuse
func TestNewlineAndShellHeredocConsultationsRemainMisuse(t *testing.T) {
	for _, command := range []string{
		"echo status\nts-decide '{}'",
		"echo status # comment\nts-decide '{}'",
		"bash <<'SH'\nts-decide '{}'\nSH\n",
		"env -S 'bash -' <<'SH'\nts-decide '{}'\nSH\n",
		"python3 - <<'PY'\nprint('diagnostic')\nPY\nts-decide '{}'\n",
	} {
		assertClassify(t, command, "misuse", "")
	}
}

// py: tests/test_gates.py::test_shell_comments_and_quoted_or_escaped_newlines_are_data
func TestShellCommentsAndQuotedOrEscapedNewlinesAreData(t *testing.T) {
	for _, command := range []string{"# (ts-decide) is the subject\nls", "echo 'literal\nts-decide'",
		"echo foo \\\nts-decide '{}'"} {
		assertClassify(t, command, "other", "")
	}
}

// py: tests/test_gates.py::test_wrapped_consultations_are_misuse
func TestWrappedConsultationsAreMisuse(t *testing.T) {
	for _, command := range []string{"env ts-decide '{}'", "command ts-decide '{}'", "exec ts-decide '{}'",
		"time ts-decide '{}'", "xargs ts-decide <<'EOF'\n{}\nEOF\n",
		"env -S'bash -' <<'SH'\nts-decide '{}'\nSH\n",
		"env --split-string='bash -' <<'SH'\nts-decide '{}'\nSH\n",
		"env -i FLAG=1 command -p ts-decide '{}'", "FLAG=1 ts-decide '{}'",
		"env -P /usr/bin ts-decide '{}'"} {
		assertClassify(t, command, "misuse", "")
	}
}

// py: tests/test_gates.py::test_wrapped_lookups_mentions_and_data_heredocs_are_other
func TestWrappedLookupsMentionsAndDataHeredocsAreOther(t *testing.T) {
	for _, command := range []string{"command -v ts-decide", "command -V ts-decide", "command -pv ts-decide",
		"env echo ts-decide", "exec -a ts-decide echo status", "time -o ts-decide echo status",
		"xargs -I ts-decide echo ts-decide", "xargs -J ts-decide echo ts-decide",
		"env -- FLAG=1 echo ts-decide",
		"env python3 - <<'PY'\nprint('ts-decide')\nPY\n",
		"env -S'python3 -' <<'PY'\nprint('ts-decide')\nPY\n"} {
		assertClassify(t, command, "other", "")
	}
}

// py: tests/test_gates.py::test_python_heredoc_can_mention_the_consultation_command_as_data
func TestPythonHeredocCanMentionTheConsultationCommandAsData(t *testing.T) {
	body := "print('the user\\'s ts-decide history')\n"
	assertClassify(t, "python3 - <<'PY'\n"+body+"PY\n", "other", "")
}

// py: tests/test_gates.py::test_whole_command_forms_carry_the_request_intact
func TestWholeCommandFormsCarryTheRequestIntact(t *testing.T) {
	setUp(t)
	request := loads(t, gatesRequest)
	raw, err := json.Dumps(request, json.Options{AllowUnicode: true})
	if err != nil {
		t.Fatal(err)
	}
	quoted := "ts-decide " + shlex.Quote(raw)
	heredoc := "ts-decide <<'EOF'\n" + raw + "\nEOF\n"
	for _, command := range []string{quoted, heredoc, "  " + quoted + "  ",
		"/Users/x/agent-decisions/bin/ts-decide " + shlex.Quote(raw)} {
		kind, got := Classify(command)
		if kind != "decide" {
			t.Fatalf("%q: kind %q", command, kind)
		}
		assertEqual(t, loads(t, got), request)
	}
}

// py: tests/test_gates.py::test_crlf_heredoc_is_accepted
func TestCrlfHeredocIsAccepted(t *testing.T) {
	request := loads(t, gatesRequest)
	raw := dumps(t, request)
	kind, got := Classify("ts-decide <<'EOF'\r\n" + raw + "\r\nEOF\r\n")
	if kind != "decide" {
		t.Fatalf("kind %q", kind)
	}
	assertEqual(t, loads(t, got), request)
}

// py: tests/test_gates.py::test_ts_decide_inside_a_larger_command_is_misuse
func TestTsDecideInsideALargerCommandIsMisuse(t *testing.T) {
	for _, command := range []string{"ts-decide '{}' && rm -rf x", "ts-decide '{}'; ls", "ts-decide '{}' | tee out",
		"ts-decide '{}' > out", "cd /x && ts-decide '{}'", "(ts-decide '{}')",
		"ts-decide '{}'\nls", "ts-decide '{}' extra", "ts-decide \"$(cat req.json)\" | cat",
		"ts-decide '{}' 'unterminated"} {
		if kind, _ := Classify(command); kind != "misuse" {
			t.Errorf("Classify(%q) = %q", command, kind)
		}
	}
}

// py: tests/test_gates.py::test_mentions_that_do_not_invoke_ts_decide_are_other
func TestMentionsThatDoNotInvokeTsDecideAreOther(t *testing.T) {
	for _, command := range []string{"grep -rn ts-decide .", "echo ts-decide", "ls", "git log --grep ts-decide"} {
		assertClassify(t, command, "other", "")
	}
}

// py: tests/test_gates.py::test_command_of_reads_claude_and_codex_shapes
func TestCommandOfReadsClaudeAndCodexShapes(t *testing.T) {
	for _, item := range []struct{ input, want string }{
		{`{"command": "ls"}`, "ls"},
		{`{"cmd": "ls -l"}`, "ls -l"},
		{`{"command": ["bash", "-lc", "ts-decide '{}'"]}`, "ts-decide '{}'"},
		{`{"command": ["ls", "-l"]}`, "ls -l"},
		{`{"input": "patch"}`, ""},
	} {
		if got := CommandOf(obj(t, item.input)); got != item.want {
			t.Errorf("CommandOf(%s) = %q, want %q", item.input, got, item.want)
		}
	}
}

// --- tests/test_gates.py GuardTest

// py: tests/test_gates.py::test_peer_python_list_read_does_not_raise_an_invalid_glob_error
func TestPeerPythonListReadDoesNotRaiseAnInvalidGlobError(t *testing.T) {
	setUp(t)
	command := `python3 - <<'PY'
import hashlib,json
from pathlib import Path
root=Path('/Users/rexliu/fieldforge/.local/v45-campaign-20261004')
for p in [root/'journal.jsonl',root/'qualification-20261005/QUALIFICATION.md']:
    data=p.read_bytes()
    print(str(p),hashlib.sha256(data).hexdigest())
    if p.suffix=='.jsonl':
        rows=[json.loads(x) for x in data.decode().splitlines() if x.strip()]
        print(json.dumps(rows[-1],indent=2))
    else:print('\\n'.join(data.decode().splitlines()[:42]))
PY
`
	if outcome := guard(t, json.NewObject("cmd", command), "/Users/rexliu/fieldforge"); outcome != nil {
		t.Fatalf("got %+v", outcome)
	}
}

// The Python test patches fnmatch.fnmatchcase to raise re.error, which selects guard's literal-comparison
// fallback for Python 3.9. Python 3.15 and the Go port never raise there, so the port checks the outcome.
//
// py: tests/test_gates.py::test_invalid_glob_does_not_skip_a_later_private_path
func TestInvalidGlobDoesNotSkipALaterPrivatePath(t *testing.T) {
	setUp(t)
	outcome := guard(t, json.NewObject("paths", []any{"/tmp/[z-a]", stateDir(t) + "/sessions/s.jsonl"}), nil)
	if outcome == nil || outcome.Action != "deny" {
		t.Fatalf("got %+v", outcome)
	}
}

// py: tests/test_gates.py::test_denies_any_mention_of_the_state_directory
func TestDeniesAnyMentionOfTheStateDirectory(t *testing.T) {
	setUp(t)
	state := stateDir(t)
	for _, toolInput := range []*json.Object{
		cmd("echo x >> " + state + "/sessions/s.jsonl"),
		json.NewObject("file_path", state+"/verdicts/v.txt", "content", "x"),
		cmd("cat ~/.local/state/agent-decisions/sessions/s.jsonl"),
	} {
		outcome := guard(t, toolInput, nil)
		if outcome == nil || outcome.Action != "deny" {
			t.Fatalf("%s: got %+v", dumps(t, toolInput), outcome)
		}
		assertEqual(t, outcome.Record, json.NewObject("gate", "guard"))
	}
}

// py: tests/test_gates.py::test_glob_and_shell_spellings_of_the_state_path_are_denied
func TestGlobAndShellSpellingsOfTheStatePathAreDenied(t *testing.T) {
	setUp(t)
	state := stateDir(t)
	for _, command := range []string{"cat " + state[:len(state)-3] + "*/sessions/s.jsonl",
		"cat ~/.local/state/agent-d*/ledger.key", "cat ~/.local/state/*/ledger.key",
		"cat ~/.local/**/ledger.key", "cat ~/.local/sta?e/agent-decisions/ledger.key",
		"cd ~ && cat .local/state/agent-decisions/ledger.key",
		"cat $HOME/.local/state/agent-decisions/ledger.key",
		"cat ${HOME}/.local/state/agent-decisions/ledger.key",
		"cat ~/.local/state/agent-{decisions,x}/ledger.key"} {
		if outcome := guard(t, cmd(command), nil); outcome == nil || outcome.Action != "deny" {
			t.Errorf("%q: got %+v", command, outcome)
		}
	}
}

// py: tests/test_gates.py::test_globs_that_stop_above_the_state_path_are_allowed_like_literal_ancestors
func TestGlobsThatStopAboveTheStatePathAreAllowedLikeLiteralAncestors(t *testing.T) {
	setUp(t)
	users := filepath.Dir(os.Getenv("HOME"))
	for _, command := range []string{"ls ~/*", "du -sh " + users + "/*", "ls ~/.local/*", "ls ~/.local/state",
		"echo $HOME_DIR/x", "ls ~/agent-decisions/{README,SECURITY}.md"} {
		if outcome := guard(t, cmd(command), nil); outcome != nil {
			t.Errorf("%q: got %+v", command, outcome)
		}
	}
}

// py: tests/test_gates.py::test_an_unclosed_brace_after_brace_groups_is_checked_in_linear_time
func TestAnUnclosedBraceAfterBraceGroupsIsCheckedInLinearTime(t *testing.T) {
	setUp(t)
	start := time.Now()
	if outcome := guard(t, cmd("echo "+strings.Repeat("{a,b}", 6)+"{"+strings.Repeat(",", 5000)), nil); outcome != nil {
		t.Fatalf("got %+v", outcome)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

// py: tests/test_gates.py::test_tilde_words_for_unknown_users_do_not_raise
func TestTildeWordsForUnknownUsersDoNotRaise(t *testing.T) {
	setUp(t)
	for _, toolInput := range []*json.Object{cmd("echo ~5% faster; ls ~nosuchuser/x"),
		json.NewObject("file_path", "~nosuchuser/x")} {
		if outcome := guard(t, toolInput, nil); outcome != nil {
			t.Errorf("got %+v", outcome)
		}
	}
}

// py: tests/test_gates.py::test_allows_the_repository_and_everything_else
func TestAllowsTheRepositoryAndEverythingElse(t *testing.T) {
	setUp(t)
	for _, toolInput := range []*json.Object{cmd("cd ~/agent-decisions && git status"),
		json.NewObject("file_path", os.Getenv("HOME")+"/agent-decisions/README.md")} {
		if outcome := guard(t, toolInput, nil); outcome != nil {
			t.Errorf("got %+v", outcome)
		}
	}
}

// --- tests/test_gates.py QuestionGateTest

const asked = `{"questions": [{"question": "Which database for the job queue?", "header": "DB", "multiSelect": false,
	"options": [{"label": "Postgres", "description": "x"}, {"label": "SQLite", "description": "y"}]}]}`

// py: tests/test_gates.py::test_oversized_question_is_rejected_before_the_api
func TestOversizedQuestionIsRejectedBeforeTheApi(t *testing.T) {
	setUp(t)
	api := fake(nil)
	toolInput := json.NewObject("questions", []any{json.NewObject("title", strings.Repeat("x", 250_000))})
	assertAction(t, questionGate(t, toolInput, makeCtx(t, ctxOptions{}), asker(api)), "deny")
	if len(api.Requests()) != 0 {
		t.Fatal("the API was called")
	}
}

// py: tests/test_gates.py::test_codex_question_shape_preserves_prompt_and_string_options
func TestCodexQuestionShapePreservesPromptAndStringOptions(t *testing.T) {
	setUp(t)
	api := fake(map[string]any{"stated_0": 0.95, "owner_0": "user"})
	outcome := questionGate(t, obj(t, `{"questions": [{"title": "Which database?", "options": ["Postgres", "SQLite"]}]}`),
		makeCtx(t, ctxOptions{host: "codex"}), asker(api))
	assertAction(t, outcome, "deny")
	assertEqual(t, api.Requests()[0].Value("state").(*json.Object).Value("questions"),
		loads(t, `[{"question": "Which database?", "options": ["Postgres", "SQLite"]}]`))
}

// py: tests/test_gates.py::test_denies_when_the_user_already_stated_the_answer
func TestDeniesWhenTheUserAlreadyStatedTheAnswer(t *testing.T) {
	setUp(t)
	outcome := questionGate(t, obj(t, asked), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"stated_0": 0.9, "owner_0": "user"})))
	assertAction(t, outcome, "deny")
	assertContains(t, outcome.Message, "already answer")
}

// py: tests/test_gates.py::test_denies_when_the_agent_owns_the_decision
func TestDeniesWhenTheAgentOwnsTheDecision(t *testing.T) {
	setUp(t)
	outcome := questionGate(t, obj(t, asked), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"stated_0": 0.1, "owner_0": "agent"})))
	assertAction(t, outcome, "deny")
	assertContains(t, outcome.Message, "your call")
}

// py: tests/test_gates.py::test_allows_a_user_owned_unanswered_question
func TestAllowsAUserOwnedUnansweredQuestion(t *testing.T) {
	setUp(t)
	outcome := questionGate(t, obj(t, asked), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"stated_0": 0.1, "owner_0": "user"})))
	assertAction(t, outcome, "allow")
	if outcome.Record.Value("gate") != "question" {
		t.Fatalf("record %s", dumps(t, outcome.Record))
	}
}

// py: tests/test_gates.py::test_identical_denial_is_cached_and_still_denied
func TestIdenticalDenialIsCachedAndStillDenied(t *testing.T) {
	setUp(t)
	first := questionGate(t, obj(t, asked), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"stated_0": 0.9})))
	api := fake(map[string]any{"stated_0": 0.9})
	second := questionGate(t, obj(t, asked), makeCtx(t, ctxOptions{entries: []*json.Object{entryOf(first.Record, "action", "deny")}}), asker(api))
	assertAction(t, second, "deny")
	if len(api.Requests()) != 0 {
		t.Fatal("the cached denial asked again")
	}
}

// py: tests/test_gates.py::test_jev_failure_allows_with_a_note_in_availability_mode
func TestJevFailureAllowsWithANoteInAvailabilityMode(t *testing.T) {
	setUp(t)
	availability(t, "claude")
	outcome := questionGate(t, obj(t, asked), makeCtx(t, ctxOptions{}), asker(&jevtest.Fake{Raises: jevError("http_529")}))
	assertAction(t, outcome, "allow")
	assertContains(t, outcome.Note, "http_529")
}

// --- tests/test_gates.py PlanGateTest

// py: tests/test_gates.py::test_every_retry_needs_a_completed_consultation
func TestEveryRetryNeedsACompletedConsultation(t *testing.T) {
	setUp(t)
	ctx := makeCtx(t, ctxOptions{})
	first := planGate(t, ctx, nil)
	assertAction(t, first, "deny")
	again := makeCtx(t, ctxOptions{entries: []*json.Object{entryOf(first.Record, "action", "deny", "turn", ctx.Transcript.TurnKey())}})
	assertAction(t, planGate(t, again, nil), "deny")
}

// py: tests/test_gates.py::test_allows_after_a_backed_ts_decide
func TestAllowsAfterABackedTsDecide(t *testing.T) {
	setUp(t)
	calls := []transcript.ToolCall{{ID: "toolu_d", Name: "Bash", Input: `{"command": "ts-decide '{}'"}`, Output: "ts-decide verdict: ADVISORY"}}
	tr := transcript.Transcript{UserMessages: []string{"plan it"}, ToolCalls: calls, TurnCalls: calls}
	ctx := makeCtx(t, ctxOptions{transcript: &tr, entries: []*json.Object{
		obj(t, `{"gate": "decide", "tool_use_id": "toolu_d", "action": "advisory"}`)}})
	assertAction(t, planGate(t, ctx, nil), "allow")
}

// py: tests/test_gates.py::test_a_forged_ledger_line_does_not_count
func TestAForgedLedgerLineDoesNotCount(t *testing.T) {
	setUp(t)
	ctx := makeCtx(t, ctxOptions{entries: []*json.Object{obj(t, `{"gate": "decide", "tool_use_id": "toolu_forged"}`)}})
	assertAction(t, planGate(t, ctx, nil), "deny")
}

// --- tests/test_adversarial.py AdversarialGateTest

// py: tests/test_adversarial.py::test_printed_fact_is_rejected_even_if_the_model_would_believe_it
func TestPrintedFactIsRejectedEvenIfTheModelWouldBelieveIt(t *testing.T) {
	setUp(t)
	claim := "The integration tests passed with zero failures."
	calls := []transcript.ToolCall{{ID: "echo", Name: "Bash", Input: "printf '" + claim + "'", Output: claim}}
	tr := transcript.Transcript{UserMessages: []string{"Keep existing behavior."}, ToolCalls: calls, TurnCalls: calls}
	request := json.NewObject("decision", "Which cache?", "options", loads(t,
		`[{"id": "a", "description": "Cache A"}, {"id": "b", "description": "Cache B"}]`), "facts", []any{claim})
	api := fake(map[string]any{"sufficient": .99, "unsupported": 0.0})
	assertAction(t, decide(t, request, makeCtx(t, ctxOptions{transcript: &tr}), asker(api)), "invalid")
	if len(api.Requests()) != 0 {
		t.Fatal("the API was called")
	}
}

// py: tests/test_adversarial.py::test_stop_retries_and_active_flag_do_not_authorize_a_choice
func TestStopRetriesAndActiveFlagDoNotAuthorizeAChoice(t *testing.T) {
	setUp(t)
	prior := json.NewObject("gate", "stop", "action", "block", "turn", makeCtx(t, ctxOptions{}).Transcript.TurnKey())
	for _, active := range []bool{false, true} {
		outcome := stopGate(t, "I chose SQLite over PostgreSQL.", active, makeCtx(t, ctxOptions{entries: []*json.Object{prior}}),
			asker(fake(map[string]any{"made_choice": .95})))
		assertAction(t, outcome, "block")
	}
}

// py: tests/test_adversarial.py::test_request_padding_is_rejected_before_the_api
func TestRequestPaddingIsRejectedBeforeTheApi(t *testing.T) {
	setUp(t)
	request := json.NewObject("decision", "Which cache?", "options", loads(t,
		`[{"id": "a", "description": "A"}, {"id": "b", "description": "B"}]`), "padding", strings.Repeat("x", 100_000))
	api := fake(nil)
	assertAction(t, decide(t, request, makeCtx(t, ctxOptions{}), asker(api)), "invalid")
	if len(api.Requests()) != 0 {
		t.Fatal("the API was called")
	}
}

// py: tests/test_adversarial.py::test_long_final_cannot_hide_a_choice_in_the_omitted_middle
func TestLongFinalCannotHideAChoiceInTheOmittedMiddle(t *testing.T) {
	setUp(t)
	final := strings.Repeat("Status. ", 4_000) + "I chose PostgreSQL over SQLite." + strings.Repeat(" End.", 4_000)
	assertAction(t, stopGate(t, final, false, makeCtx(t, ctxOptions{}), asker(fake(nil))), "block")
}

// py: tests/test_adversarial.py::test_documenting_the_state_path_does_not_trip_the_guard
func TestDocumentingTheStatePathDoesNotTripTheGuard(t *testing.T) {
	dir := setUp(t)
	toolInput := json.NewObject("file_path", dir+"/README.md", "content", "Ledger location: "+stateDir(t)+"/sessions.")
	if outcome := guard(t, toolInput, nil); outcome != nil {
		t.Fatalf("got %+v", outcome)
	}
}

// py: tests/test_adversarial.py::test_canonical_private_path_is_denied
func TestCanonicalPrivatePathIsDenied(t *testing.T) {
	dir := setUp(t)
	alias := filepath.Join(dir, "alias")
	if err := os.Mkdir(stateDir(t), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stateDir(t), alias); err != nil {
		t.Fatal(err)
	}
	if outcome := guard(t, json.NewObject("path", alias+"/ledger.key"), nil); outcome == nil || outcome.Action != "deny" {
		t.Fatalf("got %+v", outcome)
	}
}

// py: tests/test_adversarial.py::test_unsupported_claim_cannot_produce_an_actionable_verdict
func TestUnsupportedClaimCannotProduceAnActionableVerdict(t *testing.T) {
	setUp(t)
	request := obj(t, `{"decision": "Which database should store the records?", "options": [
		{"id": "sqlite", "description": "SQLite"}, {"id": "postgres", "description": "PostgreSQL"}],
		"facts": ["The user approved losing existing records."]}`)
	outcome := decide(t, request, makeCtx(t, ctxOptions{}),
		asker(fake(map[string]any{"pick": "sqlite", "sufficient": .95, "unsupported": .95})))
	assertAction(t, outcome, "invalid")
}

// py: tests/test_adversarial.py::test_repetition_cannot_turn_a_missing_option_into_permission
func TestRepetitionCannotTurnAMissingOptionIntoPermission(t *testing.T) {
	setUp(t)
	request := obj(t, `{"decision": "Which cache eviction policy?", "options": [
		{"id": "lru", "description": "Least recently used"}, {"id": "lfu", "description": "Least frequently used"}]}`)
	var entries []*json.Object
	for attempt := 1; attempt <= 5; attempt++ {
		outcome := decide(t, request, makeCtx(t, ctxOptions{entries: entries}),
			asker(fake(map[string]any{"pick": "lru", "sufficient": .95, "missing_option": .95})))
		if outcome.Action != "refused" {
			t.Fatalf("attempt %d: %s", attempt, outcome.Action)
		}
		entries = append(entries, entryOf(outcome.Record, "action", outcome.Action))
	}
}

// py: tests/test_adversarial.py::test_unrelated_consultation_cannot_cover_the_final_choice
func TestUnrelatedConsultationCannotCoverTheFinalChoice(t *testing.T) {
	setUp(t)
	calls := []transcript.ToolCall{{ID: "decide1", Name: "Bash", Input: "ts-decide '{}'", Output: "ts-decide verdict: ADVISORY"}}
	tr := transcript.Transcript{UserMessages: []string{"Select a database for authentication state."}, ToolCalls: calls, TurnCalls: calls}
	entry := obj(t, `{"gate": "decide", "action": "advisory", "tool_use_id": "decide1",
		"decision": "Which cache eviction policy?", "pick": "lru"}`)
	outcome := stopGate(t, "I chose SQLite rather than PostgreSQL.", false, makeCtx(t, ctxOptions{transcript: &tr, entries: []*json.Object{entry}}),
		asker(fake(map[string]any{"made_choice": .95, "uncovered_choice": .99})))
	assertAction(t, outcome, "block")
}

// py: tests/test_adversarial.py::test_api_outage_allows_a_pending_edit_in_availability_mode
func TestApiOutageAllowsAPendingEditInAvailabilityMode(t *testing.T) {
	setUp(t)
	availability(t, "claude")
	outcome := editGate(t, "Edit", json.NewObject("file_path", "parser.py"), makeCtx(t, ctxOptions{}),
		asker(&jevtest.Fake{Raises: jevError("timeout")}))
	assertAction(t, outcome, "allow")
	assertContains(t, outcome.Note, "timeout")
}

// --- tests/test_review_progress.py

const progressRequest = `{"decision": "Which cache policy should the parser use?", "options": [
    {"id": "lru", "description": "Least recently used eviction"},
    {"id": "lfu", "description": "Least frequently used eviction"}],
    "scope": {"paths": ["parser.py"], "invariants": ["Retain each active input lease until parsing ends."]}}`

// progressAnswers ports test_review_progress.answers.
func progressAnswers(t *testing.T, tie bool, invariantLFU float64) Ask {
	base := asker(fake(map[string]any{"pick": "lfu", "sufficient": .95, "owner": "agent",
		"invariant_0_lru": .95, "invariant_0_lfu": invariantLFU}))
	return func(state, questions *json.Object) (*json.Object, error) {
		result, err := base(state, questions)
		if err == nil && tie {
			result.Set("pick", obj(t, `{"type": "choice", "choice": "lfu", "confidence": 0.15,
				"probabilities": {"lru": 0.42, "lfu": 0.43, "none_fit": 0.15}}`))
		}
		return result, err
	}
}

// withField returns {**object, key: value}.
func withField(object *json.Object, key string, value any) *json.Object {
	return entryOf(object, key, value)
}

func patch[T any](t *testing.T, target *T, value T) {
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

// py: tests/test_review_progress.py::test_previous_question_contract_cannot_reuse_a_scoped_gather
func TestPreviousQuestionContractCannotReuseAScopedGather(t *testing.T) {
	setUp(t)
	api := fake(map[string]any{"pick": "lfu", "sufficient": .83, "unsupported": .82,
		"missing_fact": "not_identified", "invariant_0_lru": .60, "invariant_0_lfu": .40})
	var first Outcome
	func() {
		saved := reviewContract
		reviewContract = "scoped-review-1"
		defer func() { reviewContract = saved }()
		first = decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(api))
	}()
	entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action, "verdict", first.Message)
	second := decide(t, progressRequest, makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), asker(api))
	if len(api.Requests()) != 2 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
	assertNotContains(t, second.Message, "reused:")
	if first.Record.Value("evidence_fingerprint") == second.Record.Value("evidence_fingerprint") {
		t.Fatal("the fingerprint did not change with the contract")
	}
}

// py: tests/test_review_progress.py::test_peer_unidentified_gather_names_invariants_without_repeating_comparison
func TestPeerUnidentifiedGatherNamesInvariantsWithoutRepeatingComparison(t *testing.T) {
	setUp(t)
	request := obj(t, `{"decision": "Which isolated Windows build route should capture the trim refusal cause first?",
		"options": [{"id": "single_tu_diagnostic_relink", "description": "Prepare a private diagnostic relink."},
			{"id": "fresh_isolated_complete_build", "description": "Prepare a fresh isolated build."}],
		"scope": {"paths": ["app/shell/"], "invariants": [
			"Preserve the retained source and build artifacts unchanged.",
			"Keep all diagnostic outputs in a separate campaign root."]}}`)
	api := fake(map[string]any{"pick": "single_tu_diagnostic_relink", "sufficient": .83, "unsupported": .82,
		"missing_fact": "not_identified", "evidence_needed": "comparison",
		"invariant_0_single_tu_diagnostic_relink": .40, "invariant_1_single_tu_diagnostic_relink": .60})
	outcome := decide(t, request, makeCtx(t, ctxOptions{}), asker(api))
	assertAction(t, outcome, "gather_evidence")
	if _, ok := outcome.Record.Get("receipt_id"); ok {
		t.Fatal("a gather has a receipt")
	}
	for _, invariant := range request.Value("scope").(*json.Object).Value("invariants").([]any) {
		assertContains(t, outcome.Message, invariant.(string))
	}
	assertContains(t, outcome.Message, "Do not repeat a completed comparison")
	assertNotContains(t, outcome.Message, "Run a check comparing these options")
}

// py: tests/test_review_progress.py::test_recorded_stage_comparison_preserves_negative_cases
func TestRecordedStageComparisonPreservesNegativeCases(t *testing.T) {
	setUp(t)
	for _, item := range []struct {
		unsupported, invariant float64
		expected               string
	}{{.31, .80, "advisory"}, {.44, .05, "refused"}, {.82, .09, "refused"}} {
		api := fake(map[string]any{"pick": "lfu", "sufficient": .95, "unsupported": item.unsupported,
			"invariant_0_lru": .95, "invariant_0_lfu": item.invariant})
		outcome := decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(api))
		assertAction(t, outcome, item.expected)
		if _, ok := outcome.Record.Get("receipt_id"); ok && item.expected != "advisory" {
			t.Fatal("a refusal has a receipt")
		}
	}
}

// py: tests/test_review_progress.py::test_neutral_repair_of_an_argued_question_keeps_identity_and_rechecks
func TestNeutralRepairOfAnArguedQuestionKeepsIdentityAndRechecks(t *testing.T) {
	setUp(t)
	first := decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"argued": .95, "argued_field": "decision"})))
	assertAction(t, first, "refused")
	entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action, "verdict", first.Message)
	repaired := withField(obj(t, progressRequest), "decision", "How should parser cache entries be evicted?")
	second := decide(t, repaired, makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), progressAnswers(t, false, .95))
	assertAction(t, second, "advisory")
	assertEqual(t, second.Record.Value("decision_key"), first.Record.Value("decision_key"))
	assertContains(t, first.Message, "repair field: decision")
}

// py: tests/test_review_progress.py::test_large_invariant_cross_product_is_rejected_before_api_work
func TestLargeInvariantCrossProductIsRejectedBeforeApiWork(t *testing.T) {
	setUp(t)
	var options, invariants []any
	for i := range 9 {
		options = append(options, json.NewObject("id", "option_"+string(rune('0'+i)), "description", "Policy "+string(rune('0'+i))))
	}
	for i := range 32 {
		invariants = append(invariants, "Preserve lease "+dumps(t, json.IntOf(int64(i)))+".")
	}
	request := withField(withField(obj(t, progressRequest), "options", options), "scope",
		json.NewObject("paths", []any{"parser.py"}, "invariants", invariants))
	api := fake(nil)
	outcome := decide(t, request, makeCtx(t, ctxOptions{}), asker(api))
	assertAction(t, outcome, "invalid")
	assertContains(t, outcome.Message, "256 invariant checks")
	if len(api.Requests()) != 0 {
		t.Fatal("the API was called")
	}
}

// py: tests/test_review_progress.py::test_scoped_pick_violating_an_invariant_is_refused
func TestScopedPickViolatingAnInvariantIsRefused(t *testing.T) {
	setUp(t)
	outcome := decide(t, progressRequest, makeCtx(t, ctxOptions{}), progressAnswers(t, false, .01))
	assertAction(t, outcome, "refused")
	assertContains(t, outcome.Message, "invariant_0_lfu")
	assertContains(t, outcome.Message, "Retain each active input lease until parsing ends.")
}

// py: tests/test_review_progress.py::test_each_refusal_reason_names_its_repair
func TestEachRefusalReasonNamesItsRepair(t *testing.T) {
	setUp(t)
	request := obj(t, progressRequest)
	request.Delete("scope")
	for _, item := range []struct {
		overrides      map[string]any
		reason, repair string
	}{
		{map[string]any{"missing_option": .9}, "missing_option", "Add the alternative"},
		{map[string]any{"helper_present": .9}, "missing_existing_helper", "helper_present 0.90"},
		{map[string]any{"specified_lru": .9}, "pick_conflicts_with_instruction", "prescribe lru"},
		{map[string]any{"pick": "none_fit"}, "none_fit", "Add the option that the evidence supports"},
	} {
		answers := map[string]any{"pick": "lfu", "sufficient": .95, "owner": "agent"}
		for key, value := range item.overrides {
			answers[key] = value
		}
		outcome := decide(t, request, makeCtx(t, ctxOptions{}), asker(fake(answers)))
		assertAction(t, outcome, "refused")
		assertContains(t, outcome.Message, "Refused ("+item.reason+")")
		assertContains(t, outcome.Message, "repair: ")
		assertContains(t, outcome.Message, item.repair)
	}
}

// py: tests/test_review_progress.py::test_worker_near_tie_requires_parent_review_without_a_receipt
func TestWorkerNearTieRequiresParentReviewWithoutAReceipt(t *testing.T) {
	setUp(t)
	outcome := decide(t, progressRequest, makeCtx(t, ctxOptions{agentID: "worker"}), progressAnswers(t, true, .95))
	assertAction(t, outcome, "parent_review")
	if _, ok := outcome.Record.Get("receipt_id"); ok {
		t.Fatal("a parent review has a receipt")
	}
}

// py: tests/test_review_progress.py::test_main_near_tie_is_advisory_and_reports_uncertain_preference
func TestMainNearTieIsAdvisoryAndReportsUncertainPreference(t *testing.T) {
	setUp(t)
	outcome := decide(t, progressRequest, makeCtx(t, ctxOptions{}), progressAnswers(t, true, .95))
	assertAction(t, outcome, "advisory")
	assertContains(t, outcome.Message, "preference: uncertain")
	assertEqual(t, outcome.Record.Value("eligible_options"), []any{"lru", "lfu"})
}

// py: tests/test_review_progress.py::test_scoped_identity_survives_a_neutral_question_repair
func TestScopedIdentitySurvivesANeutralQuestionRepair(t *testing.T) {
	setUp(t)
	first := decide(t, progressRequest, makeCtx(t, ctxOptions{}), progressAnswers(t, false, .95))
	repaired := withField(obj(t, progressRequest), "decision", "How should parser cache entries be evicted?")
	second := decide(t, repaired, makeCtx(t, ctxOptions{}), progressAnswers(t, false, .95))
	assertEqual(t, first.Record.Value("decision_key"), second.Record.Value("decision_key"))
}

// py: tests/test_review_progress.py::test_question_rephrasing_cannot_reroll_an_unchanged_scoped_review
func TestQuestionRephrasingCannotRerollAnUnchangedScopedReview(t *testing.T) {
	setUp(t)
	api := fake(map[string]any{"pick": "lfu", "sufficient": .95, "invariant_0_lru": .95, "invariant_0_lfu": .95})
	first := decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(api))
	entry := entryOf(first.Record, "gate", "decide", "host", "claude", "agent_id", nil, "action", first.Action, "verdict", first.Message)
	repaired := withField(obj(t, progressRequest), "decision", "How should parser cache entries be evicted?")
	second := decide(t, repaired, makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), asker(api))
	assertAction(t, second, "advisory")
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
	assertContains(t, second.Message, first.Record.Value("receipt_id").(string))
}

func TestRoutineReuseRequiresTheSameDecisionText(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "material_clarification"}[changed], func(t *testing.T) {
			setUp(t)
			api := fake(map[string]any{"pick": "lfu", "sufficient": .95, "routine_operation": .95,
				"invariant_0_lru": .95, "invariant_0_lfu": .95})
			first := decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(api))
			assertAction(t, first, "routine")
			assertContains(t, first.Message, "classification: exact_operation=")
			assertContains(t, first.Message, "routine_operation=0.95")
			assertContains(t, first.Message, "PROCEED does not select a material approach")
			entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action, "verdict", first.Message)
			request := obj(t, progressRequest)
			if changed {
				request.Set("decision", "Select an unimplemented cache policy. No policy has been selected; this is a material choice, not routine implementation.")
			}
			calls := 0
			ask := func(state, questions *json.Object) (*json.Object, error) {
				calls++
				return progressAnswers(t, false, .95)(state, questions)
			}
			second := decide(t, request, makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), ask)
			if changed {
				if calls != 1 {
					t.Fatalf("changed decision reused PROCEED without a classification check")
				}
				assertAction(t, second, "advisory")
				if _, ok := second.Record.Get("receipt_id"); !ok {
					t.Fatal("the completed material review has no receipt")
				}
			} else {
				if calls != 0 {
					t.Fatal("an unchanged routine request was reviewed again")
				}
				assertAction(t, second, "routine")
			}
		})
	}
}

func TestQuestionRephrasingCannotRerollAnUnchangedScopedDenial(t *testing.T) {
	for _, tc := range []struct {
		action string
		field  string
		value  any
	}{
		{"gather_evidence", "unsupported", .95},
		{"refused", "missing_option", .95},
		{"ask_user", "owner", "user"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			setUp(t)
			api := fake(map[string]any{"pick": "lfu", "sufficient": .95, "invariant_0_lru": .95,
				"invariant_0_lfu": .95, tc.field: tc.value})
			first := decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(api))
			assertAction(t, first, tc.action)
			entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action, "verdict", first.Message)
			repaired := withField(obj(t, progressRequest), "decision", "How should parser cache entries be evicted?")
			second := decide(t, repaired, makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), asker(api))
			assertAction(t, second, tc.action)
			if len(api.Requests()) != 1 {
				t.Fatalf("%d requests", len(api.Requests()))
			}
		})
	}
}

// py: tests/test_review_progress.py::test_new_observed_source_requires_a_fresh_review
func TestNewObservedSourceRequiresAFreshReview(t *testing.T) {
	setUp(t)
	api := fake(map[string]any{"pick": "lfu", "sufficient": .95, "invariant_0_lru": .95, "invariant_0_lfu": .95})
	first := decide(t, progressRequest, makeCtx(t, ctxOptions{}), asker(api))
	entry := entryOf(first.Record, "gate", "decide", "host", "claude", "agent_id", nil, "action", first.Action, "verdict", first.Message)
	current := makeCtx(t, ctxOptions{entries: []*json.Object{entry}})
	current.Evidence.Set("source_files", loads(t, `[{"path": "parser.py", "sha256": "new-content", "text": "changed"}]`))
	decide(t, progressRequest, current, asker(api))
	if len(api.Requests()) != 2 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// py: tests/test_review_progress.py::test_missing_implementation_check_names_the_observed_helper_path
func TestMissingImplementationCheckNamesTheObservedHelperPath(t *testing.T) {
	setUp(t)
	context := makeCtx(t, ctxOptions{})
	context.Evidence.Set("source_files", loads(t, `[{"path": "src/owner.py", "sha256": "observed", "text": "owner code"}]`))
	outcome := decide(t, progressRequest, context, asker(fake(map[string]any{"pick": "lfu", "sufficient": .95,
		"unsupported": .95, "evidence_needed": "implementation", "invariant_0_lru": .95, "invariant_0_lfu": .95})))
	assertAction(t, outcome, "gather_evidence")
	assertContains(t, outcome.Message, "src/owner.py")
}

// --- tests/test_required_review.py

// py: tests/test_required_review.py::test_timeout_cannot_authorize_an_unchecked_write_by_default
func TestTimeoutCannotAuthorizeAnUncheckedWriteByDefault(t *testing.T) {
	setUp(t)
	outcome := editGate(t, "Edit", obj(t, `{"file_path": "parser.py", "new_string": "read_chunks()"}`),
		makeCtx(t, ctxOptions{}), asker(&jevtest.Fake{Raises: jevError("timeout")}))
	assertAction(t, outcome, "deny")
	assertContains(t, strings.ToLower(outcome.Message), "required")
}

// py: tests/test_required_review.py::test_signed_operator_availability_policy_retains_visible_fail_open
func TestSignedOperatorAvailabilityPolicyRetainsVisibleFailOpen(t *testing.T) {
	setUp(t)
	availability(t, "claude")
	outcome := editGate(t, "Edit", obj(t, `{"file_path": "parser.py", "new_string": "read_chunks()"}`),
		makeCtx(t, ctxOptions{}), asker(&jevtest.Fake{Raises: jevError("timeout")}))
	assertAction(t, outcome, "allow")
	assertContains(t, outcome.Note, "action allowed")
}

// --- tests/test_qa_tools.py

func pendingOf(t *testing.T, toolInput *json.Object) []*json.Object {
	t.Helper()
	result, err := pendingParts(toolInput, gateFrame+1)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fieldsOf(parts []*json.Object) []*json.Object {
	var fields []*json.Object
	for _, part := range parts {
		for _, field := range part.Value("fields").([]any) {
			fields = append(fields, field.(*json.Object))
		}
	}
	return fields
}

// joined returns "".join(field["value"] for field in fields if field["path"] == path).
func joined(t *testing.T, fields []*json.Object, path []any) string {
	var text strings.Builder
	for _, field := range fields {
		if equal(t, field.Value("path"), path) {
			text.WriteString(field.Value("value").(string))
		}
	}
	return text.String()
}

// py: tests/test_qa_tools.py::test_large_edit_preserves_existing_and_proposed_field_roles
func TestLargeEditPreservesExistingAndProposedFieldRoles(t *testing.T) {
	toolInput := json.NewObject("file_path", "parser.py", "old_string", strings.Repeat("old line\n", 2_000),
		"new_string", strings.Repeat("new line\n", 2_000), "replace_all", false)
	result := pendingOf(t, toolInput)
	fields := fieldsOf(result)
	for _, key := range []string{"old_string", "new_string"} {
		if joined(t, fields, []any{key}) != toolInput.Value(key) {
			t.Fatalf("%s was not preserved", key)
		}
	}
	for _, part := range result {
		if part.Value("operation_context").(*json.Object).Value("file_path") != "parser.py" || len(dumps(t, part)) > 16_000 {
			t.Fatalf("part %s", dumps(t, part)[:200])
		}
	}
}

// py: tests/test_qa_tools.py::test_chunked_nested_multiedit_keeps_paths_and_boolean_values
func TestChunkedNestedMultieditKeepsPathsAndBooleanValues(t *testing.T) {
	data := json.NewObject("file_path", "parser.py", "edits", []any{json.NewObject(
		"old_string", strings.Repeat("x", 20_000), "new_string", strings.Repeat("y", 20_000), "replace_all", false)})
	fields := fieldsOf(pendingOf(t, data))
	want := obj(t, `{"path": ["edits", 0, "replace_all"], "value": false}`)
	found := false
	for _, field := range fields {
		found = found || equal(t, field, want)
	}
	if !found {
		t.Fatal("replace_all field missing")
	}
	if joined(t, fields, []any{"edits", json.IntOf(0), "new_string"}) != strings.Repeat("y", 20_000) {
		t.Fatal("new_string was not preserved")
	}
}

// py: tests/test_qa_tools.py::test_large_write_cannot_hide_the_choice_between_the_head_and_tail
func TestLargeWriteCannotHideTheChoiceBetweenTheHeadAndTail(t *testing.T) {
	setUp(t)
	text := strings.Repeat("# padding\n", 2_000) + "read_chunks()" + strings.Repeat("\n# padding", 2_000)
	api := fake(map[string]any{"needs_decision": .99})
	outcome := editGate(t, "Write", json.NewObject("file_path", "parser.py", "content", text), makeCtx(t, ctxOptions{}), asker(api))
	assertAction(t, outcome, "deny")
	seen := false
	for _, request := range api.Requests() {
		pending := dumps(t, request.Value("state").(*json.Object).Value("pending_input"))
		seen = seen || strings.Contains(pending, "read_chunks()")
		if len(pending) >= 20_000 {
			t.Fatalf("pending input of %d characters", len(pending))
		}
	}
	if !seen {
		t.Fatal("no request showed the choice")
	}
}

// py: tests/test_qa_tools.py::test_chunk_focus_exposes_code_surrounded_by_comments
func TestChunkFocusExposesCodeSurroundedByComments(t *testing.T) {
	text := strings.Repeat("# fixture\n", 2_000) + "read_chunks()\n" + strings.Repeat("# fixture\n", 2_000)
	seen := false
	for _, part := range pendingOf(t, json.NewObject("file_path", "parser.py", "content", text)) {
		excerpt, _ := part.Value("proposed_excerpt").(string)
		seen = seen || strings.Contains(excerpt, "read_chunks()")
		if len(dumps(t, part)) > 16_000 {
			t.Fatal("part exceeds the clip")
		}
	}
	if !seen {
		t.Fatal("no excerpt showed the code")
	}
}

// py: tests/test_qa_tools.py::test_interactive_code_cannot_mutate_files_through_unhooked_stdin
func TestInteractiveCodeCannotMutateFilesThroughUnhookedStdin(t *testing.T) {
	setUp(t)
	for _, command := range []string{"python3", "bash -i", "node -i", "python3 -", "python3 -u -",
		"bash -s", "bash -se", "sh -s", "node -", "python3 -m code", "python3 -mcode"} {
		outcome := editGate(t, "Bash", cmd(command), makeCtx(t, ctxOptions{}), asker(fake(nil)))
		assertAction(t, outcome, "deny")
		assertContains(t, outcome.Message, "stdin")
	}
}

// py: tests/test_qa_tools.py::test_data_stdin_and_test_progress_are_allowed
func TestDataStdinAndTestProgressAreAllowed(t *testing.T) {
	setUp(t)
	for _, command := range []string{"pytest -q", "cat", "python3 -c 'print(1)'", "npm test",
		"python3 - <<'PY'\nprint(1)\nPY", "bash -s <<'SH'\nprintf ok\nSH", "python3 - < inspect.py"} {
		outcome := editGate(t, "Bash", json.NewObject("command", command, "tty", true), makeCtx(t, ctxOptions{}), asker(fake(nil)))
		assertAction(t, outcome, "allow")
	}
}

// py: tests/test_qa_tools.py::test_quoted_redirect_token_does_not_authorize_future_code_stdin
func TestQuotedRedirectTokenDoesNotAuthorizeFutureCodeStdin(t *testing.T) {
	setUp(t)
	assertAction(t, editGate(t, "Bash", cmd("python3 - '<<'"), makeCtx(t, ctxOptions{}), asker(fake(nil))), "deny")
}

// py: tests/test_qa_tools.py::test_opaque_evaluator_is_denied_with_a_specific_repair
func TestOpaqueEvaluatorIsDeniedWithASpecificRepair(t *testing.T) {
	setUp(t)
	outcome := editGate(t, "Bash", cmd("python3 -c 'exec(input())'"), makeCtx(t, ctxOptions{}),
		asker(fake(map[string]any{"accepts_code_input": .99})))
	assertAction(t, outcome, "deny")
	assertContains(t, outcome.Message, "complete")
}

// py: tests/test_qa_tools.py::test_api_outage_allows_with_a_visible_note_in_availability_mode
func TestApiOutageAllowsWithAVisibleNoteInAvailabilityMode(t *testing.T) {
	setUp(t)
	availability(t, "claude")
	outcome := editGate(t, "Bash", cmd("python3"), makeCtx(t, ctxOptions{}), asker(&jevtest.Fake{Raises: jevError("timeout")}))
	assertAction(t, outcome, "allow")
	assertContains(t, outcome.Note, "timeout")
}

// --- tests/test_metrics.py

const colour = `{"questions": [{"question": "Which colour?", "options": []}]}`

// py: tests/test_metrics.py::test_model_change_invalidates_identical_gate_cache
func TestModelChangeInvalidatesIdenticalGateCache(t *testing.T) {
	setUp(t)
	first := questionGate(t, obj(t, colour), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"owner_0": "user"})))
	entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action)
	api := fake(map[string]any{"owner_0": "user"})
	patch(t, &model, "jev-next")
	questionGate(t, obj(t, colour), makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), asker(api))
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// py: tests/test_metrics.py::test_operator_policy_change_invalidates_identical_gate_cache
func TestOperatorPolicyChangeInvalidatesIdenticalGateCache(t *testing.T) {
	setUp(t)
	first := questionGate(t, obj(t, colour), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"owner_0": "user"})))
	entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action)
	availability(t, "claude")
	api := fake(map[string]any{"owner_0": "user"})
	questionGate(t, obj(t, colour), makeCtx(t, ctxOptions{entries: []*json.Object{entry}}), asker(api))
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// --- tests/test_ledger.py

// py: tests/test_ledger.py::test_expired_cache_requires_a_fresh_review
func TestExpiredCacheRequiresAFreshReview(t *testing.T) {
	setUp(t)
	ctx := makeCtx(t, ctxOptions{})
	toolInput := obj(t, `{"file_path": "parser.py", "new_string": "read_chunks()"}`)
	first := editGate(t, "Edit", toolInput, ctx, asker(fake(map[string]any{"needs_decision": 0.0})))
	if err := ledger.Append(merge(json.NewObject("session_id", "s1", "host", "claude", "agent_id", nil), first.Record)); err != nil {
		t.Fatal(err)
	}
	entries := func(limit *int) []*json.Object {
		rows, err := ledger.Entries("s1", limit)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	api := fake(map[string]any{"needs_decision": 0.95})
	assertAction(t, editGate(t, "Edit", toolInput, makeCtx(t, ctxOptions{entries: entries(nil)}), asker(api)), "allow")
	if len(api.Requests()) != 0 {
		t.Fatal("the cached review asked again")
	}
	zero := 0
	assertAction(t, editGate(t, "Edit", toolInput, makeCtx(t, ctxOptions{entries: entries(&zero)}), asker(api)), "deny")
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// --- tests/test_qa_context.py GateCacheQATest

// py: tests/test_qa_context.py::test_source_with_spaces_changes_invalidate_cache
func TestSourceWithSpacesChangesInvalidateCache(t *testing.T) {
	dir := setUp(t)
	folder := filepath.Join(dir, "project with spaces")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "config.py")
	write := func(text string) {
		if err := os.WriteFile(source, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("VALUE = 1\n")
	pending := json.NewObject("file_path", source, "old_string", "VALUE = 1", "new_string", "VALUE = 2")
	tr := transcript.Transcript{UserMessages: []string{"Update configuration."}}
	build := func() *json.Object {
		bundle, err := evidence.Build(tr, nil, evidence.Options{Query: dumps(t, pending), Cwd: &dir, Frame: 3})
		if err != nil {
			t.Fatal(err)
		}
		return bundle
	}
	ctx := makeCtx(t, ctxOptions{transcript: &tr})
	ctx.Evidence = build()
	first := editGate(t, "Edit", pending, ctx, asker(fake(map[string]any{"needs_decision": .01})))
	write("VALUE = 3\n")
	changed := ctx
	changed.Evidence = build()
	changed.Entries = []*json.Object{entryOf(first.Record, "host", ctx.Host, "agent_id", nil)}
	api := fake(map[string]any{"needs_decision": .99})
	assertAction(t, editGate(t, "Edit", pending, changed, asker(api)), "deny")
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// py: tests/test_qa_context.py::test_cache_does_not_cross_host_boundary
func TestCacheDoesNotCrossHostBoundary(t *testing.T) {
	setUp(t)
	firstCtx := makeCtx(t, ctxOptions{host: "claude"})
	pending := obj(t, `{"file_path": "parser.py", "content": "parse_chunks()"}`)
	first := editGate(t, "Write", pending, firstCtx, asker(fake(map[string]any{"needs_decision": .01})))
	other := firstCtx
	other.Host = "codex"
	other.Entries = []*json.Object{entryOf(first.Record, "host", "claude", "agent_id", nil)}
	api := fake(map[string]any{"needs_decision": .99})
	assertAction(t, editGate(t, "Write", pending, other, asker(api)), "deny")
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// contextForSource ports GateCacheQATest.context_for_source.
func contextForSource(t *testing.T, dir string, agentID any) Context {
	t.Helper()
	tr := transcript.Transcript{UserMessages: []string{"Update config.py."}}
	bundle, err := evidence.Build(tr, nil, evidence.Options{Query: "config.py", Cwd: &dir, Frame: 3})
	if err != nil {
		t.Fatal(err)
	}
	return Context{Host: "claude", SessionID: "s1", AgentID: agentID, ToolUseID: "toolu_now", Transcript: tr, Evidence: bundle}
}

// py: tests/test_qa_context.py::test_source_change_invalidates_reused_gate_approval
func TestSourceChangeInvalidatesReusedGateApproval(t *testing.T) {
	dir := setUp(t)
	source := filepath.Join(dir, "config.py")
	if err := os.WriteFile(source, []byte("VALUE = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := cmd("python update_config.py config.py")
	first := editGate(t, "Bash", pending, contextForSource(t, dir, nil),
		asker(fake(map[string]any{"writes_files": .99, "needs_decision": .01})))
	if err := os.WriteFile(source, []byte("VALUE = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := contextForSource(t, dir, nil)
	second.Entries = []*json.Object{entryOf(first.Record, "host", second.Host, "action", first.Action, "agent_id", second.AgentID)}
	api := fake(map[string]any{"writes_files": .99, "needs_decision": .99})
	assertAction(t, editGate(t, "Bash", pending, second, asker(api)), "deny")
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// py: tests/test_qa_context.py::test_signed_verdict_requires_exact_body_and_matching_host
func TestSignedVerdictRequiresExactBodyAndMatchingHost(t *testing.T) {
	setUp(t)
	full := "ts-decide verdict: ADVISORY\ndecision: Which cache?\npick: lru\nnext: Follow."
	call := transcript.ToolCall{ID: "toolu_d", Name: "Bash", Input: `{"command": "ts-decide '{}'"}`, Output: full}
	tr := transcript.Transcript{UserMessages: []string{"Fix cache."}, ToolCalls: []transcript.ToolCall{call}, TurnCalls: []transcript.ToolCall{call}}
	entry := json.NewObject("host", "claude", "gate", "decide", "action", "advisory", "tool_use_id", "toolu_d", "verdict", full)
	cut := call
	cut.Output = "ts-decide verdict: ADVISORY"
	truncated := transcript.Transcript{UserMessages: []string{"Fix cache."}, ToolCalls: []transcript.ToolCall{cut},
		TurnCalls: []transcript.ToolCall{call}}
	for _, ctx := range []Context{makeCtx(t, ctxOptions{transcript: &truncated, entries: []*json.Object{entry}}),
		makeCtx(t, ctxOptions{transcript: &tr, entries: []*json.Object{entry}, host: "codex"})} {
		consulted, err := Consulted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(consulted) != 0 {
			t.Fatalf("consulted %s", dumps(t, consulted[0]))
		}
	}
}

// py: tests/test_qa_context.py::test_cross_agent_cached_answers_do_not_cover_current_agent
func TestCrossAgentCachedAnswersDoNotCoverCurrentAgent(t *testing.T) {
	setUp(t)
	ctx := makeCtx(t, ctxOptions{agentID: "a1"})
	first := editGate(t, "Bash", cmd("python update.py"), ctx, asker(fake(map[string]any{"writes_files": .99, "needs_decision": .01})))
	reused := entryOf(first.Record, "action", first.Action, "agent_id", "other-agent")
	api := fake(map[string]any{"writes_files": .99, "needs_decision": .99})
	second := editGate(t, "Bash", cmd("python update.py"), makeCtx(t, ctxOptions{entries: []*json.Object{reused}, agentID: "a1"}), asker(api))
	assertAction(t, second, "deny")
	if len(api.Requests()) != 1 {
		t.Fatalf("%d requests", len(api.Requests()))
	}
}

// --- tests/test_jev.py ContextLimitTest (the gate subjects)

// limitContext ports ContextLimitTest.context.
func limitContext(t *testing.T) Context {
	ctx := makeCtx(t, ctxOptions{host: "codex"})
	update := obj(t, `{
		"user_messages": ["Preserve the guard.", "fix it asap"],
		"brief": "Keep the required review policy.",
		"tool_calls_newest_first": [{"id": "native-read", "tool": "exec_command",
			"input": "sed -n '1,400p' scripts/common.sh", "output": `+dumps(t, strings.Repeat("diagnostic evidence\n", 800))+`,
			"trust": "unverified_tool_observation", "execution": {"cwd": "/other/worktree"}}],
		"source_files": [
			{"path": "scripts/common.sh", "text": `+dumps(t, strings.Repeat("helper source\n", 1000))+`,
			 "sha256": "helper-hash", "trust": "verified_file_observation", "truncated": false},
			{"path": "AGENTS.md", "text": `+dumps(t, strings.Repeat("Mandatory rules\n", 500))+`, "sha256": "rules-hash"},
			{"path": "contract.md", "text": `+dumps(t, strings.Repeat("Mandatory contract\n", 500))+`, "sha256": "contract-hash"}],
		"contract_observations": {"contract.md": {"sha256": "contract-hash"}}}`)
	for _, key := range update.Keys() {
		ctx.Evidence.Set(key, update.Value(key))
	}
	return ctx
}

// recorder records request bodies from concurrent posts.
type recorder struct {
	mu       sync.Mutex
	requests []*json.Object
	timeouts []time.Duration
}

func (r *recorder) record(t *testing.T, body []byte, timeout time.Duration) (*json.Object, int) {
	request := obj(t, string(body))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	r.timeouts = append(r.timeouts, timeout)
	return request, len(r.requests)
}

func (r *recorder) all() []*json.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*json.Object(nil), r.requests...)
}

var contextLimit = &jev.JevError{Reason: "http_400", Detail: "max_tokens_exceeded"}

// transport ports ContextLimitTest.transport without the pause, which only the jev test uses.
func transport(t *testing.T, alwaysFail bool) (jev.PostFunc, *recorder) {
	r := &recorder{}
	api := fake(nil)
	return func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		if _, n := r.record(t, body, timeout); n == 1 || alwaysFail {
			return nil, contextLimit
		}
		return api.Post(body, key, timeout)
	}, r
}

// without returns {k: v for k, v in object.items() if k != key}.
func without(object *json.Object, key string) *json.Object {
	result := object.Copy()
	result.Delete(key)
	return result
}

func copyOf(t *testing.T, v any) any {
	return loads(t, dumps(t, v))
}

// py: tests/test_jev.py::test_retry_preserves_authority_and_records_actual_native_read_state
func TestRetryPreservesAuthorityAndRecordsActualNativeReadState(t *testing.T) {
	setUp(t)
	ctx := limitContext(t)
	original := copyOf(t, ctx.Evidence).(*json.Object)
	post, r := transport(t, false)
	outcome := editGate(t, "mcp__codex_app__read_thread", json.NewObject("threadId", "peer"), ctx, hookAsker(post))
	assertAction(t, outcome, "allow")
	requests := r.all()
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	assertEqual(t, outcome.Record.Value("state"), requests[1].Value("state"))
	assertEqual(t, ctx.Evidence, original)
	first, second := requests[0].Value("state").(*json.Object), requests[1].Value("state").(*json.Object)
	assertEqual(t, without(first, "evidence"), without(second, "evidence"))
	evidence := second.Value("evidence").(*json.Object)
	for _, name := range []string{"user_messages", "brief", "contract_observations"} {
		assertEqual(t, evidence.Value(name), original.Value(name))
	}
	assertEqual(t, evidence.Value("source_files").([]any)[1:], original.Value("source_files").([]any)[1:])
	call := evidence.Value("tool_calls_newest_first").([]any)[0].(*json.Object)
	assertEqual(t, without(call, "output"), without(original.Value("tool_calls_newest_first").([]any)[0].(*json.Object), "output"))
	assertContains(t, call.Value("output").(string), "characters cut")
	if evidence.Value("source_files").([]any)[0].(*json.Object).Value("truncated") != true {
		t.Fatal("the helper source was not truncated")
	}
	if cut := evidence.Value("metadata").(*json.Object).Value("context_limit_chars_cut"); !json.Truthy(cut) {
		t.Fatalf("context_limit_chars_cut %v", cut)
	}
}

// py: tests/test_jev.py::test_retry_failure_still_denies_with_exact_last_attempt_state
func TestRetryFailureStillDeniesWithExactLastAttemptState(t *testing.T) {
	setUp(t)
	post, r := transport(t, true)
	outcome := editGate(t, "mcp__codex_app__wait_threads", obj(t, `{"timeoutMs": 0}`), limitContext(t), hookAsker(post))
	assertAction(t, outcome, "deny")
	requests := r.all()
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	assertEqual(t, outcome.Record.Value("state"), requests[1].Value("state"))
	for _, part := range []string{"max_tokens_exceeded", "attempt_count=2", "user_messages="} {
		assertContains(t, outcome.Message, part)
	}
	assertNotContains(t, outcome.Message, "Mandatory rules")
}

// py: tests/test_jev.py::test_peer_dispatch_recovers_when_the_old_first_reduction_is_still_too_large
func TestPeerDispatchRecoversWhenTheOldFirstReductionIsStillTooLarge(t *testing.T) {
	setUp(t)
	ctx := limitContext(t)
	original := copyOf(t, ctx.Evidence).(*json.Object)
	r := &recorder{}
	api := fake(nil)
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		r.record(t, body, timeout)
		if len(body) > 27000 {
			return nil, contextLimit
		}
		return api.Post(body, key, timeout)
	}
	pending := obj(t, `{"task_name": "v45_trim_failure_trace", "model": "gpt-6-luna",
		"reasoning_effort": "max", "fork_turns": "none",
		"message": "Read-only source trace. Do not edit files or execute remote commands."}`)
	outcome := editGate(t, "spawn_agent", pending, ctx, hookAsker(post))
	assertAction(t, outcome, "allow")
	requests := r.all()
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	last := requests[1].Value("state").(*json.Object)
	assertEqual(t, outcome.Record.Value("state"), last)
	assertEqual(t, last.Value("pending_input"), pending)
	assertEqual(t, ctx.Evidence, original)
	evidence := last.Value("evidence").(*json.Object)
	assertEqual(t, evidence.Value("user_messages"), original.Value("user_messages"))
	assertEqual(t, evidence.Value("source_files").([]any)[1:], original.Value("source_files").([]any)[1:])
}

// py: tests/test_jev.py::test_concurrent_chunks_record_their_independent_review_states
func TestConcurrentChunksRecordTheirIndependentReviewStates(t *testing.T) {
	setUp(t)
	ctx := limitContext(t)
	original := copyOf(t, ctx.Evidence).(*json.Object)
	r := &recorder{}
	api := fake(nil)
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		request, _ := r.record(t, body, timeout)
		metadata := request.Value("state").(*json.Object).Value("evidence").(*json.Object).Value("metadata").(*json.Object)
		if _, ok := metadata.Get("context_limit_chars_cut"); !ok {
			return nil, contextLimit
		}
		return api.Post(body, key, timeout)
	}
	pending := json.NewObject("file_path", "test.py", "old_string", strings.Repeat("x", 20000), "new_string", strings.Repeat("y", 20000))
	outcome := editGate(t, "Edit", pending, ctx, hookAsker(post))
	assertAction(t, outcome, "allow")
	assertEqual(t, ctx.Evidence, original)
	var reviewed []any
	for _, request := range r.all() {
		state := request.Value("state").(*json.Object)
		if _, ok := state.Value("evidence").(*json.Object).Value("metadata").(*json.Object).Get("context_limit_chars_cut"); ok {
			reviewed = append(reviewed, state)
		}
	}
	chunks := outcome.Record.Value("chunks").([]any)
	if len(chunks) != len(reviewed) {
		t.Fatalf("%d chunks, %d reviewed states", len(chunks), len(reviewed))
	}
	for _, chunk := range chunks {
		state := chunk.(*json.Object).Value("state")
		found := false
		for _, candidate := range reviewed {
			found = found || equal(t, state, candidate)
		}
		if !found {
			t.Fatal("a chunk state was never reviewed")
		}
	}
}

// --- port checks

// The Python RecursionError for a too deep tool input: guard.collect enters frame 1001 at depth 995.
func TestGuardRecursionLimit(t *testing.T) {
	setUp(t)
	nested := func(depth int) *json.Object {
		var value any = "x"
		for range depth {
			value = []any{value}
		}
		return json.NewObject("paths", value)
	}
	if _, err := Guard(nested(993), nil, guardFrame); err != nil {
		t.Fatalf("depth 993: %v", err)
	}
	var recursion *json.RecursionError
	if _, err := Guard(nested(994), nil, guardFrame); !errors.As(err, &recursion) {
		t.Fatalf("depth 994: %v", err)
	}
}
