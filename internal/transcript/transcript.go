// Package transcript reads Claude Code and Codex transcripts into the parts the gates use.
//
// py: agent_decisions/transcript.py
package transcript

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"regexp"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// codexInjected lists the prefixes of the context that Codex records as user messages.
var codexInjected = []string{
	"# AGENTS.md instructions",
	"<environment_context>",
	"<user_instructions>",
	"The following is the Codex agent history",
	"<codex_delegation>",
	"<subagent_notification>",
	"<external_codex_apps_open_page>",
}

// claudeEchoes lists the prefixes of the shell and slash-command output that Claude Code echoes as
// origin-less user records.
var claudeEchoes = []string{"<bash-stdout>", "<bash-stderr>", "<local-command-stdout>", "<local-command-stderr>"}

// IsCodexHookFeedback reports whether text is hook feedback, which Codex records as a user-role message
// without a human origin.
//
// py: agent_decisions/transcript.py is_codex_hook_feedback
func IsCodexHookFeedback(text string) bool {
	return startsWith(pystr.LStrip(text), "<hook_prompt ", "<hook_prompt>")
}

// HostIdentity is the native identity that a Codex transcript records. ParentThreadID and AgentID are nil
// for a root thread.
//
// py: agent_decisions/transcript.py HostIdentity
type HostIdentity struct {
	ThreadID       string
	SessionID      string
	ParentThreadID *string
	AgentID        *string
	Cwd            string
}

// ToolCall is one tool call and its output. Execution holds the native host fields of a Codex command
// (nil for None); they remain observations, not verification.
//
// py: agent_decisions/transcript.py ToolCall
type ToolCall struct {
	ID        string
	Name      string
	Input     string
	Output    string
	Execution *json.Object
}

// Transcript is the part of a host transcript that the gates read. Its slices are shared and must not be
// changed.
//
// py: agent_decisions/transcript.py Transcript
type Transcript struct {
	UserMessages      []string   // the user's own words, oldest first
	ToolCalls         []ToolCall // oldest first
	TurnCalls         []ToolCall // calls after the latest user (or parent) message
	LastAssistantText string
	Brief             *string  // a subagent's task, written by its parent agent; nil for None
	UserKeys          []string // host record identities, including timestamps when present
	QuestionReplyKeys []string // host replies matched to earlier completed questions
	Identity          *HostIdentity
}

// TurnKey identifies the current turn: it changes only with a new user message. Python's str.encode
// raises for a lone surrogate, which Load never returns; this hashes its WTF-8 bytes instead.
//
// py: agent_decisions/transcript.py Transcript.turn_key
func (t Transcript) TurnKey() string {
	latest := ""
	if n := len(t.UserMessages); n > 0 {
		latest = t.UserMessages[n-1]
	} else if t.Brief != nil {
		latest = *t.Brief
	}
	seed := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s", len(t.UserMessages), latest))
	return hex.EncodeToString(seed[:])[:16]
}

// questionReplyItems returns the replies in a question-reply envelope, or nil when text is not a
// well-formed envelope. Each reply has str "questionItemId", "question" and "answer" values.
//
// py: agent_decisions/transcript.py _question_reply_items
func questionReplyItems(text string) ([]*json.Object, error) {
	const prefix, suffix = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
	text = pystr.Strip(text)
	// The prefix and suffix are ASCII and cannot overlap, so byte offsets equal code point offsets here.
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) {
		return nil, nil
	}
	decoded, err := json.Loads(text[len(prefix) : len(text)-len(suffix)])
	if err != nil {
		return nil, notValueError(err)
	}
	list, ok := decoded.([]any)
	if !ok || len(list) == 0 {
		return nil, nil
	}
	replies := make([]*json.Object, len(list))
	for i, item := range list {
		reply, ok := item.(*json.Object)
		if !ok {
			return nil, nil
		}
		for _, key := range []string{"questionItemId", "question", "answer"} {
			if _, ok := reply.Value(key).(string); !ok {
				return nil, nil
			}
		}
		replies[i] = reply
	}
	return replies, nil
}

// QuestionReplyWords returns the decoded answers of a question-reply envelope. Only the answers are human
// words; the envelope also repeats the agent's question.
//
// py: agent_decisions/transcript.py question_reply_words
func QuestionReplyWords(text string) ([]string, error) {
	replies, err := questionReplyItems(text)
	words := make([]string, len(replies))
	for i, reply := range replies {
		words[i] = reply.Value("answer").(string)
	}
	return words, err
}

// codexQuestionReply reports whether text answers questions that completed calls in calls asked.
//
// py: agent_decisions/transcript.py _codex_question_reply
func codexQuestionReply(text string, calls []ToolCall) (bool, error) {
	replies, err := questionReplyItems(text)
	if err != nil || len(replies) == 0 {
		return false, err
	}
	questions := map[string]ToolCall{}
	for _, call := range calls {
		if policy.QuestionTools[call.Name] && call.Output != "" {
			questions[call.ID] = call
		}
	}
	for _, reply := range replies {
		if matched, err := replyAsked(reply, questions); err != nil || !matched {
			return false, err
		}
	}
	return true, nil
}

// replyAsked reports whether reply names a question that a call in questions asked. Each false return
// stands for a Python check that fails or an exception that _codex_question_reply catches.
//
// py: agent_decisions/transcript.py _codex_question_reply (loop body)
func replyAsked(reply *json.Object, questions map[string]ToolCall) (bool, error) {
	identity, err := json.Loads(reply.Value("questionItemId").(string))
	if err != nil {
		return false, notValueError(err)
	}
	parts, ok := identity.([]any)
	if !ok || len(parts) != 3 {
		return false, nil
	}
	callID, isStr := parts[1].(string)
	index, isInt := parts[2].(json.Int)
	if !isStr || !isInt || strings.HasPrefix(string(index), "-") {
		return false, nil
	}
	call, ok := questions[callID]
	if !ok || parts[0] != call.Name {
		return false, nil
	}
	request, err := json.Loads(call.Input)
	if err != nil {
		return false, notValueError(err)
	}
	// request["questions"][index] reaches a dict only through a dict holding a list; every other shape
	// raises TypeError, KeyError, IndexError or (for a str item) AttributeError at .get.
	object, ok := request.(*json.Object)
	if !ok {
		return false, nil
	}
	asked, ok := object.Value("questions").([]any)
	i, fits := index.Int64()
	if !ok || !fits || i >= int64(len(asked)) {
		return false, nil
	}
	question, ok := asked[i].(*json.Object)
	if !ok {
		return false, nil
	}
	title, ok := question.Get("title")
	if !ok {
		title = question.Value("question")
	}
	return json.Equal(reply.Value("question"), title)
}

// newLexer returns the shlex.shlex(command, posix=True) that IsDecide reads. Tests replace it, as the
// Python tests patch shlex.shlex.
var newLexer = func(command string) *shlex.Lexer { return shlex.New(command, true, "") }

// IsDecide reports whether call runs ts-decide or is the hook's literal verdict transport.
//
// py: agent_decisions/transcript.py is_decide
func IsDecide(call ToolCall) (bool, error) {
	toolInput, err := json.Loads(call.Input)
	if err != nil {
		if err := notValueError(err); err != nil {
			return false, err
		}
		toolInput = call.Input
	}
	command := toolInput
	if object, ok := toolInput.(*json.Object); ok {
		command = object.ValueOr("command", object.ValueOr("cmd", ""))
	}
	if list, ok := command.([]any); ok {
		if flag, _ := listItem(list, 1).(string); len(list) == 3 && (flag == "-c" || flag == "-lc") {
			command = list[2]
		} else if command, err = shellJoin(list); err != nil {
			return false, err
		}
	}
	shell, ok := command.(string)
	if !ok {
		return false, nil
	}
	lexer := newLexer(shell)
	lexer.WhitespaceSplit = true
	lexer.Commenters = ""
	first, ok, err := lexer.GetToken()
	if err != nil || !ok {
		return false, nil
	}
	if pathlib.Name(pathlib.Str(first)) != "ts-decide" && first != "printf" && first != "cat" {
		return false, nil // ordinary commands do not need their script bodies tokenized
	}
	rest, err := lexer.Tokens()
	if err != nil {
		return false, nil
	}
	tokens := append([]string{first}, rest...)
	return pathlib.Name(pathlib.Str(first)) == "ts-decide" ||
		len(tokens) == 3 && tokens[0] == "printf" && tokens[1] == `%s\n` &&
			startsWith(tokens[2], "ts-decide verdict:", "ts-decide: decision unverified (") ||
		len(tokens) == 2 && tokens[0] == "cat" && pathlib.Name(pathlib.Parent(pathlib.Str(tokens[1]))) == "verdicts", nil
}

// Load reads the transcript at path that host ("claude" or "codex") wrote.
//
// py: agent_decisions/transcript.py load
func Load(path, host string) (Transcript, error) {
	stream, err := open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer stream.Close()
	// Both Codex passes use the same append-only snapshot.
	end, err := stream.Seek(0, io.SeekEnd)
	if err != nil {
		return Transcript{}, pyerr.NewOSError(err)
	}
	var parsed Transcript
	switch host {
	case "codex":
		identity, err := loadIdentity(records(stream, end, loadFrame+4)) // _codex_identity, its generator
		if err != nil {
			return Transcript{}, err
		}
		if parsed, err = codexRecords(records(stream, end, loadFrame+3), identity); err != nil {
			return Transcript{}, err
		}
	case "claude":
		if parsed, err = claude(records(stream, end, loadFrame+3)); err != nil {
			return Transcript{}, err
		}
	default:
		return Transcript{}, &pyerr.Error{Class: "KeyError", Msg: pystr.Repr(host)}
	}
	if len(parsed.UserMessages) == 0 && len(parsed.ToolCalls) == 0 && parsed.Brief == nil && parsed.Identity == nil {
		return Transcript{}, &pyerr.OSError{Class: "OSError", Msg: "no usable transcript records"}
	}
	return parsed, nil
}

// loadIdentity is the identity pass of Load. Tests replace it, as the Python tests patch _codex_identity.
var loadIdentity = codexIdentity

// loadFrame is the depth of the Python stack at transcript.load when a gate reads a transcript: hooks/hook.py
// <module>, main, handle, _pre_tool_use, _gate or _decide, _context, then _load_transcript or
// _inherited_decisions calls load. The surrogate pass of loads recurses in Python frames, so this depth
// sets the nesting at which a line raises RecursionError (see json.LoadsBytes).
//
// ponytail: one depth for every caller. Python's session_start loads 3 frames shallower, and its Codex
// identity pass reads the records after the first session_meta 1 frame deeper (inside any()); a line
// within that many levels of the limit raises in one and not the other. Pass the depth into Load if the
// hook needs that edge exact.
const loadFrame = 8

// records yields each JSON object line in the first end bytes of stream, from the start. Lines that do
// not parse are skipped: a line still being written, or a damaged one. frames is the Python stack depth at
// each loads call: load's callers, load, the reader of _records, _records itself and loads.
//
// py: agent_decisions/transcript.py _records
func records(stream io.ReadSeeker, end int64, frames int) iter.Seq2[*json.Object, error] {
	return func(yield func(*json.Object, error) bool) {
		if _, err := stream.Seek(0, io.SeekStart); err != nil {
			yield(nil, pyerr.NewOSError(err))
			return
		}
		lines := bufio.NewReader(io.LimitReader(stream, end))
		for {
			line, readErr := lines.ReadBytes('\n')
			if len(line) > 0 {
				record, err := json.LoadsBytes(line, frames)
				if err != nil {
					if err := notValueError(err); err != nil {
						yield(nil, err)
						return
					}
				} else if object, ok := record.(*json.Object); ok && !yield(object, nil) {
					return
				}
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				yield(nil, pyerr.NewOSError(readErr))
				return
			}
		}
	}
}

// text returns the text of a message's content: the str itself, or the text blocks of a list joined by
// newlines.
//
// py: agent_decisions/transcript.py _text
func text(content any) (string, error) {
	switch content := content.(type) {
	case string:
		return content, nil
	case []any:
		var parts []string
		for _, item := range content {
			block, ok := item.(*json.Object)
			if !ok {
				continue
			}
			if kind := block.Value("type"); !isStr(kind, "text") && !isStr(kind, "input_text") && !isStr(kind, "output_text") {
				continue
			}
			part, ok := block.ValueOr("text", "").(string)
			if !ok {
				return "", &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("sequence item %d: expected str instance, %s found",
					len(parts), typeName(block.ValueOr("text", "")))}
			}
			parts = append(parts, part)
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", nil
}

// builder collects a transcript's parts as Load reads its records.
//
// py: agent_decisions/transcript.py _Builder
type builder struct {
	users             []string
	userKeys          []string
	questionReplyKeys []string
	userOccurrences   map[string]int
	calls             []ToolCall // id, name and input only
	callIDs           map[string]bool
	outputs           map[string]string
	executions        map[string]*json.Object
	turnStart         int
	lastText          string
	brief             *string
}

// py: agent_decisions/transcript.py _Builder.__init__
func newBuilder() *builder {
	return &builder{
		userOccurrences: map[string]int{},
		callIDs:         map[string]bool{},
		outputs:         map[string]string{},
		executions:      map[string]*json.Object{},
	}
}

// py: agent_decisions/transcript.py _Builder.user
func (b *builder) user(text string, record *json.Object, questionReply bool) error {
	b.users = append(b.users, text)
	dumped, err := json.Dumps(record, json.Options{SortKeys: true})
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(dumped))
	key := hex.EncodeToString(digest[:])
	b.userOccurrences[key]++
	identity := fmt.Sprintf("%s:%d", key, b.userOccurrences[key])
	b.userKeys = append(b.userKeys, identity)
	if questionReply {
		b.questionReplyKeys = append(b.questionReplyKeys, identity)
	}
	b.turnStart = len(b.calls)
	return nil
}

// py: agent_decisions/transcript.py _Builder.parent
func (b *builder) parent(text string) {
	if b.brief == nil {
		b.brief = &text
	}
	b.turnStart = len(b.calls)
}

// call records a tool call once: classic response items and completed events can describe the same call.
// Python also keeps a call whose id is None, a bool or a number, or whose name is not a str. No host writes
// such calls and ToolCall holds str fields, so they fail here with a TypeError instead.
//
// py: agent_decisions/transcript.py _Builder.call
func (b *builder) call(callID, name, toolInput any) error {
	id, ok := callID.(string)
	if !ok {
		if err := unhashable(callID, "set element"); err != nil {
			return err
		}
		return notStr("tool call id", callID)
	}
	if b.callIDs[id] {
		return nil
	}
	callName, ok := name.(string)
	if !ok {
		return notStr("tool call name", name)
	}
	b.callIDs[id] = true
	input, ok := toolInput.(string)
	if !ok {
		var err error
		if input, err = json.Dumps(toolInput, json.Options{SortKeys: true, AllowUnicode: true}); err != nil {
			return err
		}
	}
	b.calls = append(b.calls, ToolCall{ID: id, Name: callName, Input: input})
	return nil
}

// output records a call's output. Python files an output under a None, bool or number id too, but only a
// str id is ever read back, so this drops it.
//
// py: agent_decisions/transcript.py _Builder.output
func (b *builder) output(callID, output any) error {
	value, ok := output.(string)
	if !ok {
		var err error
		if value, err = text(output); err != nil {
			return err
		}
		if value == "" {
			if value, err = json.Dumps(output, json.Options{AllowUnicode: true}); err != nil {
				return err
			}
		}
	}
	if id, ok := callID.(string); ok {
		b.outputs[id] = value
		return nil
	}
	return unhashable(callID, "dict key")
}

// questions returns the question tool calls read so far, with their outputs.
//
// py: agent_decisions/transcript.py _codex_records (questions tuple)
func (b *builder) questions() []ToolCall {
	var calls []ToolCall
	for _, call := range b.calls {
		if policy.QuestionTools[call.Name] {
			call.Output = b.outputs[call.ID]
			calls = append(calls, call)
		}
	}
	return calls
}

// py: agent_decisions/transcript.py _Builder.build
func (b *builder) build() Transcript {
	calls := make([]ToolCall, len(b.calls))
	for i, call := range b.calls {
		call.Output = b.outputs[call.ID]
		call.Execution = b.executions[call.ID]
		calls[i] = call
	}
	return Transcript{
		UserMessages:      b.users,
		ToolCalls:         calls,
		TurnCalls:         calls[b.turnStart:],
		LastAssistantText: b.lastText,
		Brief:             b.brief,
		UserKeys:          b.userKeys,
		QuestionReplyKeys: b.questionReplyKeys,
	}
}

// claudeQueuedPrompt returns the text the user typed while the agent worked. Peer messages and task
// notifications queue the same way.
//
// py: agent_decisions/transcript.py _claude_queued_prompt
func claudeQueuedPrompt(record *json.Object) (string, error) {
	queued, err := orEmpty(record.Value("attachment"))
	if err != nil {
		return "", err
	}
	if !isStr(queued.Value("type"), "queued_command") || !isStr(queued.Value("commandMode"), "prompt") ||
		queued.Value("humanTurn") != true {
		return "", nil
	}
	if kind, err := originKind(queued, nil); err != nil || !isStr(kind, "human") {
		return "", err
	}
	return text(queued.Value("prompt"))
}

// claudeAnswers returns the answers the user picked or typed in a question box, without the agent's
// question. A hook or an SDK callback can also fill in answers, so only a user permission decision counts.
//
// py: agent_decisions/transcript.py _claude_answers
func claudeAnswers(record *json.Object, callID any, calls []ToolCall) ([]string, error) {
	decision, ok := record.Value("permissionDecision").(*json.Object)
	if !ok || !isStr(decision.Value("decision"), "accept") || !isStr(decision.Value("source"), "user_temporary") {
		return nil, nil
	}
	var answers *json.Object
	if result, ok := record.Value("toolUseResult").(*json.Object); ok {
		answers, _ = result.Value("answers").(*json.Object)
	}
	request, found := "", false
	for _, call := range calls {
		if isStr(callID, call.ID) && call.Name == "AskUserQuestion" {
			request, found = call.Input, true
			break
		}
	}
	if answers == nil || !found {
		return nil, nil
	}
	asked, err := askedQuestions(request)
	if err != nil {
		return nil, err
	}
	var words []string
	for _, question := range answers.Keys() {
		if !asked[question] {
			return nil, nil
		}
		answer := answers.Value(question)
		if labels, ok := stringList(answer); ok {
			answer = strings.Join(labels, ", ") // an SDK host can return multi-select labels as a list
		}
		if answer, ok := answer.(string); ok && pystr.Strip(answer) != "" {
			words = append(words, answer)
		}
	}
	return words, nil
}

// askedQuestions returns the str "question" values of request["questions"], or nil when Python's set
// comprehension raises an exception that _claude_answers catches. A nil set rejects every answer, as the
// caught exception does; an empty one asked nothing, so every answer fails it as well.
//
// py: agent_decisions/transcript.py _claude_answers (asked set)
func askedQuestions(request string) (map[string]bool, error) {
	decoded, err := json.Loads(request)
	if err != nil {
		return nil, notValueError(err)
	}
	object, ok := decoded.(*json.Object)
	if !ok {
		return nil, nil
	}
	// Iterating a dict or str yields str items, which have no .get; a non-list asks nothing usable.
	list, ok := object.Value("questions").([]any)
	if !ok {
		return nil, nil
	}
	asked := map[string]bool{}
	for _, item := range list {
		question, ok := item.(*json.Object)
		if !ok {
			return nil, nil
		}
		switch value := question.Value("question").(type) {
		case string:
			asked[value] = true
		case []any, *json.Object:
			return nil, nil // unhashable in a set
		}
	}
	return asked, nil
}

// claude reads a Claude Code transcript.
//
// py: agent_decisions/transcript.py _claude
func claude(records iter.Seq2[*json.Object, error]) (Transcript, error) {
	b := newBuilder()
	for r, err := range records {
		if err == nil {
			err = claudeRecord(b, r)
		}
		if err != nil {
			return Transcript{}, err
		}
	}
	return b.build(), nil
}

// py: agent_decisions/transcript.py _claude (loop body)
func claudeRecord(b *builder, r *json.Object) error {
	message, err := orEmpty(r.Value("message"))
	if err != nil {
		return err
	}
	content := message.Value("content")
	blocks, isList := content.([]any)
	switch {
	case isStr(r.Value("type"), "assistant") && isList:
		for _, item := range blocks {
			block, ok := item.(*json.Object)
			if !ok {
				continue
			}
			if isStr(block.Value("type"), "tool_use") {
				if err := b.call(block.ValueOr("id", ""), block.ValueOr("name", ""), block.Value("input")); err != nil {
					return err
				}
			} else if isStr(block.Value("type"), "text") && json.Truthy(block.Value("text")) {
				said, ok := block.Value("text").(string)
				if !ok {
					return notStr("assistant text", block.Value("text"))
				}
				b.lastText = said
			}
		}
	case isStr(r.Value("type"), "attachment") && !json.Truthy(r.Value("isSidechain")):
		said, err := claudeQueuedPrompt(r)
		if err != nil {
			return err
		}
		if said != "" {
			return b.user(said, r, false)
		}
	case isStr(r.Value("type"), "user"):
		var results []*json.Object
		for _, item := range blocks {
			if result, ok := item.(*json.Object); ok && isStr(result.Value("type"), "tool_result") {
				results = append(results, result)
			}
		}
		for _, result := range results {
			callID := result.ValueOr("tool_use_id", "")
			if err := b.output(callID, result.Value("content")); err != nil {
				return err
			}
			answers, err := claudeAnswers(r, callID, b.calls)
			if err != nil {
				return err
			}
			for _, answer := range answers {
				if err := b.user(answer, r, true); err != nil {
					return err
				}
			}
		}
		said, err := text(content)
		if err != nil {
			return err
		}
		if len(results) > 0 || said == "" || json.Truthy(r.Value("isCompactSummary")) || json.Truthy(r.Value("isMeta")) {
			return nil
		}
		if json.Truthy(r.Value("isSidechain")) {
			b.parent(said)
			return nil
		}
		kind, err := originKind(r, nil)
		if err != nil {
			return err
		}
		if isStr(kind, "human") || r.Value("origin") == nil && !startsWith(said, claudeEchoes...) {
			return b.user(said, r, false) // a headless `claude -p` prompt carries no origin
		}
	}
	return nil
}

// codexRecords reads a Codex transcript whose identity pass returned identity.
//
// py: agent_decisions/transcript.py _codex_records
func codexRecords(records iter.Seq2[*json.Object, error], identity *HostIdentity) (Transcript, error) {
	b := newBuilder()
	for r, err := range records {
		if err == nil {
			err = codexRecord(b, r, identity)
		}
		if err != nil {
			return Transcript{}, err
		}
	}
	built := b.build()
	built.Identity = identity
	return built, nil
}

// py: agent_decisions/transcript.py _codex_records (loop body)
func codexRecord(b *builder, r *json.Object, identity *HostIdentity) error {
	if isStr(r.Value("type"), "event_msg") {
		return codexEvent(b, r)
	}
	if !isStr(r.Value("type"), "response_item") {
		return nil
	}
	p, err := orEmpty(r.Value("payload"))
	if err != nil {
		return err
	}
	switch kind := p.Value("type"); {
	case isStr(kind, "message"):
		return codexMessage(b, r, p, identity)
	case isStr(kind, "function_call"):
		return b.call(p.ValueOr("call_id", ""), p.ValueOr("name", ""), p.ValueOr("arguments", ""))
	case isStr(kind, "custom_tool_call"):
		if !isStr(p.Value("name"), "exec") {
			return b.call(p.ValueOr("call_id", ""), p.ValueOr("name", ""), p.ValueOr("input", ""))
		}
	case isStr(kind, "function_call_output"), isStr(kind, "custom_tool_call_output"):
		return b.output(p.ValueOr("call_id", ""), p.ValueOr("output", ""))
	}
	return nil
}

// codexEvent reads a completed command, MCP call or file change event.
//
// py: agent_decisions/transcript.py _codex_records (event_msg branch)
func codexEvent(b *builder, r *json.Object) error {
	p, err := orEmpty(r.Value("payload"))
	if err != nil || !isStr(p.Value("type"), "item_completed") {
		return err
	}
	item, err := orEmpty(p.Value("item"))
	if err != nil {
		return err
	}
	callID := item.ValueOr("id", "")
	switch kind := item.Value("type"); {
	case isStr(kind, "CommandExecution"):
		if err := b.call(callID, "Bash", json.NewObject("command", item.ValueOr("command", []any{}))); err != nil {
			return err
		}
		if err := b.output(callID, item.ValueOr("aggregated_output", "")); err != nil {
			return err
		}
		execution := &json.Object{}
		for _, key := range []string{"cwd", "status", "exit_code", "process_id"} {
			if value, ok := item.Get(key); ok {
				execution.Set(key, value)
			}
		}
		b.executions[callID.(string)] = execution // call accepts only a str id
	case isStr(kind, "McpToolCall"):
		name := "mcp__" + display(item.ValueOr("server", "")) + "__" + display(item.ValueOr("tool", ""))
		if err := b.call(callID, name, item.Value("arguments")); err != nil {
			return err
		}
		return b.output(callID, item.Value("result"))
	case isStr(kind, "FileChange"):
		if err := b.call(callID, "apply_patch", item.Value("changes")); err != nil {
			return err
		}
		return b.output(callID, display(item.ValueOr("stdout", ""))+"\n"+display(item.ValueOr("stderr", "")))
	}
	return nil
}

// codexMessage reads a message response item.
//
// py: agent_decisions/transcript.py _codex_records (message branch)
func codexMessage(b *builder, r, p *json.Object, identity *HostIdentity) error {
	said, err := text(p.Value("content"))
	if err != nil {
		return err
	}
	fromUser := isStr(p.Value("role"), "user")
	trimmed := pystr.LStrip(said)
	if fromUser && strings.HasPrefix(trimmed, "<send_user_message_question_reply") {
		questions := b.questions()
		kind, err := originKind(p, "human")
		if err != nil || !isStr(kind, "human") {
			return err
		}
		matched, err := codexQuestionReply(said, questions)
		if err != nil || !matched {
			return err
		}
		return b.user(said, r, true)
	}
	if fromUser && strings.HasPrefix(trimmed, "<codex_delegation>") {
		b.parent(said)
		return nil
	}
	ownWords := said != "" && !startsWith(trimmed, codexInjected...) && !IsCodexHookFeedback(said)
	if fromUser && said != "" && identity != nil && identity.AgentID != nil {
		kind, err := originKind(p, nil)
		if err != nil {
			return err
		}
		if !isStr(kind, "human") && ownWords {
			b.parent(said)
			return nil
		}
	}
	if fromUser && ownWords {
		kind, err := originKind(p, "human")
		if err != nil {
			return err
		}
		if isStr(kind, "human") {
			// The first branch took every text that starts with the reply envelope, so Python's
			// _codex_question_reply returns False here and its `continue` never runs.
			return b.user(said, r, false)
		}
	}
	if isStr(p.Value("role"), "assistant") && said != "" {
		b.lastText = said
	}
	return nil
}

var (
	validID   = regexp.MustCompile(`\A[A-Za-z0-9_-]{1,128}\z`) // py: re.fullmatch(r"[A-Za-z0-9_-]{1,128}", value)
	validPart = regexp.MustCompile(`\A[A-Za-z0-9_-]{1,80}\z`)  // py: re.fullmatch(r"[A-Za-z0-9_-]{1,80}", part)
)

// codexIdentity returns the native identity of a Codex transcript, or nil for older transcripts that do
// not record one. It reads only as far as Python's lazy generator does.
//
// py: agent_decisions/transcript.py _codex_identity
func codexIdentity(records iter.Seq2[*json.Object, error]) (*HostIdentity, error) {
	var first *json.Object
	for r, err := range records {
		if err != nil {
			return nil, err
		}
		if !isStr(r.Value("type"), "session_meta") {
			continue
		}
		if first == nil {
			payload, ok := r.Value("payload").(*json.Object)
			if !ok {
				return nil, nil // older host transcripts do not contain native identity
			}
			if _, ok := payload.Get("source"); !ok {
				return nil, nil
			}
			first = payload
		} else if same, err := json.Equal(r.Value("payload"), first); err != nil {
			return nil, err
		} else if !same {
			return nil, &pyerr.OSError{Class: "OSError", Msg: "conflicting host identity records"}
		}
	}
	if first == nil {
		return nil, nil
	}
	threadID, _ := first.Value("id").(string)
	sessionID, _ := first.ValueOr("session_id", first.Value("id")).(string)
	cwd, _ := first.Value("cwd").(string)
	if !validID.MatchString(threadID) || !validID.MatchString(sessionID) || cwd == "" {
		return nil, &pyerr.OSError{Class: "OSError", Msg: "invalid host identity"}
	}
	source := first.Value("source")
	if _, ok := source.(string); ok {
		return &HostIdentity{ThreadID: threadID, SessionID: sessionID, Cwd: cwd}, nil
	}
	var spawn *json.Object
	if source, ok := source.(*json.Object); ok {
		if subagent, ok := source.Value("subagent").(*json.Object); ok {
			spawn, _ = subagent.Value("thread_spawn").(*json.Object)
		}
	}
	if spawn == nil {
		return nil, &pyerr.OSError{Class: "OSError", Msg: "unsupported host agent identity"}
	}
	parent, _ := spawn.Value("parent_thread_id").(string)
	agent, isAgent := spawn.Value("agent_path").(string)
	if !validID.MatchString(parent) || parent == threadID || !isAgent {
		return nil, &pyerr.OSError{Class: "OSError", Msg: "invalid host ancestry"}
	}
	parts := strings.Split(agent, "/") // ASCII separator: exact for str.split("/")
	depth, isInt := spawn.Value("depth").(json.Int)
	n, fits := depth.Int64()
	valid := len(parts) >= 3 && parts[0] == "" && parts[1] == "root" && isInt && fits &&
		n == int64(len(parts)-2) && n <= 32
	for _, part := range parts[min(2, len(parts)):] {
		valid = valid && validPart.MatchString(part)
	}
	if !valid {
		return nil, &pyerr.OSError{Class: "OSError", Msg: "invalid canonical agent path"}
	}
	return &HostIdentity{ThreadID: threadID, SessionID: sessionID, ParentThreadID: &parent, AgentID: &agent, Cwd: cwd}, nil
}

// isStr reports whether v is the str s, which is Python's v == s for a decoded JSON value.
func isStr(v any, s string) bool {
	text, ok := v.(string)
	return ok && text == s
}

// startsWith is str.startswith with a tuple of prefixes. The bytes of a WTF-8 prefix match exactly where
// its code points do.
func startsWith(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// listItem returns list[i], or nil when i is out of range.
func listItem(list []any, i int) any {
	if i < len(list) {
		return list[i]
	}
	return nil
}

// stringList returns v as a list of str when it is a list whose items are all str.
func stringList(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	labels := make([]string, len(list))
	for i, item := range list {
		if labels[i], ok = item.(string); !ok {
			return nil, false
		}
	}
	return labels, true
}

// orEmpty returns `v or {}` for a following .get call: a false value reads as an empty dict, and a true
// value that is not a dict raises AttributeError at .get.
func orEmpty(v any) (*json.Object, error) {
	if !json.Truthy(v) {
		return &json.Object{}, nil
	}
	if object, ok := v.(*json.Object); ok {
		return object, nil
	}
	return nil, &pyerr.Error{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", typeName(v))}
}

// originKind returns (record.get("origin") or {}).get("kind", fallback).
func originKind(record *json.Object, fallback any) (any, error) {
	origin, err := orEmpty(record.Value("origin"))
	if err != nil {
		return nil, err
	}
	return origin.ValueOr("kind", fallback), nil
}

// shellJoin returns shlex.join(args) for decoded JSON items. shlex.quote returns two single quotes for
// any false value and raises TypeError for any other non-str.
//
// py: Lib/shlex.py join, quote (Python 3.15)
func shellJoin(args []any) (string, error) {
	words := make([]string, len(args))
	for i, arg := range args {
		word, ok := arg.(string)
		if !ok && json.Truthy(arg) {
			return "", &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("expected string object, got '%s'", typeName(arg))}
		}
		words[i] = word
	}
	return shlex.Join(words), nil
}

// typeName returns type(v).__name__ for a decoded JSON value.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *json.Object:
		return "dict"
	}
	panic(fmt.Sprintf("transcript: %T is outside the JSON value model", v))
}

// display returns str(v), as an f-string formats a decoded JSON value.
func display(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	return json.Repr(v)
}

// unhashable returns the TypeError Python raises for a list or dict used as a set element or dict key,
// and nil for any other value.
func unhashable(v any, use string) error {
	switch v.(type) {
	case []any, *json.Object:
		name := typeName(v)
		return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("cannot use '%s' as a %s (unhashable type: '%s')", name, use, name)}
	}
	return nil
}

// notStr reports a value that Python keeps but this port cannot hold in a str field. No host writes one.
func notStr(what string, v any) error {
	return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("%s is a '%s', not a str", what, typeName(v))}
}

// notValueError returns nil for a json error that Python's `except ValueError` catches, and err for the
// RecursionError of too deep nesting, which it does not catch.
func notValueError(err error) error {
	if errors.As(err, new(*json.RecursionError)) {
		return err
	}
	return nil
}

// open opens path for reading as Python's open(path, "rb") does, with the same exceptions.
//
// py: Modules/_io/fileio.c _io_FileIO___init___impl (Python 3.15)
func open(path string) (*os.File, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return nil, &pyerr.Error{Class: "ValueError", Msg: "embedded null byte"}
	}
	stream, err := os.Open(path)
	if err != nil {
		return nil, pyerr.NewOSError(err, path)
	}
	if info, err := stream.Stat(); err == nil && info.IsDir() {
		stream.Close()
		return nil, pyerr.NewOSError(syscall.EISDIR, path)
	}
	return stream, nil
}
