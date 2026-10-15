package hook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// TestReplayRealTranscripts replays the hook calls of the newest local Claude and Codex transcripts through
// hook.py handle and Go Handle, and compares them as TestHandleMatchesPython does.
//
// The transcripts are the user's private data. The test is opt-in, reads only copies under t.TempDir(), and
// logs only counts, hosts, tool names, gates, ordinals and line numbers.
//
// Environment: AGENT_DECISIONS_REAL_TRANSCRIPTS=1 and AGENT_DECISIONS_PYTHON enable it.
// AGENT_DECISIONS_REPLAY_FILES (default 30) is the number of newest transcripts per host.
// AGENT_DECISIONS_REPLAY_MAX_MB (default 20) skips larger transcripts: every gated call reparses its
// transcript prefix, so the cost grows with calls times size.
func TestReplayRealTranscripts(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_REAL_TRANSCRIPTS") != "1" {
		t.Skip("set AGENT_DECISIONS_REAL_TRANSCRIPTS=1 and AGENT_DECISIONS_PYTHON to replay local transcripts")
	}
	files := envInt(t, "AGENT_DECISIONS_REPLAY_FILES", 30)
	maxBytes := int64(envInt(t, "AGENT_DECISIONS_REPLAY_MAX_MB", 20)) << 20
	home, err := os.UserHomeDir() // before oracleSetUp replaces HOME
	if err != nil {
		t.Fatal(err)
	}
	work, python := oracleSetUp(t)
	corpus := map[string][]string{"claude": claudeTranscripts(t, home), "codex": codexTranscripts(t, home)}

	r := &replayReport{steps: map[string]int{}, mismatches: map[string][]string{}}
	for _, host := range []string{"claude", "codex"} {
		found := newest(t, corpus[host], files)
		r.logf("%s: %d transcripts found, replaying the newest %d", host, len(corpus[host]), len(found))
		for i, source := range found {
			name := fmt.Sprintf("%s#%d", host, i+1)
			if source.size > maxBytes {
				r.logf("%s: skipped, over %d MiB", name, maxBytes>>20)
				continue
			}
			r.replayTranscript(t, python, filepath.Join(work, "replay", name), name, host, source)
		}
	}
	r.print(t)
}

// transcriptFile is a transcript path with its size and modification time at discovery.
type transcriptFile struct {
	path     string
	size     int64
	modified time.Time
}

// claudeTranscripts returns ~/.claude*/projects/*/*.jsonl, without the second spelling of a symlinked file.
func claudeTranscripts(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, ".claude*", "projects", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, path := range matches {
		real, err := filepath.EvalSymlinks(path)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		paths = append(paths, real)
	}
	return paths
}

// codexTranscripts returns ~/.codex/sessions/**/*.jsonl.
func codexTranscripts(t *testing.T, home string) []string {
	t.Helper()
	var paths []string
	root := filepath.Join(home, ".codex", "sessions")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return paths
}

// newest returns the n most recently modified paths, newest first.
func newest(t *testing.T, paths []string, n int) []transcriptFile {
	t.Helper()
	var found []transcriptFile
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue // removed since the listing
		}
		found = append(found, transcriptFile{path, info.Size(), info.ModTime()})
	}
	slices.SortFunc(found, func(a, b transcriptFile) int { return b.modified.Compare(a.modified) })
	return found[:min(n, len(found))]
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	text := os.Getenv(name)
	if text == "" {
		return fallback
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < 1 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return value
}

// copyPrefix copies the first size bytes of source to target: the transcript as it was at discovery.
func copyPrefix(t *testing.T, source, target string, size int64) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var data bytes.Buffer
	if _, err := io.Copy(&data, io.LimitReader(in, size)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

// --- the hook calls of a transcript

// stepMeta places a replayed step in its transcript, for the report.
type stepMeta struct {
	tool    string // the tool name, or "-"
	ordinal int    // the 1-based ordinal of the tool call or prompt in its transcript
	line    int    // the 1-based line of the record that caused the step
}

// toolCall is a tool call as the host recorded it.
type toolCall struct {
	id, name string
	input    any
}

// field returns v[key] when v is a dict, else nil.
func field(v any, key string) any {
	if object, ok := v.(*json.Object); ok && object != nil {
		return object.Value(key)
	}
	return nil
}

// contentText ports transcript.py _text for a str or a list of text blocks.
func contentText(content any) string {
	switch content := content.(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, block := range content {
			switch field(block, "type") {
			case "text", "input_text", "output_text":
				text, _ := field(block, "text").(string)
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func hasPrefix(s string, prefixes ...string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(s, prefix) })
}

// claudePrompt returns the text of a record that transcript.py _claude counts as a typed user prompt.
// Queued prompts and question answers do not fire UserPromptSubmit, so they are not prompts here.
func claudePrompt(r *json.Object) (string, bool) {
	if r.Value("type") != "user" || json.Truthy(r.Value("isSidechain")) || json.Truthy(r.Value("isMeta")) ||
		json.Truthy(r.Value("isCompactSummary")) {
		return "", false
	}
	content := field(r.Value("message"), "content")
	if blocks, ok := content.([]any); ok && slices.ContainsFunc(blocks, func(block any) bool {
		return field(block, "type") == "tool_result"
	}) {
		return "", false
	}
	text := contentText(content)
	if text == "" {
		return "", false
	}
	if origin := r.Value("origin"); origin != nil {
		return text, field(origin, "kind") == "human"
	}
	return text, !hasPrefix(text, "<bash-stdout>", "<bash-stderr>", "<local-command-stdout>", "<local-command-stderr>")
}

// codexPrompt returns the text of a record that transcript.py _codex_records counts as a typed user prompt.
func codexPrompt(r *json.Object) (string, bool) {
	p := r.Value("payload")
	if r.Value("type") != "response_item" || field(p, "type") != "message" || field(p, "role") != "user" {
		return "", false
	}
	text := contentText(field(p, "content"))
	kind := field(field(p, "origin"), "kind")
	if kind == nil {
		kind = "human"
	}
	return text, text != "" && kind == "human" && !hasPrefix(pystr.LStrip(text), "# AGENTS.md instructions",
		"<environment_context>", "<user_instructions>", "The following is the Codex agent history",
		"<codex_delegation>", "<subagent_notification>", "<external_codex_apps_open_page>", "<hook_prompt ",
		"<hook_prompt>", "<send_user_message_question_reply")
}

// claudeCalls returns the tool_use blocks of an assistant record.
func claudeCalls(r *json.Object) []toolCall {
	blocks, _ := field(r.Value("message"), "content").([]any)
	if r.Value("type") != "assistant" {
		return nil
	}
	var calls []toolCall
	for _, block := range blocks {
		if field(block, "type") == "tool_use" {
			id, _ := field(block, "id").(string)
			name, _ := field(block, "name").(string)
			calls = append(calls, toolCall{id, name, field(block, "input")})
		}
	}
	return calls
}

// codexCalls returns the tool calls of a record as transcript.py _codex_records names them. A
// function_call's arguments become the dict the host decodes from them.
func codexCalls(r *json.Object) []toolCall {
	p := r.Value("payload")
	switch r.Value("type") {
	case "event_msg":
		item := field(p, "item")
		if field(p, "type") != "item_completed" {
			return nil
		}
		id, _ := field(item, "id").(string)
		switch field(item, "type") {
		case "CommandExecution":
			command := field(item, "command")
			if command == nil {
				command = []any{}
			}
			return []toolCall{{id, "Bash", json.NewObject("command", command)}}
		case "McpToolCall":
			server, _ := field(item, "server").(string)
			tool, _ := field(item, "tool").(string)
			return []toolCall{{id, "mcp__" + server + "__" + tool, field(item, "arguments")}}
		case "FileChange":
			return []toolCall{{id, "apply_patch", field(item, "changes")}}
		}
	case "response_item":
		id, _ := field(p, "call_id").(string)
		name, _ := field(p, "name").(string)
		switch field(p, "type") {
		case "function_call":
			arguments, _ := field(p, "arguments").(string)
			var input any = arguments
			if decoded, err := json.Loads(arguments); err == nil {
				if object, ok := decoded.(*json.Object); ok {
					input = object
				}
			}
			return []toolCall{{id, name, input}}
		case "custom_tool_call":
			if name != "exec" {
				return []toolCall{{id, name, field(p, "input")}}
			}
		}
	}
	return nil
}

// readsTranscript reports whether hook.py _pre_tool_use loads the transcript for this call: the edit tools,
// ExitPlanMode, and a shell call that classify can read as ts-decide.
//
// ponytail: a shell call is a decide candidate when its input mentions "decide"; a ts-decide spelled
// without that substring gets no transcript file, and both engines then see it as unreadable.
func readsTranscript(name string, input any) bool {
	switch {
	case policy.EditTools[name] || name == "ExitPlanMode":
		return true
	case policy.ShellTools[name]:
		text, err := json.Dumps(input, json.Options{AllowUnicode: true})
		return err != nil || strings.Contains(text, "decide")
	}
	return false
}

// transcriptSteps returns the hook calls that host made for a transcript, in order: session_start before
// the first prompt or tool call, user_prompt_submit for each typed prompt, and pre_tool_use for each tool
// call. A call's transcript_path names a copy of the records before its own record, which is what the
// host had written when the hook ran (docs/probes.md A1, A5). Only calls that read the transcript get
// that copy on disk; the hook does not open the path of any other call.
// skippedCalls counts host records that the replay does not turn into hook calls.
type skippedCalls struct {
	sidechainCalls  int // Claude tool calls in sidechain records, whose agent_id the record does not carry
	subagentPrompts int // user messages of a Codex subagent thread: its brief, not a typed prompt
}

func transcriptSteps(t *testing.T, host string, data []byte, dir string) ([]replayStep, []stepMeta, skippedCalls) {
	t.Helper()
	var steps []replayStep
	var meta []stepMeta
	var session, cwd any
	seen := map[string]bool{}
	started, subagent := false, false
	calls, prompts := 0, 0
	var skipped skippedCalls
	offset := 0
	for number, line := range bytes.SplitAfter(data, []byte("\n")) {
		start := offset
		offset += len(line)
		value, err := json.LoadsBytes(line, 1)
		r, ok := value.(*json.Object)
		if err != nil || !ok {
			continue
		}
		var found []toolCall
		var prompt string
		var isPrompt bool
		if host == "claude" {
			if text, ok := r.Value("sessionId").(string); ok {
				session = text
			}
			if text, ok := r.Value("cwd").(string); ok {
				cwd = text
			}
			if json.Truthy(r.Value("isSidechain")) {
				skipped.sidechainCalls += len(claudeCalls(r))
			} else {
				found = claudeCalls(r)
			}
			prompt, isPrompt = claudePrompt(r)
		} else {
			p := r.Value("payload")
			switch r.Value("type") {
			case "session_meta":
				session = field(p, "session_id")
				if session == nil {
					session = field(p, "id")
				}
				cwd = field(p, "cwd")
				_, native := field(p, "source").(string)
				subagent = field(p, "source") != nil && !native
			case "turn_context":
				if text, ok := field(p, "cwd").(string); ok {
					cwd = text
				}
			}
			found = codexCalls(r)
			prompt, isPrompt = codexPrompt(r)
			if isPrompt && subagent {
				isPrompt = false
				skipped.subagentPrompts++
			}
		}
		found = slices.DeleteFunc(found, func(call toolCall) bool {
			if seen[call.id] {
				return true // response items and completed events can describe the same call
			}
			seen[call.id] = true
			return false
		})
		if !isPrompt && len(found) == 0 {
			continue
		}
		prefix := filepath.Join(dir, strconv.Itoa(number)+".jsonl")
		materialize := func() {
			if _, err := os.Stat(prefix); err == nil {
				return
			}
			if err := os.WriteFile(prefix, data[:start], 0o600); err != nil {
				t.Fatal(err)
			}
		}
		base := json.NewObject("session_id", session, "transcript_path", prefix, "cwd", cwd)
		if !started {
			started = true
			materialize()
			steps = append(steps, call("session_start", host, with(base, "source", "startup"), replayFake{}))
			meta = append(meta, stepMeta{"-", 1, number + 1})
		}
		if isPrompt {
			prompts++
			steps = append(steps, call("user_prompt_submit", host, with(base, "prompt", prompt), replayFake{}))
			meta = append(meta, stepMeta{"-", prompts, number + 1})
		}
		for _, c := range found {
			calls++
			if readsTranscript(c.name, c.input) {
				materialize()
			}
			payload := with(base, "tool_name", c.name, "tool_use_id", c.id, "tool_input", c.input)
			steps = append(steps, call("pre_tool_use", host, payload, replayFake{}))
			meta = append(meta, stepMeta{c.name, calls, number + 1})
		}
	}
	return steps, meta, skipped
}

// --- the replay

// replayReport collects what TestReplayRealTranscripts prints. It holds no payload text.
type replayReport struct {
	lines        []string
	steps        map[string]int      // "host event" to count
	mismatches   map[string][]string // "host event tool gate: what" to where
	pythonTime   time.Duration
	goTime       time.Duration
	transcripts  int
	materialized int
	skipped      skippedCalls
}

func (r *replayReport) logf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// replayTranscript runs one transcript through both engines and records the differences.
func (r *replayReport) replayTranscript(t *testing.T, python, dir, name, host string, source transcriptFile) {
	t.Helper()
	defer os.RemoveAll(dir)
	data := copyPrefix(t, source.path, filepath.Join(dir, "copy", "transcript.jsonl"), source.size)
	steps, meta, skipped := transcriptSteps(t, host, data, dir)
	r.skipped.sidechainCalls += skipped.sidechainCalls
	r.skipped.subagentPrompts += skipped.subagentPrompts
	if len(steps) == 0 {
		r.logf("%s: no prompts or tool calls", name)
		return
	}
	prefixes, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	r.transcripts++
	r.materialized += len(prefixes)
	for _, step := range steps {
		r.steps[host+" "+step.event]++
	}

	state := resetState(t)
	started := time.Now()
	fromPython := runPythonQuietly(t, python, steps)
	elapsed := time.Since(started)
	r.pythonTime += elapsed
	pythonFiles := snapshot(t, state)
	checkSeals(t, "python", pythonFiles)

	resetState(t)
	started = time.Now()
	fromGo := runGo(t, steps)
	r.goTime += time.Since(started)
	goFiles := snapshot(t, state)
	checkSeals(t, "go", goFiles)

	before := r.count()
	r.compare(t, name, host, steps, meta, fromPython, fromGo, pythonFiles, goFiles)
	r.logf("%s: %d steps, %d transcript prefixes, python %s, %d mismatches", name, len(steps), len(prefixes),
		elapsed.Round(time.Millisecond), r.count()-before)
}

func (r *replayReport) count() int {
	total := 0
	for _, where := range r.mismatches {
		total += len(where)
	}
	return total
}

// runPythonQuietly is runPython for private data: on a driver failure it reports the exit status only,
// since a Python traceback can quote the payload.
func runPythonQuietly(t *testing.T, python string, steps []replayStep) []*stepResult {
	t.Helper()
	wire := make([]any, len(steps))
	for i, step := range steps {
		wire[i] = json.NewObject("kind", "handle", "event", step.event, "host", step.host,
			"payload", step.payload, "fake", step.fake.wire())
	}
	source := oracletest.Source(t)
	command := exec.Command(python, "-I", "-c", replayScript, source)
	command.Stdin = strings.NewReader(dumps(t, json.NewObject("time", json.IntOf(oracleTime), "uuid", oracleUUID,
		"steps", wire)))
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("python replay driver: exit status %d (stderr withheld: it can quote private data)", exit.ExitCode())
		}
		t.Fatalf("python replay driver: %v", err)
	}
	// The Python engine reports paths under its own checkout; the Go engine reports them under this repo.
	output = bytes.ReplaceAll(output, []byte(source), []byte(repoRoot))
	replies, ok := loads(t, string(output)).([]any)
	if !ok || len(replies) != len(steps) {
		t.Fatalf("python replay driver: %d results for %d steps", len(replies), len(steps))
	}
	results := make([]*stepResult, len(replies))
	for i, reply := range replies {
		object := reply.(*json.Object)
		result := &stepResult{}
		if text, ok := object.Value("out").(string); ok {
			result.out = &text
		}
		if failure, ok := object.Value("error").([]any); ok {
			result.errClass, result.errText = failure[0].(string), failure[1].(string)
		}
		for _, request := range object.Value("requests").([]any) {
			result.requests = append(result.requests, request.(string))
		}
		results[i] = result
	}
	return results
}

// compare records every difference between the engines' results and state files, with the
// normalizations of TestHandleMatchesPython: receipt ids become ordinals, Jev request bodies drop jev's
// own timings and are sorted, and a ledger line that differs byte for byte compares without its seal.
// Ledger lines are grouped by tool_use_id so that a difference names its call; checkSeals still proves
// each engine's seals, and a separate check catches rows that only change order.
func (r *replayReport) compare(t *testing.T, name, host string, steps []replayStep, meta []stepMeta,
	python, golang []*stepResult, pythonFiles, goFiles map[string][]byte) {
	t.Helper()
	gateOf := rowGates(t, pythonFiles)
	byID, byVerdict := map[string]int{}, map[string]int{}
	for i, step := range steps {
		if id, ok := field(step.payload, "tool_use_id").(string); ok && id != "" {
			byID[id], byVerdict["verdicts/"+ledger.VerdictName(id)] = i, i
		}
	}
	add := func(i int, what string) {
		step, m := steps[i], meta[i]
		gate := gateOf[fmt.Sprint(field(step.payload, "tool_use_id"))]
		if step.event != "pre_tool_use" || gate == "" {
			gate = "-"
		}
		key := fmt.Sprintf("%s %s %s %s: %s", host, step.event, m.tool, gate, what)
		r.mismatches[key] = append(r.mismatches[key], fmt.Sprintf("%s ordinal %d line %d", name, m.ordinal, m.line))
	}
	other := func(file, what string) {
		key := fmt.Sprintf("%s state %s: %s", host, stateKind(file), what)
		r.mismatches[key] = append(r.mismatches[key], name)
	}

	pn, gn := &normalizer{map[string]string{}}, &normalizer{map[string]string{}}
	for i := range steps {
		p, g := python[i], golang[i]
		switch {
		case p.errClass != g.errClass:
			add(i, fmt.Sprintf("error class python %q, go %q", p.errClass, g.errClass))
		case p.errText != g.errText:
			add(i, "error text ("+p.errClass+")")
		}
		switch {
		case (p.out == nil) != (g.out == nil):
			add(i, fmt.Sprintf("response python %s, go %s", presence(p.out), presence(g.out)))
		case p.out != nil && *p.out != *g.out && pn.text(*p.out) != gn.text(*g.out):
			add(i, "response")
		}
		if !slices.Equal(sortedRequests(t, pn, p.requests), sortedRequests(t, gn, g.requests)) {
			add(i, fmt.Sprintf("Jev requests (python %d, go %d)", len(p.requests), len(g.requests)))
		}
	}

	for _, file := range slices.Sorted(maps.Keys(mergedKeys(pythonFiles, goFiles))) {
		p, pOK := pythonFiles[file]
		g, gOK := goFiles[file]
		if !strings.HasSuffix(file, ".jsonl") {
			if pOK && gOK && (bytes.Equal(p, g) || pn.text(string(p)) == gn.text(string(g))) {
				continue
			}
			what := "content differs"
			if !pOK || !gOK {
				what = fmt.Sprintf("written by python %v, go %v", pOK, gOK)
			}
			if i, ok := byVerdict[file]; ok {
				add(i, what)
			} else {
				other(file, what)
			}
			continue
		}
		pIDs, pRows := rowsByCall(t, p)
		gIDs, gRows := rowsByCall(t, g)
		for _, id := range slices.Sorted(maps.Keys(mergedKeys(pRows, gRows))) {
			pl, gl := pRows[id], gRows[id]
			what := ""
			if len(pl) != len(gl) {
				what = fmt.Sprintf("%s rows python %d, go %d", stateKind(file), len(pl), len(gl))
			} else {
				for j := range pl {
					if pl[j] != gl[j] && pn.json(t, pl[j]) != gn.json(t, gl[j]) {
						what = fmt.Sprintf("%s row %d of the call", stateKind(file), j+1)
						break
					}
				}
			}
			if what == "" {
				continue
			}
			if i, ok := byID[id]; ok {
				add(i, what)
			} else {
				other(file, what+" (session or prompt rows)")
			}
		}
		if len(pIDs) == len(gIDs) && !slices.Equal(pIDs, gIDs) {
			other(file, "row order")
		}
	}
}

func presence(s *string) string {
	if s == nil {
		return "None"
	}
	return "present"
}

func mergedKeys[V any](a, b map[string]V) map[string]bool {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return keys
}

// stateKind names a state file without its session id or hash.
func stateKind(file string) string {
	switch {
	case strings.HasPrefix(file, "sessions/architecture-"):
		return "architecture ledger"
	case strings.HasPrefix(file, "sessions/"):
		return "session ledger"
	case strings.HasPrefix(file, "verdicts/"):
		return "verdict file"
	}
	return "other file"
}

// rowsByCall splits a ledger file into its lines, grouped by the entry's tool_use_id, and returns the
// tool_use_id of each line in file order.
func rowsByCall(t *testing.T, data []byte) ([]string, map[string][]string) {
	t.Helper()
	var ids []string
	rows := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		id := fmt.Sprint(field(field(loads(t, line), "entry"), "tool_use_id"))
		ids = append(ids, id)
		rows[id] = append(rows[id], line)
	}
	return ids, rows
}

// rowGates maps each tool_use_id in Python's session ledgers to the gate of its last row.
func rowGates(t *testing.T, files map[string][]byte) map[string]string {
	t.Helper()
	gates := map[string]string{}
	for file, data := range files {
		if !strings.HasPrefix(file, "sessions/") || strings.HasPrefix(file, "sessions/architecture-") {
			continue
		}
		_, rows := rowsByCall(t, data)
		for id, lines := range rows {
			gates[id] = fmt.Sprint(field(field(loads(t, lines[len(lines)-1]), "entry"), "gate"))
		}
	}
	return gates
}

// print logs the report and fails the test on any mismatch.
func (r *replayReport) print(t *testing.T) {
	t.Helper()
	for _, line := range r.lines {
		t.Log(line)
	}
	t.Logf("replayed %d transcripts; %d transcript prefixes written; skipped %d Claude sidechain calls and %d Codex subagent briefs",
		r.transcripts, r.materialized, r.skipped.sidechainCalls, r.skipped.subagentPrompts)
	for _, key := range slices.Sorted(maps.Keys(r.steps)) {
		t.Logf("calls replayed: %s %d", key, r.steps[key])
	}
	t.Logf("wall time: python %s (including interpreter start-up per transcript), go %s",
		r.pythonTime.Round(time.Millisecond), r.goTime.Round(time.Millisecond))
	for _, key := range slices.Sorted(maps.Keys(r.mismatches)) {
		where := r.mismatches[key]
		t.Errorf("mismatch %s: %d, at %s", key, len(where), strings.Join(where[:min(5, len(where))], "; "))
	}
	t.Logf("%d mismatches", r.count())
}
