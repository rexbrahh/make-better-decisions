package hook

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
)

// The synthetic transcripts of the large-transcript benchmark: about 24 MB each, shaped like the records
// that Claude Code and Codex write, with non-ASCII text and escapes in the bodies.
const (
	largeTurns   = 4000
	largeRequest = `{"decision": "Which cache should parser.py use?", "options": [{"id": "lru", "description": "LRU"}, {"id": "lfu", "description": "LFU"}]}`
)

var largeWords = strings.Fields(`the cache parser tile reads bursts eviction policy index résumé naïve 東京 データ
	función ✓ — "quoted" tab	path\to\file emoji 🚀 error retry budget latency throughput lock queue shard`)

// largeText returns n words drawn from largeWords, with a line break every twelve words.
func largeText(random *rand.Rand, n int) string {
	var text strings.Builder
	for i := range n {
		switch {
		case i%12 == 11:
			text.WriteByte('\n')
		case i > 0:
			text.WriteByte(' ')
		}
		text.WriteString(largeWords[random.IntN(len(largeWords))])
	}
	return text.String()
}

// largeClaude returns the records of a long Claude Code session: a typed prompt every 25 turns, and an
// assistant turn of thinking, text and one tool call, answered by a large tool result.
func largeClaude(work string) []*json.Object {
	random := rand.New(rand.NewPCG(1, 2))
	var records []*json.Object
	parent := any(nil)
	add := func(record *json.Object) {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", len(records), len(records))
		record = merge(json.NewObject("parentUuid", parent, "isSidechain", false, "userType", "external",
			"cwd", work, "sessionId", "s1", "version", "2.1.0", "gitBranch", "main"), record)
		record.Set("uuid", id)
		record.Set("timestamp", fmt.Sprintf("2026-10-14T%02d:%02d:%02d.000Z", len(records)/3600%24, len(records)/60%60, len(records)%60))
		parent = id
		records = append(records, record)
	}
	for turn := range largeTurns {
		if turn%25 == 0 {
			add(human(fmt.Sprintf("Turn %d: %s", turn, largeText(random, 40))))
		}
		id := fmt.Sprintf("toolu_%06d", turn)
		var name string
		var input *json.Object
		switch turn % 4 {
		case 0:
			name, input = "Read", json.NewObject("file_path", filepath.Join(work, fmt.Sprintf("src/f%d.py", turn%97)))
		case 1:
			name, input = "Bash", json.NewObject("command", "rg -n cache src | head -50", "description", "Search the cache")
		case 2:
			name, input = "Edit", json.NewObject("file_path", filepath.Join(work, "parser.py"),
				"old_string", largeText(random, 30), "new_string", largeText(random, 30))
		case 3:
			name, input = "Grep", json.NewObject("pattern", "cache", "path", work)
		}
		if turn%200 == 7 {
			name, input = "Bash", json.NewObject("command", "ts-decide "+shlex.Quote(largeRequest), "description", "decide")
		}
		add(json.NewObject("type", "assistant", "message", json.NewObject(
			"id", fmt.Sprintf("msg_%06d", turn), "type", "message", "role", "assistant", "model", "claude-opus-5-5",
			"content", []any{
				json.NewObject("type", "thinking", "thinking", largeText(random, 150), "signature", strings.Repeat("Sig0", 80)),
				json.NewObject("type", "text", "text", largeText(random, 60)),
				json.NewObject("type", "tool_use", "id", id, "name", name, "input", input),
			},
			"stop_reason", "tool_use", "usage", json.NewObject("input_tokens", json.IntOf(int64(turn)),
				"output_tokens", json.IntOf(512), "cache_read_input_tokens", json.IntOf(90000)))))
		output := largeText(random, 700)
		if name == "Bash" && turn%200 == 7 {
			output = "ts-decide verdict: ADVISORY\nPick lfu."
		}
		result := toolResult(id, output)
		result.Set("toolUseResult", json.NewObject("stdout", output[:min(len(output), 200)], "stderr", "", "interrupted", false))
		add(result)
	}
	return records
}

// largeCodex returns the records of a long Codex session in the same proportions as largeClaude.
func largeCodex(work string) []*json.Object {
	random := rand.New(rand.NewPCG(3, 4))
	records := []*json.Object{
		oracleNativeMetadata("c1", "c1", work, "", ""),
		json.NewObject("type", "turn_context", "payload", json.NewObject("cwd", work, "model", "gpt-6",
			"approval_policy", "on-request", "sandbox_policy", json.NewObject("mode", "workspace-write"))),
	}
	for turn := range largeTurns {
		if turn%25 == 0 {
			records = append(records, codexUser(fmt.Sprintf("Turn %d: %s", turn, largeText(random, 40))))
		}
		id := fmt.Sprintf("call_%06d", turn)
		arguments := dumpsCompact(json.NewObject("cmd", "rg -n cache src | head -50", "workdir", work))
		if turn%200 == 7 {
			arguments = dumpsCompact(json.NewObject("cmd", "ts-decide "+shlex.Quote(largeRequest)))
		}
		output := largeText(random, 700)
		records = append(records,
			json.NewObject("type", "response_item", "payload", json.NewObject("type", "reasoning",
				"summary", []any{json.NewObject("type", "summary_text", "text", largeText(random, 40))},
				"encrypted_content", strings.Repeat("gAAAAAB", 200))),
			codexAssistant(largeText(random, 60)),
			codexCall(id, "exec_command", arguments),
			codexOutput(id, output),
			json.NewObject("type", "event_msg", "payload", json.NewObject("type", "token_count",
				"info", json.NewObject("total_token_usage", json.NewObject("input_tokens", json.IntOf(int64(turn)))))),
		)
	}
	return records
}

func dumpsCompact(v any) string {
	text, err := json.Dumps(v, json.Options{Compact: true, AllowUnicode: true})
	if err != nil {
		panic(err)
	}
	return text
}

// writeLarge writes records as compact JSON lines, as the hosts do, and returns the path.
func writeLarge(tb testing.TB, path string, records []*json.Object) string {
	tb.Helper()
	var text strings.Builder
	for _, record := range records {
		text.WriteString(dumpsCompact(record) + "\n")
	}
	if err := os.WriteFile(path, []byte(text.String()), 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

// largeSteps writes the synthetic transcripts into work and returns the gated hook calls against them, by
// name: each reads and parses its whole transcript and builds evidence from it.
func largeSteps(tb testing.TB, work string) (names []string, steps []replayStep) {
	tb.Helper()
	writeFile(tb, filepath.Join(work, "parser.py"), "def parse(text):\n    return text.split()\n")
	claude := writeLarge(tb, filepath.Join(work, "claude.jsonl"), largeClaude(work))
	codex := writeLarge(tb, filepath.Join(work, "codex.jsonl"), largeCodex(work))
	parser := filepath.Join(work, "parser.py")
	sure := answers("pick", "lfu", "sufficient", 0.9, "owner", "agent")
	claudeBase := json.NewObject("session_id", "s1", "transcript_path", claude, "cwd", work)
	codexBase := json.NewObject("session_id", "c1", "transcript_path", codex, "cwd", work)
	add := func(name, host string, base *json.Object, tool string, input any, fake replayFake) {
		names = append(names, name)
		steps = append(steps, call("pre_tool_use", host, with(base, "tool_name", tool, "tool_use_id", "bench_"+name,
			"tool_input", input), fake))
	}
	add("claude-Edit", "claude", claudeBase, "Edit", json.NewObject("file_path", parser,
		"old_string", "text.split()", "new_string", "text.split(None)"), answers("needs_decision", 0.0))
	add("claude-ExitPlanMode", "claude", claudeBase, "ExitPlanMode", json.NewObject("plan", "1. Use LFU in parser.py"), replayFake{})
	add("claude-decide", "claude", claudeBase, "Bash", json.NewObject("command", "ts-decide "+shlex.Quote(largeRequest),
		"description", "decide"), sure)
	add("codex-apply_patch", "codex", codexBase, "apply_patch", json.NewObject("input",
		"*** Begin Patch\n*** Update File: parser.py\n@@\n-    return text.split()\n+    return text.split(None)\n*** End Patch"),
		answers("needs_decision", 0.0))
	add("codex-decide", "codex", codexBase, "exec_command", json.NewObject("cmd", "ts-decide "+shlex.Quote(largeRequest)), sure)
	return names, steps
}

// BenchmarkHandleLargeTranscript times Handle for each gated call against the synthetic transcripts. Each
// iteration starts from an empty state directory.
func BenchmarkHandleLargeTranscript(b *testing.B) {
	work := setUp(b)
	patch(b, &now, func() time.Time { return time.Unix(oracleTime, 0) })
	patch(b, &perfCounter, func() float64 { return 0 })
	patch(b, &newID, func() string { return oracleUUID })
	names, steps := largeSteps(b, work)
	for i, step := range steps {
		b.Run(names[i], func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				resetState(b)
				payload, err := json.Loads(dumpsCompact(step.payload))
				if err != nil {
					b.Fatal(err)
				}
				api := step.fake.jev()
				b.StartTimer()
				if _, err := Handle(step.event, payload, step.host, api.Post); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestHandleLargeTranscriptMatchesPython replays the gated calls of BenchmarkHandleLargeTranscript through
// hook.py handle and Go Handle, compares them as TestHandleMatchesPython does, and logs the wall time of
// each handle call in both engines. It is opt-in, since Python takes seconds per call.
func TestHandleLargeTranscriptMatchesPython(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_LARGE") != "1" {
		t.Skip("set AGENT_DECISIONS_LARGE=1 and AGENT_DECISIONS_PYTHON to replay the large synthetic transcripts")
	}
	work, python := oracleSetUp(t)
	names, steps := largeSteps(t, work)
	state := resetState(t)
	fromPython := runPython(t, python, steps)
	pythonFiles := snapshot(t, state)
	resetState(t)
	fromGo := runGo(t, steps)
	goFiles := snapshot(t, state)
	for i, name := range names {
		t.Logf("%-20s python %8.1f ms   go %8.1f ms", name, ms(fromPython[i].elapsed), ms(fromGo[i].elapsed))
	}
	pn, gn := &normalizer{receipts: map[string]string{}}, &normalizer{receipts: map[string]string{}}
	compareResults(t, steps, fromPython, fromGo, pn, gn)
	compareStates(t, pythonFiles, goFiles, pn, gn)
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
