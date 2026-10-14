package transcript_test

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/provenance"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// oracleScript answers one JSON request per stdin line with one json.dumps reply per stdout line. Text
// arrives as hex of its UTF-8 bytes with surrogatepass, so lone surrogates and NUL bytes survive. A "load"
// reply summarizes every Transcript field, the derived calls, and provenance.verify over facts drawn from
// the transcript; with digest set, each field (or each item of a list field) is replaced by the sha256 of
// its json.dumps text, so no transcript content leaves the process.
const oracleScript = `
import functools, hashlib, json, sys
sys.path.insert(0, sys.argv[1])
from agent_decisions import provenance, transcript
from agent_decisions.transcript import Transcript

def text(wire):
    return bytes.fromhex(wire).decode("utf-8", "surrogatepass")

def outcome(call):
    try:
        return ["ok", call()]
    except Exception as error:
        return [type(error).__name__, str(error)]

def calls(items):
    return [[c.id, c.name, c.input, c.output, c.execution] for c in items]

def facts(t):
    found = ["", "yes", "x" * 11, "the user said memory is not a concern"]
    for message in t.user_messages:
        found += [message, message[1:-1], message.casefold(), " ".join(message.split()[:3])]
        try:
            found += list(transcript.question_reply_words(message))
        except RecursionError:
            pass
    found += [call.output[:60] for call in t.tool_calls]
    if t.brief is not None:
        found.append(t.brief)
    return found

def summary(t):
    i = t.identity
    found = facts(t)
    return {
        "user_messages": list(t.user_messages),
        "tool_calls": calls(t.tool_calls),
        "turn_calls": calls(t.turn_calls),
        "last_assistant_text": t.last_assistant_text,
        "brief": t.brief,
        "user_keys": list(t.user_keys),
        "question_reply_keys": list(t.question_reply_keys),
        "identity": None if i is None else [i.thread_id, i.session_id, i.parent_thread_id, i.agent_id, i.cwd],
        "turn_key": t.turn_key,
        "is_decide": [outcome(lambda c=c: transcript.is_decide(c)) for c in t.tool_calls],
        "question_reply_words": [outcome(lambda m=m: list(transcript.question_reply_words(m)))
                                 for m in t.user_messages],
        "facts": found,
        "verify": outcome(lambda: list(provenance.verify(found, t))),
    }

def digest(value):
    sha = lambda item: hashlib.sha256(json.dumps(item).encode()).hexdigest()
    return {key: [sha(item) for item in field] if isinstance(field, list) else sha(field)
            for key, field in value.items()}

def at_frame(frame, call):
    """Run call() as frame number frame of the Python stack, counting <module> as 1."""
    depth, f = 0, sys._getframe()
    while f:
        depth, f = depth + 1, f.f_back
    return call() if depth == frame - 1 else at_frame(frame, call)

for line in sys.stdin:
    op, *args = json.loads(line)
    if op == "load":
        reply = outcome(lambda: summary(transcript.load(text(args[0]), args[1])))
        if args[2] and reply[0] == "ok":
            reply = ["ok", digest(reply[1])]
        elif args[2]:
            reply = [reply[0], ""]
    elif op == "load_at":
        reply = outcome(lambda: at_frame(args[2], functools.partial(transcript.load, text(args[0]), args[1])) and None)
    elif op == "is_decide":
        reply = outcome(lambda: transcript.is_decide(transcript.ToolCall("c", "Bash", text(args[0]))))
    elif op == "reply_words":
        reply = outcome(lambda: list(transcript.question_reply_words(text(args[0]))))
    elif op == "verify":
        users = tuple(text(user) for user in args[1])
        reply = outcome(lambda: list(provenance.verify([text(f) for f in args[0]], Transcript(users, (), ()))))
    print(json.dumps(reply), flush=True)
`

// askPython runs every request through one CPython process named by AGENT_DECISIONS_PYTHON and returns
// its reply lines in order. It skips the test when the variable is unset.
func askPython(t *testing.T, requests [][]any) []string {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var input strings.Builder
	for _, request := range requests {
		line, err := stdjson.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		input.Write(line)
		input.WriteByte('\n')
	}
	command := exec.Command(python, "-I", "-c", oracleScript, repoRoot)
	command.Stdin = strings.NewReader(input.String())
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var replies []string
	lines := bufio.NewScanner(strings.NewReader(string(output)))
	lines.Buffer(nil, 1<<30)
	for lines.Scan() {
		replies = append(replies, lines.Text())
	}
	if len(replies) != len(requests) {
		t.Fatalf("python answered %d of %d requests", len(replies), len(requests))
	}
	return replies
}

func wire(s string) string {
	return hex.EncodeToString([]byte(s))
}

// outcome is the oracle's ["ok", result] or [exception class, str(exception)].
func outcome(value any, err error) []any {
	if err == nil {
		return []any{"ok", value}
	}
	if typed, ok := err.(interface{ PyType() string }); ok {
		return []any{typed.PyType(), err.Error()}
	}
	return []any{fmt.Sprintf("Go %T", err), err.Error()}
}

func list(items []string) []any {
	values := make([]any, len(items))
	for i, item := range items {
		values[i] = item
	}
	return values
}

func calls(items []transcript.ToolCall) []any {
	values := make([]any, len(items))
	for i, call := range items {
		var execution any
		if call.Execution != nil {
			execution = call.Execution
		}
		values[i] = []any{call.ID, call.Name, call.Input, call.Output, execution}
	}
	return values
}

func optional(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// facts mirrors the oracle's facts().
func facts(t transcript.Transcript) []string {
	found := []string{"", "yes", strings.Repeat("x", 11), "the user said memory is not a concern"}
	for _, message := range t.UserMessages {
		words := pystr.Split(message)
		found = append(found, message, pystr.Slice(message, 1, -1), pystr.Casefold(message),
			strings.Join(words[:min(3, len(words))], " "))
		if answers, err := transcript.QuestionReplyWords(message); err == nil {
			found = append(found, answers...)
		}
	}
	for _, call := range t.ToolCalls {
		found = append(found, pystr.SliceTo(call.Output, 60))
	}
	if t.Brief != nil {
		found = append(found, *t.Brief)
	}
	return found
}

// summary mirrors the oracle's summary().
func summary(t transcript.Transcript) *json.Object {
	var identity any
	if i := t.Identity; i != nil {
		identity = []any{i.ThreadID, i.SessionID, optional(i.ParentThreadID), optional(i.AgentID), i.Cwd}
	}
	decided := make([]any, len(t.ToolCalls))
	for i, call := range t.ToolCalls {
		decided[i] = outcome(transcript.IsDecide(call))
	}
	words := make([]any, len(t.UserMessages))
	for i, message := range t.UserMessages {
		answers, err := transcript.QuestionReplyWords(message)
		words[i] = outcome(list(answers), err)
	}
	found := facts(t)
	verified, unverified, err := provenance.Verify(found, t)
	return json.NewObject(
		"user_messages", list(t.UserMessages),
		"tool_calls", calls(t.ToolCalls),
		"turn_calls", calls(t.TurnCalls),
		"last_assistant_text", t.LastAssistantText,
		"brief", optional(t.Brief),
		"user_keys", list(t.UserKeys),
		"question_reply_keys", list(t.QuestionReplyKeys),
		"identity", identity,
		"turn_key", t.TurnKey(),
		"is_decide", decided,
		"question_reply_words", words,
		"facts", list(found),
		"verify", outcome([]any{list(verified), list(unverified)}, err),
	)
}

func sha(v any) string {
	digest := sha256.Sum256([]byte(dumps(v, json.Options{})))
	return hex.EncodeToString(digest[:])
}

// digest mirrors the oracle's digest().
func digest(value *json.Object) *json.Object {
	hashed := &json.Object{}
	for _, key := range value.Keys() {
		field := value.Value(key)
		if items, ok := field.([]any); ok {
			hashes := make([]any, len(items))
			for i, item := range items {
				hashes[i] = sha(item)
			}
			hashed.Set(key, hashes)
		} else {
			hashed.Set(key, sha(field))
		}
	}
	return hashed
}

// loadReply returns the Go side of the oracle's reply to ["load", path, host, digest].
func loadReply(path, host string, digested bool) string {
	parsed, err := transcript.Load(path, host)
	if err != nil {
		reply := outcome(nil, err)
		if digested {
			reply[1] = ""
		}
		return dumps(reply, json.Options{})
	}
	if digested {
		return dumps([]any{"ok", digest(summary(parsed))}, json.Options{})
	}
	return dumps([]any{"ok", summary(parsed)}, json.Options{})
}

// line returns v as one JSON line.
func line(v any) string {
	return dumps(v, json.Options{})
}

func codexMessage(role, text string, origin any) string {
	payload := json.NewObject("type", "message", "role", role,
		"content", []any{json.NewObject("type", "input_text", "text", text)})
	if origin != nil {
		payload.Set("origin", origin)
	}
	return line(json.NewObject("type", "response_item", "payload", payload))
}

func codexCall(callID, name, arguments string) string {
	return line(json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "function_call", "name", name, "arguments", arguments, "call_id", callID)))
}

func codexOutput(callID, output string) string {
	return line(json.NewObject("type", "response_item", "payload", json.NewObject(
		"type", "function_call_output", "call_id", callID, "output", output)))
}

func questionReply(items ...[3]string) string {
	var replies []any
	for _, item := range items {
		replies = append(replies, json.NewObject("questionItemId", item[0], "question", item[1], "answer", item[2]))
	}
	return "<send_user_message_question_reply>" + line(replies) + "</send_user_message_question_reply>"
}

func sessionMeta(payload string) string {
	return `{"type":"session_meta","payload":` + payload + `}`
}

const rootMeta = `{"id":"thread-1","cwd":"/work","source":"cli"}`

// corpus maps a file name to its host and lines. A name ends in .claude or .codex; lines are written as
// given, each with a newline, except a final line that starts with "partial:".
func corpus() map[string][]string {
	human := func(text string) string {
		return line(json.NewObject("type", "user", "message", json.NewObject("role", "user", "content", text),
			"origin", json.NewObject("kind", "human")))
	}
	ask := func(id string, input string) string {
		return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id +
			`","name":"AskUserQuestion","input":` + input + `}]}}`
	}
	answer := func(id, source, answers string) string {
		return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
			`","content":"answered"}]},"permissionDecision":{"decision":"accept","source":"` + source +
			`"},"toolUseResult":{"answers":` + answers + `}}`
	}
	queued := func(fields string) string {
		return `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"prompt",` + fields + `}}`
	}
	worker := func(spawn string) string {
		return sessionMeta(`{"id":"worker","cwd":"/work","source":{"subagent":{"thread_spawn":` + spawn + `}}}`)
	}
	asks := `{"questions":[{"title":"Pick a cache"},{"question":"Why?"},{"title":null,"question":"shadowed"}]}`
	return map[string][]string{
		"basic.claude": {
			`{"type":"user","message":{"role":"user","content":"first ask"},"origin":{"kind":"human"},"timestamp":"2026-10-14T00:00:00Z"}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls","description":"List \u00e9 files \ud83d\ude00","n":1e16,"z":-0.0,"big":123456789012345678901234567890,"f":1.5,"nan":NaN,"inf":-Infinity}}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"}]}}`,
			"not json at all", "[1,2,3]", `"a string"`, "5", "", "   ",
			`{"type":"user","message":{"role":"user","content":"peer note"},"origin":{"kind":"peer"}}`,
			`{"type":"user","isCompactSummary":true,"origin":{"kind":"human"},"message":{"role":"user","content":"summary"}}`,
			`{"type":"user","message":{"role":"user","content":"first ask"},"origin":{"kind":"human"},"timestamp":"2026-10-14T00:00:00Z"}`,
			`{"type":"user","message":{"role":"user","content":"Keep \ud800 the cache, \udc00 and \ud83d\ude00."},"origin":{"kind":"human"}}`,
			`{"type":"user","message":{"role":"user","content":"Keep \ud800 the key"},"origin":{"kind":"human"},"k\udfff":"v"}`,
			"{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"bad bytes \xff\xfe and \xe2\x82 end \xed\xa0\x80\"},\"origin\":{\"kind\":\"human\"}}",
			"\xef\xbb\xbf" + `{"type":"user","message":{"role":"user","content":"bom line"},"origin":{"kind":"human"}}`,
			`{"type":"user","message":{"role":"user","content":"big int ` + strings.Repeat("9", 4301) + `"},"origin":{"kind":"human"},"n":` + strings.Repeat("9", 4301) + `}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Read"},{"type":"text","text":""},"junk",{"type":"text","text":"done"}]}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":"dup"}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"line 1"},{"type":"image","source":{}},{"type":"output_text","text":"line 2"}]}]}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_3","name":"Write","input":"raw string input"},{"type":"tool_use","name":"NoID","input":{}}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":null},{"type":"tool_result","content":{"k":"\u00e9","a":[1,2.5,null,true]}},{"type":"tool_result","tool_use_id":7,"content":"int id dropped"}]}}`,
			`{"type":"user","message":{"role":"user","content":"<bash-stdout>echo</bash-stdout>"}}`,
			`{"type":"user","message":{"role":"user","content":"headless ask"}}`,
			`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta text"}}`,
			`{"type":"user","message":"","origin":{"kind":"human"}}`,
			`{"type":"summary","message":null}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"list text one"},{"type":"input_text","text":"two"},{"type":"tool_reference"}]},"origin":{"kind":"human"}}`,
			`{"type":"user","message":{"role":"user","content":"origin is an empty string"},"origin":""}`,
			`{"type":"user","message":{"role":"user","content":"origin is null"},"origin":null}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"final words"},{"type":"text","text":0}]}}`,
			`partial:{"type":"user","mess`,
		},
		"answers.claude": {
			human("pick one"),
			ask("ask1", `{"questions":[{"question":"Which cache?"},{"question":"Which langs?","multiSelect":true},{"question":"Notes?"},{"question":"Empty?"}]}`),
			answer("ask1", "user_temporary", `{"Which cache?":"LFU","Which langs?":["Go","Python"],"Notes?":"   ","Empty?":[]}`),
			ask("ask2", `{"questions":[{"question":"Hook?"}]}`),
			answer("ask2", "hook", `{"Hook?":"filled by hook"}`),
			ask("ask3", `{"questions":[{"question":"Real?"}]}`),
			answer("ask3", "user_temporary", `{"Real?":"yes really","Invented?":"no"}`),
			ask("ask4", `"not json"`),
			answer("ask4", "user_temporary", `{"Q":"A"}`),
			ask("ask5", `{"questions":[{"question":["unhashable"]},{"question":"Q5"}]}`),
			answer("ask5", "user_temporary", `{"Q5":"A5"}`),
			ask("ask6", `{"questions":[{"question":"Q6"},{"question":"Q6b"}]}`),
			answer("ask6", "user_temporary", `{"Q6":["a",1],"Q6b":5}`),
			ask("ask7", `{"questions":"Q7"}`),
			answer("ask7", "user_temporary", `{}`),
			ask("ask8", `{"questions":[{"question":"Q8"},"str item"]}`),
			answer("ask8", "user_temporary", `{"Q8":"A8"}`),
			ask("ask9", `{"questions":[{"question":"Q9"}],"extra":1}`),
			answer("ask9", "user_temporary", `{"Q9":"A9 \u00e9 spaced  out"}`),
			answer("missing", "user_temporary", `{"Q9":"no such call"}`),
			queued(`"prompt":"typed while working","humanTurn":true,"origin":{"kind":"human"}`),
			queued(`"prompt":"peer queued","humanTurn":true,"origin":{"kind":"peer"}`),
			queued(`"prompt":"no human turn","origin":{"kind":"human"}`),
			queued(`"prompt":"turn is 1","humanTurn":1,"origin":{"kind":"human"}`),
			`{"type":"attachment","isSidechain":true,"attachment":{"type":"queued_command","commandMode":"prompt","prompt":"sidechain","humanTurn":true,"origin":{"kind":"human"}}}`,
			queued(`"prompt":[{"type":"text","text":"list prompt"}],"humanTurn":true,"origin":{"kind":"human"}`),
			`{"type":"attachment","attachment":{"type":"queued_command","commandMode":"bash","prompt":"ls","humanTurn":true,"origin":{"kind":"human"}}}`,
			`{"type":"attachment","attachment":null}`,
			`{"type":"attachment","attachment":{"type":"other"}}`,
		},
		"sidechain.claude": {
			`{"type":"user","isSidechain":true,"agentId":"a1","message":{"role":"user","content":"Find why tiles reload"}}`,
			`{"type":"user","isSidechain":true,"agentId":"a1","message":{"role":"user","content":"second brief"}}`,
			`{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_3","name":"Grep","input":{"pattern":"reload"}}]}}`,
			`{"type":"user","isSidechain":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"cache.py:10"}]}}`,
		},
		"empty.claude":            {},
		"junk.claude":             {"nope", "[]", `{"type":"assistant","message":{"content":"not a list"}}`},
		"message-str.claude":      {human("ok"), `{"type":"user","message":"string message"}`},
		"origin-list.claude":      {`{"type":"user","message":{"content":"x"},"origin":["human"]}`},
		"id-list.claude":          {`{"type":"assistant","message":{"content":[{"type":"tool_use","id":[1],"name":"Bash"}]}}`},
		"result-id-dict.claude":   {`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":{"a":1},"content":"x"}]}}`},
		"text-int.claude":         {`{"type":"user","message":{"content":[{"type":"text","text":5}]},"origin":{"kind":"human"}}`},
		"text-null.claude":        {`{"type":"user","message":{"content":[{"type":"text","text":"a"},{"type":"text"},{"type":"text","text":null}]}}`},
		"attachment-str.claude":   {`{"type":"attachment","attachment":"queued"}`},
		"queued-origin.claude":    {queued(`"prompt":"x","humanTurn":true,"origin":"human"`)},
		"result-content.claude":   {`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":[1]}]}]}}`},
		"message-number.claude":   {`{"type":"summary","message":5}`},
		"assistant-list.claude":   {`{"type":"assistant","message":[1]}`},
		"sidechain-attach.claude": {`{"type":"attachment","isSidechain":true,"attachment":"str"}`, human("still read")},
		"basic.codex": {
			sessionMeta(`{"id":"thread-1","cwd":"/work","source":"cli","originator":"codex"}`),
			codexMessage("user", "# AGENTS.md instructions for /x\n...", nil),
			codexMessage("user", "<environment_context>cwd</environment_context>", nil),
			codexMessage("user", " \n<user_instructions>x", nil),
			codexMessage("user", "fix the build", nil),
			codexCall("call_1", "exec_command", `{"cmd": "make"}`),
			codexOutput("call_1", "error: missing header"),
			`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch"}}`,
			`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"Success"}}`,
			`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"outer","name":"exec","input":"tools.exec_command()"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"patched"}]}}`,
			`{"type":"event_msg","payload":{"type":"token_count"}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-1","command":["/bin/zsh","-lc","printf '%s\\n' 'ts-decide verdict: ADVISORY'"],"aggregated_output":"ts-decide verdict: ADVISORY","cwd":"/work","exit_code":0,"process_id":"123","status":"completed","extra":1}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-2","command":"cat /s/verdicts/x.txt"}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-3","command":["a",0,null,"b c",false,[],{}]}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-4","command":["a",5],"status":null}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-5","command":["bash","-c"],"aggregated_output":{"text":"x"}}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","id":"mcp-1","server":5,"tool":null,"arguments":{"u":"\u00e9","z":[1,{"b":2,"a":1}]},"result":{"content":[{"type":"text","text":"docs"}]}}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","id":"mcp-2","server":["a",1.0,{"k":null,"q":"it's"},true],"tool":1e20,"result":{"isError":true}}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","id":"mcp-3","server":"s","tool":NaN,"result":"plain"}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","id":"mcp-4","server":-Infinity,"tool":1e-7,"result":[]}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"fc-1","changes":{"a.go":{"add":"x"}},"stdout":"ok"}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"fc-2","stdout":true,"stderr":{"e":[1,"two\n",-0.0]}}}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","item":null}}`,
			`{"type":"event_msg","payload":{"type":"item_completed"}}`,
			`{"type":"event_msg","payload":null}`,
			`{"type":"event_msg","payload":{"type":"other","item":"not checked"}}`,
			`{"type":"response_item","payload":{"type":"function_call","call_id":"call_1","name":"dup","arguments":"x"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":{"content":[{"type":"output_text","text":"replaced"}]}}}`,
			`{"type":"response_item","payload":{"type":"function_call","call_id":"call_9","name":"shell","arguments":{"command":["bash","-lc","ts-decide x"]}}}`,
			`{"type":"response_item","payload":{"type":"function_call","name":"no id"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","output":42}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":"string content user"}}`,
			codexMessage("user", "agent said", json.NewObject("kind", "agent")),
			codexMessage("user", "origin without kind", &json.Object{}),
			codexMessage("user", "origin is false", false),
			codexMessage("user", `<hook_prompt hook_run_id="x">feedback</hook_prompt>`, nil),
			codexMessage("user", "\t<hook_prompt>feedback</hook_prompt>", nil),
			codexMessage("user", "<codex_delegation>delegated brief</codex_delegation>", nil),
			codexMessage("user", "<codex_delegation>second</codex_delegation>", nil),
			codexMessage("developer", "dev text", nil),
			codexMessage("user", "", nil),
			`{"type":"response_item","payload":{"type":"reasoning","summary":[]}}`,
			`{"type":"response_item","payload":null}`,
			`{"type":"response_item"}`,
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"},{"type":"input_text","text":"two"}]}}`,
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`,
		},
		"questions.codex": {
			sessionMeta(rootMeta),
			codexMessage("user", "ask me", nil),
			codexCall("q1", "request_user_input", asks),
			codexOutput("q1", "answered"),
			codexCall("q2", "request_user_input", asks),
			codexCall("q3", "request_user_input_async", "not json"),
			codexOutput("q3", "answered"),
			codexCall("q4", "AskUserQuestion", `{"questions":["str item",{"title":"T4"}]}`),
			codexOutput("q4", "answered"),
			codexCall("q5", "request_user_input", `{"questions":{"0":{"title":"dict"}}}`),
			codexOutput("q5", "answered"),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",0]`, "Pick a cache", "LFU please"},
				[3]string{`["request_user_input","q1",1]`, "Why?", "memory  matters"}), nil),
			codexMessage("user", "  \n"+questionReply([3]string{`["request_user_input","q1",0]`, "Pick a cache", "padded"})+" \n", nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",0]`, "Wrong title", "no"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",3]`, "Pick a cache", "out of range"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",-1]`, "shadowed", "negative"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",true]`, "Why?", "bool index"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",0.0]`, "Pick a cache", "float index"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",99999999999999999999999]`, "x", "huge"}), nil),
			codexMessage("user", questionReply([3]string{`["AskUserQuestion","q1",0]`, "Pick a cache", "wrong name"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q2",0]`, "Pick a cache", "no output"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",2]`, "shadowed", "null title"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input_async","q3",0]`, "x", "bad request"}), nil),
			codexMessage("user", questionReply([3]string{`["AskUserQuestion","q4",0]`, "x", "str question"}), nil),
			codexMessage("user", questionReply([3]string{`["AskUserQuestion","q4",1]`, "T4", "second item"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q5",0]`, "dict", "dict questions"}), nil),
			codexMessage("user", questionReply([3]string{`not json`, "x", "bad id"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1"]`, "x", "two parts"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input",1,0]`, "x", "int call"}), nil),
			codexMessage("user", questionReply([3]string{`["request_user_input","q1",0]`, "Pick a cache", "agent origin"}), json.NewObject("kind", "agent")),
			codexMessage("user", "<send_user_message_question_reply>[not json]</send_user_message_question_reply>", nil),
			codexMessage("user", "<send_user_message_question_reply>[]</send_user_message_question_reply>", nil),
			codexMessage("user", "<send_user_message_question_reply>[{\"questionItemId\":1}]</send_user_message_question_reply>", nil),
			codexMessage("user", "<send_user_message_question_reply>[{}] unclosed", nil),
			codexMessage("user", "<send_user_message_question_reply_extra>x</send_user_message_question_reply>", nil),
			codexMessage("user", "thanks", nil),
		},
		"worker.codex": {
			codexMessage("user", "Complete the assigned parser repair.", json.NewObject("kind", "agent")),
			worker(`{"parent_thread_id":"parent","agent_path":"/root/worker","depth":1}`),
			worker(`{"depth":1,"agent_path":"/root/worker","parent_thread_id":"parent"}`),
			codexMessage("user", "later agent note", json.NewObject("kind", "agent")),
			codexMessage("user", "no origin note", nil),
			codexMessage("user", "human words", json.NewObject("kind", "human")),
			codexMessage("user", "<environment_context>x</environment_context>", json.NewObject("kind", "agent")),
			codexMessage("user", "<hook_prompt>x</hook_prompt>", json.NewObject("kind", "agent")),
			codexCall("w1", "exec_command", `{"cmd":"pwd"}`),
		},
		"identity-only.codex":   {sessionMeta(`{"id":"t","session_id":"s","cwd":"/w","source":"exec"}`)},
		"no-source.codex":       {sessionMeta(`{"id":"t","cwd":"/w"}`), sessionMeta(`{"id":"other"}`), codexMessage("user", "old format", nil)},
		"meta-not-dict.codex":   {sessionMeta(`"str"`), sessionMeta(rootMeta), codexMessage("user", "x", nil)},
		"no-meta-payload.codex": {`{"type":"session_meta"}`, codexMessage("user", "x", nil)},
		"conflict.codex":        {sessionMeta(rootMeta), codexMessage("user", "x", nil), sessionMeta(`{"id":"thread-1","cwd":"/other","source":"cli"}`)},
		"conflict-early.codex":  {sessionMeta(rootMeta), sessionMeta(`{"id":"thread-1","cwd":"/work","source":"cli","x":1}`), `{"type":"event_msg","payload":"bad"}`},
		"same-meta.codex":       {sessionMeta(`{"id":"thread-1","cwd":"/work","source":"cli","n":1}`), sessionMeta(`{"source":"cli","n":1.0,"cwd":"/work","id":"thread-1"}`), codexMessage("user", "x", nil)},
		"bad-id.codex":          {sessionMeta(`{"id":"bad id!","cwd":"/w","source":"cli"}`)},
		"long-id.codex":         {sessionMeta(`{"id":"` + strings.Repeat("a", 129) + `","cwd":"/w","source":"cli"}`)},
		"id-int.codex":          {sessionMeta(`{"id":5,"cwd":"/w","source":"cli"}`)},
		"bad-session.codex":     {sessionMeta(`{"id":"t","session_id":null,"cwd":"/w","source":"cli"}`)},
		"no-cwd.codex":          {sessionMeta(`{"id":"t","source":"cli"}`)},
		"empty-cwd.codex":       {sessionMeta(`{"id":"t","cwd":"","source":"cli"}`)},
		"source-dict.codex":     {sessionMeta(`{"id":"t","cwd":"/w","source":{"vscode":{}}}`)},
		"source-int.codex":      {sessionMeta(`{"id":"t","cwd":"/w","source":5}`)},
		"source-null.codex":     {sessionMeta(`{"id":"t","cwd":"/w","source":null}`)},
		"spawn-list.codex":      {sessionMeta(`{"id":"t","cwd":"/w","source":{"subagent":{"thread_spawn":[]}}}`)},
		"self-parent.codex":     {worker(`{"parent_thread_id":"worker","agent_path":"/root/a","depth":1}`)},
		"bad-parent.codex":      {worker(`{"parent_thread_id":"p p","agent_path":"/root/a","depth":1}`)},
		"agent-int.codex":       {worker(`{"parent_thread_id":"p","agent_path":5,"depth":1}`)},
		"short-path.codex":      {worker(`{"parent_thread_id":"p","agent_path":"/root","depth":0}`)},
		"wrong-root.codex":      {worker(`{"parent_thread_id":"p","agent_path":"/x/a","depth":1}`)},
		"bool-depth.codex":      {worker(`{"parent_thread_id":"p","agent_path":"/root/a","depth":true}`)},
		"float-depth.codex":     {worker(`{"parent_thread_id":"p","agent_path":"/root/a","depth":1.0}`)},
		"wrong-depth.codex":     {worker(`{"parent_thread_id":"p","agent_path":"/root/a/b","depth":1}`)},
		"deep.codex":            {worker(`{"parent_thread_id":"p","agent_path":"/root` + strings.Repeat("/a", 33) + `","depth":33}`)},
		"max-depth.codex":       {worker(`{"parent_thread_id":"p","agent_path":"/root` + strings.Repeat("/a", 32) + `","depth":32}`)},
		"bad-part.codex":        {worker(`{"parent_thread_id":"p","agent_path":"/root/a b","depth":1}`)},
		"empty-part.codex":      {worker(`{"parent_thread_id":"p","agent_path":"/root//a","depth":2}`)},
		"long-part.codex":       {worker(`{"parent_thread_id":"p","agent_path":"/root/` + strings.Repeat("a", 81) + `","depth":1}`)},
		"payload-str.codex":     {`{"type":"response_item","payload":"x"}`},
		"event-payload.codex":   {`{"type":"event_msg","payload":[1]}`},
		"item-str.codex":        {`{"type":"event_msg","payload":{"type":"item_completed","item":"x"}}`},
		"item-unchecked.codex":  {`{"type":"event_msg","payload":{"type":"token_count","item":"x"}}`, codexMessage("user", "x", nil)},
		"origin-list.codex":     {codexMessage("user", "x", []any{"human"})},
		"origin-injected.codex": {codexMessage("user", "<environment_context>", []any{"human"}), codexMessage("user", "kept", nil)},
		"worker-origin.codex":   {worker(`{"parent_thread_id":"p","agent_path":"/root/a","depth":1}`), codexMessage("user", "<environment_context>", "agent")},
		"reply-origin.codex":    {codexMessage("user", "<send_user_message_question_reply>", 5)},
		"content-int.codex":     {`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":1}]}}`},
		"call-id-list.codex":    {`{"type":"response_item","payload":{"type":"function_call","call_id":["x"],"name":"n","arguments":""}}`},
		"output-id-dict.codex":  {`{"type":"response_item","payload":{"type":"function_call_output","call_id":{},"output":"x"}}`},
		"event-id-list.codex":   {`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":[],"stdout":"x"}}}`},
	}
}

func writeCorpus(t *testing.T) (dir string, names []string) {
	dir = t.TempDir()
	for name, lines := range corpus() {
		var data strings.Builder
		for _, text := range lines {
			if partial, ok := strings.CutPrefix(text, "partial:"); ok {
				data.WriteString(partial)
			} else {
				data.WriteString(text + "\n")
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return dir, names
}

func hostOf(name string) string {
	return strings.TrimPrefix(filepath.Ext(name), ".")
}

// TestLoadMatchesPython loads every corpus transcript in Python and Go and compares the full summary,
// including is_decide, question_reply_words and provenance.verify, byte for byte.
func TestLoadMatchesPython(t *testing.T) {
	dir, names := writeCorpus(t)
	type load struct{ path, host string }
	loads := []load{
		{filepath.Join(dir, "missing.jsonl"), "claude"},
		{filepath.Join(dir, "missing.jsonl"), "gemini"},
		{dir, "claude"},
		{"", "codex"},
		{dir + "/x\x00y", "claude"},
		{filepath.Join(dir, "basic.claude"), "gemini"},
		{filepath.Join(dir, "basic.claude/child"), "claude"},
	}
	locked := filepath.Join(dir, "locked.jsonl")
	if err := os.WriteFile(locked, nil, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		loads = append(loads, load{locked, "claude"})
	}
	for _, name := range names {
		loads = append(loads, load{filepath.Join(dir, name), hostOf(name)}, load{filepath.Join(dir, name), map[string]string{"claude": "codex", "codex": "claude"}[hostOf(name)]})
	}
	var requests [][]any
	for _, l := range loads {
		requests = append(requests, []any{"load", wire(l.path), l.host, false})
	}
	for i, reply := range askPython(t, requests) {
		if got := loadReply(loads[i].path, loads[i].host, false); got != reply {
			t.Errorf("Load(%s, %s):\n go     %s\n python %s", filepath.Base(loads[i].path), loads[i].host, got, reply)
		}
	}
}

// TestIsDecideMatchesPython compares IsDecide on odd tool inputs.
func TestIsDecideMatchesPython(t *testing.T) {
	inputs := []string{
		`{"command": "ts-decide '{}'"}`, `{"command": "cat /s/verdicts/b.txt"}`, `{"command": "ls"}`,
		`{"cmd": "'/plugin with spaces/bin/ts-decide' <<'EOF'\n{}\nEOF"}`, `{"cmd": "/plugin/bin/ts-decide 'unterminated"}`,
		`{"cmd": "printf '%s\\n' 'ts-decide verdict: ADVISORY'"}`, `{"cmd": "printf '%s\\n' 'ts-decide: decision unverified (timeout)'"}`,
		`{"cmd": "printf '%s\\n' 'ts-decide verdict: ADVISORY' extra"}`, `{"cmd": "printf %s\\\\n 'ts-decide verdict: X'"}`,
		`{"cmd": "printf '%s\\n' \"ts-decide verdict:\""}`, `{"cmd": "printf '%s' 'ts-decide verdict: X'"}`,
		`{"cmd": "cat '/state with spaces/verdicts/receipt.txt'"}`, `{"cmd": "cat /state/verdicts/receipt.txt extra"}`,
		`{"cmd": "cat /state/verdicts/receipt.txt 'unterminated"}`, `{"cmd": "python3 - <<'PY'\nprint('ts-decide verdict: ADVISORY')\nPY"}`,
		`{"cmd": "grep source.py # ts-decide"}`, `{"cmd": ""}`, `{"cmd": "   "}`, `{"cmd": "'' ts-decide"}`,
		`{"command": ["bash", "-lc", "ts-decide x"]}`, `{"command": ["bash", "-c", "ls"]}`, `{"command": ["bash", "-c"]}`,
		`{"command": ["bash", "-x", "ts-decide"]}`, `{"command": ["bash", 1, "ts-decide"]}`, `{"command": ["bash", "-c", 5]}`,
		`{"cmd": ["ts-decide", "a b"]}`, `{"command": []}`, `{"command": null}`, `{"command": 5}`, `{"command": {"a": 1}}`,
		`{"command": ["a", 5]}`, `{"command": ["a", 0, false, null, [], {}, ""]}`, `{"command": ["x", true]}`,
		`{"command": ["x", 1.5]}`, `{"command": ["x", [1]]}`, `{"command": ["x", {"a": 1}]}`, `{"command": ["x", -0.0]}`,
		`{"command": "a/./ts-decide"}`, `{"command": "ts-decide/"}`, `{"command": "//ts-decide"}`, `{"command": "./ts-decide"}`,
		`{"command": "ts-decide."}`, `{"command": "/"}`, `{"command": "."}`, `{"command": "cat verdicts/x"}`,
		`{"command": "cat verdicts/"}`, `{"command": "cat /verdicts"}`, `{"command": "cat a/verdicts/./x"}`,
		`{"command": "cat a/verdicts/../x"}`, `{"command": "cat ./verdicts//x"}`, `{"command": "cat x/verdicts/x/"}`,
		`{"command": "cat verdicts/."}`, `{"command": "cat ''"}`, `{"command": "cat"}`,
		`{"command": "ts-decide", "cmd": "ls"}`, `{"cmd": "ts-decide"}`, `{}`, `5`, `null`, `true`, `"\"ts-decide\""`,
		`"ts-decide"`, `["ts-decide", "x"]`, `["printf", "%s\\n", "ts-decide verdict: A"]`, `[]`,
		"ts-decide run", "'unterminated", "ts-decide\u00a0x", "ts-decide\tx\\", "cat \"a/verdicts/b\" ", "{bad json",
		`{"command": "ts-decide \ud800"}`, "ts-decide \xed\xa0\x80", `NaN`, `{"command": "x"} trailing`,
		"\xef\xbb\xbf\"ts-decide\"", strings.Repeat("9", 4301),
		strings.Repeat("[", 200_000) + strings.Repeat("]", 200_000),
	}
	var requests [][]any
	for _, input := range inputs {
		requests = append(requests, []any{"is_decide", wire(input)})
	}
	for i, reply := range askPython(t, requests) {
		got := line(outcome(transcript.IsDecide(transcript.ToolCall{ID: "c", Name: "Bash", Input: inputs[i]})))
		if got != reply {
			t.Errorf("IsDecide(%.200q): go %s, python %s", inputs[i], got, reply)
		}
	}
}

// TestQuestionReplyWordsMatchesPython compares QuestionReplyWords on odd envelopes.
func TestQuestionReplyWordsMatchesPython(t *testing.T) {
	texts := []string{
		"", "plain", questionReply([3]string{"a", "b", "c"}), " \u3000" + questionReply([3]string{"a", "b", "c"}) + "\x1c",
		questionReply([3]string{"a", "b", ""}, [3]string{"x", "y", "two"}),
		"<send_user_message_question_reply></send_user_message_question_reply>",
		"<send_user_message_question_reply>[]</send_user_message_question_reply>",
		"<send_user_message_question_reply>[{\"questionItemId\":\"a\",\"question\":\"b\"}]</send_user_message_question_reply>",
		"<send_user_message_question_reply>[{\"questionItemId\":\"a\",\"question\":\"b\",\"answer\":null}]</send_user_message_question_reply>",
		"<send_user_message_question_reply>[{\"questionItemId\":\"a\",\"question\":\"b\",\"answer\":\"c\"},5]</send_user_message_question_reply>",
		"<send_user_message_question_reply>{\"questionItemId\":\"a\"}</send_user_message_question_reply>",
		"<send_user_message_question_reply>[{\"questionItemId\":\"a\",\"question\":\"b\",\"answer\":\"\\ud800\"}]</send_user_message_question_reply>",
		"<send_user_message_question_reply>" + strings.Repeat("[", 200_000) + "</send_user_message_question_reply>",
		"x" + questionReply([3]string{"a", "b", "c"}),
	}
	var requests [][]any
	for _, text := range texts {
		requests = append(requests, []any{"reply_words", wire(text)})
	}
	for i, reply := range askPython(t, requests) {
		words, err := transcript.QuestionReplyWords(texts[i])
		if got := line(outcome(list(words), err)); got != reply {
			t.Errorf("QuestionReplyWords(%.200q): go %s, python %s", texts[i], got, reply)
		}
	}
}

// TestVerifyMatchesPython compares provenance.Verify on case folding, Unicode whitespace and message
// boundaries.
func TestVerifyMatchesPython(t *testing.T) {
	cases := []struct{ facts, users []string }{
		{[]string{"STRASSE is closed now", "keep memory under 2 gb", "short"}, []string{"Die Straße is closed now", "Keep\u3000memory\x1cunder\u00a02\u2028GB"}},
		{[]string{"first message\x00second", "message second message"}, []string{"first message", "second message"}},
		{[]string{"answer number one", "agent's question text"}, []string{questionReply([3]string{"i", "agent's question text", "Answer   number one"})}},
		{[]string{"twelve chars", "elevenchars", "İstanbul is far away", "ﬃ ligature words"}, []string{"TWELVE CHARS here", "i̇stanbul is far away", "FFI LIGATURE WORDS"}},
		{[]string{"ΣΑΣ final sigma", "σας final sigma"}, []string{"σας FINAL SIGMA"}},
		{[]string{"\xed\xa0\x80 lone surrogate fact"}, []string{"\xed\xa0\x80 lone surrogate fact"}},
		{nil, nil},
	}
	var requests [][]any
	for _, c := range cases {
		facts, users := []any{}, []any{}
		for _, fact := range c.facts {
			facts = append(facts, wire(fact))
		}
		for _, user := range c.users {
			users = append(users, wire(user))
		}
		requests = append(requests, []any{"verify", facts, users})
	}
	for i, reply := range askPython(t, requests) {
		verified, unverified, err := provenance.Verify(cases[i].facts, transcript.Transcript{UserMessages: cases[i].users})
		if got := line(outcome([]any{list(verified), list(unverified)}, err)); got != reply {
			t.Errorf("Verify(%q, %q): go %s, python %s", cases[i].facts, cases[i].users, got, reply)
		}
	}
}

// TestCodeModeVerdictEventBacksConsultationAndIsExcludedFromEvidence ports the transcript and provenance
// parts of the Python test; gates.consulted and evidence.build belong to other packages.
//
// py: tests/test_transcript.py::CodexTranscriptTest::test_code_mode_verdict_event_backs_consultation_and_is_excluded_from_evidence
func TestCodeModeVerdictEventBacksConsultationAndIsExcludedFromEvidence(t *testing.T) {
	// ledger.verdict_name("exec-observed") is "exec-observed.txt": every character is already safe.
	command := "cat '/state with spaces/verdicts/exec-observed.txt'"
	lines := []string{
		codexMessage("user", "Fix the parser.", nil),
		`{"type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_outer","status":"completed","call_id":"outer","name":"exec","input":"text(await tools.exec_command({cmd:\"ts-decide fabricated\"}));"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"outer","output":"fabricated consultation fact"}}`,
		line(json.NewObject("type", "event_msg", "payload", json.NewObject("type", "item_completed", "item", json.NewObject(
			"type", "CommandExecution", "id", "exec-observed", "command", []any{"/bin/zsh", "-c", command},
			"aggregated_output", "ts-decide verdict: ADVISORY\nfabricated consultation fact", "status", "completed")))),
	}
	path := filepath.Join(t.TempDir(), "code.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := transcript.Load(path, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.ToolCalls) != 1 || parsed.ToolCalls[0].ID != "exec-observed" {
		t.Fatalf("tool calls %v", parsed.ToolCalls)
	}
	if decided, err := transcript.IsDecide(parsed.ToolCalls[0]); err != nil || !decided {
		t.Errorf("IsDecide = %v, %v", decided, err)
	}
	if verified, _, err := provenance.Verify([]string{"fabricated consultation fact"}, parsed); err != nil || len(verified) != 0 {
		t.Errorf("verified %q, %v", verified, err)
	}
}

// newest returns the count newest *.jsonl files under the directories that match pattern.
func newest(t *testing.T, pattern string, count int) []string {
	roots, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	type file struct {
		path  string
		mtime int64
	}
	var files []file
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
				files = append(files, file{path, info.ModTime().UnixNano()})
			}
			return nil
		})
	}
	slices.SortFunc(files, func(a, b file) int { return cmp.Compare(b.mtime, a.mtime) })
	var paths []string
	for _, f := range files[:min(count, len(files))] {
		paths = append(paths, f.path)
	}
	return paths
}

// TestRealTranscriptsMatchPython compares digests of every summary field for the newest 20 local Claude
// and the newest 20 local Codex transcripts. Each file is copied first, so a live session cannot grow
// between the two reads. Failures name field indexes and counts, never content.
func TestRealTranscriptsMatchPython(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_REAL_TRANSCRIPTS") != "1" {
		t.Skip("set AGENT_DECISIONS_REAL_TRANSCRIPTS=1 to compare local transcripts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	type load struct{ path, host string }
	var loads []load
	snapshots := t.TempDir()
	for host, pattern := range map[string]string{
		"claude": filepath.Join(home, ".claude*", "projects"),
		"codex":  filepath.Join(home, ".codex", "sessions"),
	} {
		for _, source := range newest(t, pattern, 20) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatalf("read transcript %d: %v", len(loads), err)
			}
			copied := filepath.Join(snapshots, fmt.Sprintf("%02d.jsonl", len(loads)))
			if err := os.WriteFile(copied, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loads = append(loads, load{copied, host})
		}
	}
	var requests [][]any
	for _, l := range loads {
		requests = append(requests, []any{"load", wire(l.path), l.host, true})
	}
	replies := askPython(t, requests)
	for i, reply := range replies {
		data, _ := os.ReadFile(loads[i].path)
		lines := strings.Count(string(data), "\n")
		got := loadReply(loads[i].path, loads[i].host, true)
		if got == reply {
			t.Logf("transcript %02d (%s, %d lines): match", i, loads[i].host, lines)
			continue
		}
		t.Errorf("transcript %02d (%s, %d lines): %s", i, loads[i].host, lines, difference(got, reply))
	}
}

// difference describes where two digest replies differ, with field names, indexes and counts only.
func difference(got, want string) string {
	parse := func(text string) []any {
		value, err := json.Loads(text)
		reply, _ := value.([]any)
		if err != nil || len(reply) != 2 {
			return []any{"unparsed", ""}
		}
		return reply
	}
	g, w := parse(got), parse(want)
	if !same(g[0], w[0]) {
		return fmt.Sprintf("outcome go %v, python %v", g[0], w[0])
	}
	gotFields, ok1 := g[1].(*json.Object)
	wantFields, ok2 := w[1].(*json.Object)
	if !ok1 || !ok2 {
		return "error messages differ"
	}
	var notes []string
	for _, key := range wantFields.Keys() {
		gotField, wantField := gotFields.Value(key), wantFields.Value(key)
		if same(gotField, wantField) {
			continue
		}
		gotItems, _ := gotField.([]any)
		wantItems, isList := wantField.([]any)
		if !isList {
			notes = append(notes, key)
			continue
		}
		first := 0
		for first < min(len(gotItems), len(wantItems)) && same(gotItems[first], wantItems[first]) {
			first++
		}
		notes = append(notes, fmt.Sprintf("%s[%d] (go %d items, python %d)", key, first, len(gotItems), len(wantItems)))
	}
	return "fields differ: " + strings.Join(notes, ", ")
}

// dumps returns json.Dumps(v, o) for a test value, which never nests deep enough to fail.
func dumps(v any, o json.Options) string {
	text, err := json.Dumps(v, o)
	if err != nil {
		panic(err)
	}
	return text
}

// same returns json.Equal(a, b) for test values, which never nest deep enough to fail.
func same(a, b any) bool {
	equal, err := json.Equal(a, b)
	if err != nil {
		panic(err)
	}
	return equal
}

// TestSurrogatePassDepthMatchesPython loads lines whose surrogate pass nests around Python's recursion
// limit, with Python's load called as frame 8 of its stack, the depth transcript.loadFrame assumes.
func TestSurrogatePassDepthMatchesPython(t *testing.T) {
	const loadFrame = 8 // transcript.loadFrame
	dir := t.TempDir()
	var requests [][]any
	var paths, hosts []string
	for _, host := range []string{"claude", "codex"} {
		for depth := 984; depth <= 990; depth++ {
			nested := strings.Repeat("[", depth) + `"\ud800"` + strings.Repeat("]", depth)
			record := `{"type":"user","message":{"role":"user","content":"hi"},"x":` + nested + "}"
			if host == "codex" {
				record = `{"type":"response_item","payload":{"type":"message","role":"user",` +
					`"content":[{"type":"input_text","text":"hi"}]},"x":` + nested + "}"
			}
			path := filepath.Join(dir, fmt.Sprintf("%s-%d.jsonl", host, depth))
			if err := os.WriteFile(path, []byte(record+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			requests = append(requests, []any{"load_at", wire(path), host, loadFrame})
			paths, hosts = append(paths, path), append(hosts, host)
		}
	}
	raised := 0
	for i, reply := range askPython(t, requests) {
		_, err := transcript.Load(paths[i], hosts[i])
		got := outcome(nil, err)
		if want := dumps(got, json.Options{}); want != reply {
			t.Errorf("%s: Go %s; Python %s", filepath.Base(paths[i]), want, reply)
		}
		if err != nil {
			raised++
		}
	}
	if raised == 0 || raised == len(requests) {
		t.Errorf("%d of %d loads raised; the depths miss the limit", raised, len(requests))
	}
}
