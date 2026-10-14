package ledger

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
)

// useState gives the test its own state directory, as tests.helpers.StateTestCase does.
func useState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_DECISIONS_STATE", state)
	return state
}

// object decodes a JSON object literal.
func object(t *testing.T, text string) *json.Object {
	t.Helper()
	value, err := json.Loads(text)
	if err != nil {
		t.Fatalf("Loads(%q): %v", text, err)
	}
	return value.(*json.Object)
}

func mustAppend(t *testing.T, entry *json.Object) {
	t.Helper()
	if err := Append(entry); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func mustEntries(t *testing.T, sessionID string, cacheLimit *int) []*json.Object {
	t.Helper()
	rows, err := Entries(sessionID, cacheLimit)
	if err != nil {
		t.Fatalf("Entries(%q): %v", sessionID, err)
	}
	return rows
}

func limit(n int) *int { return &n }

// assertRows checks rows == want with Python ==.
func assertRows(t *testing.T, rows []*json.Object, want ...*json.Object) {
	t.Helper()
	got, expected := make([]any, len(rows)), make([]any, len(want))
	for i, row := range rows {
		got[i] = row
	}
	for i, row := range want {
		expected[i] = row
	}
	if !equals(t, got, expected) {
		t.Fatalf("rows = %s, want %s", dumps(t, got, json.Options{}), dumps(t, expected, json.Options{}))
	}
}

func appendText(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func makeOld(t *testing.T, paths ...string) {
	t.Helper()
	old := time.Now().Add(-(policy.RetainDays + 1) * 24 * time.Hour)
	for _, path := range paths {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// py: tests/test_ledger.py::test_bounded_cache_preserves_authority_and_recent_complete_payloads
func TestBoundedCachePreservesAuthorityAndRecentCompletePayloads(t *testing.T) {
	useState(t)
	decision := object(t, `{"session_id": "s", "gate": "decide", "state": {"receipt": "complete"}}`)
	users := object(t, `{"session_id": "s", "gate": "user_context", "messages": ["Original instruction."]}`)
	mustAppend(t, decision)
	mustAppend(t, users)
	for index, gate := range []string{"edit", "question", "plan", "stop"} {
		state := json.NewObject("index", json.IntOf(int64(index)))
		mustAppend(t, json.NewObject("session_id", "s", "gate", gate, "state", state,
			"chunks", []any{json.NewObject("state", state)}))
	}
	rows := mustEntries(t, "s", limit(2))
	assertRows(t, rows[:2], decision, users)
	for _, row := range rows[2:4] {
		if _, has := row.Get("state"); has {
			t.Fatalf("expired row kept state: %s", dumps(t, row, json.Options{}))
		}
		if _, has := row.Get("chunks"); has {
			t.Fatalf("expired row kept chunks: %s", dumps(t, row, json.Options{}))
		}
	}
	assertRows(t, rows[4:], mustEntries(t, "s", nil)[4:]...)
	for _, row := range mustEntries(t, "s", limit(0))[2:] {
		if _, has := row.Get("state"); has {
			t.Fatalf("cache_limit=0 kept state: %s", dumps(t, row, json.Options{}))
		}
	}
}

// Python measures the tracemalloc peak while reading. Go has no peak counter, so this measures the heap
// that the returned rows retain; the reader holds one line at a time.
//
// py: tests/test_ledger.py::test_optional_review_payloads_do_not_accumulate_in_memory
func TestOptionalReviewPayloadsDoNotAccumulateInMemory(t *testing.T) {
	useState(t)
	for index := range 100 {
		mustAppend(t, json.NewObject("session_id", "s", "gate", "edit",
			"state", json.NewObject("padding", strings.Repeat("x", 65_536)), "tool_use_id", strconv.Itoa(index)))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rows := mustEntries(t, "s", limit(2))
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	if len(rows) != 100 {
		t.Fatalf("len(rows) = %d, want 100", len(rows))
	}
	var withState []any
	for _, row := range rows {
		if _, has := row.Get("state"); has {
			withState = append(withState, row.Value("tool_use_id"))
		}
	}
	if !equals(t, withState, []any{"98", "99"}) {
		t.Fatalf("rows with state = %v, want [98 99]", withState)
	}
	if retained >= 3_000_000 {
		t.Fatalf("rows retain %d bytes, want < 3000000", retained)
	}
	runtime.KeepAlive(rows)
}

// py: tests/test_ledger.py::test_round_trip_and_damaged_line_is_skipped
func TestRoundTripAndDamagedLineIsSkipped(t *testing.T) {
	state := useState(t)
	mustAppend(t, object(t, `{"session_id": "s1", "gate": "decide"}`))
	appendText(t, filepath.Join(state, "sessions", "s1.jsonl"), `{"broken`)
	mustAppend(t, object(t, `{"session_id": "s1", "gate": "stop"}`))
	var gates []any
	for _, row := range mustEntries(t, "s1", nil) {
		gates = append(gates, row.Value("gate"))
	}
	if !equals(t, gates, []any{"decide"}) {
		t.Fatalf("gates = %v, want [decide]", gates)
	}
	assertRows(t, mustEntries(t, "other", nil))
}

// appendWorkerEnv names the worker of a TestAppendManyHelper process.
const appendWorkerEnv = "LEDGER_TEST_APPEND_WORKER"

// TestAppendManyHelper is the worker process of TestParallelAppendsNeverInterleave.
//
// py: tests/test_ledger.py::_append_many
func TestAppendManyHelper(t *testing.T) {
	worker := os.Getenv(appendWorkerEnv)
	if worker == "" {
		t.Skip("runs only as a worker process of TestParallelAppendsNeverInterleave")
	}
	for i := range 50 {
		mustAppend(t, json.NewObject("session_id", "s", "worker", json.Int(worker), "i", json.IntOf(int64(i)),
			"pad", strings.Repeat("x", 5_000)))
	}
}

// py: tests/test_ledger.py::test_parallel_appends_never_interleave
func TestParallelAppendsNeverInterleave(t *testing.T) {
	useState(t)
	workers := make([]*exec.Cmd, 8)
	outputs := make([]bytes.Buffer, 8)
	for worker := range workers {
		command := exec.Command(os.Args[0], "-test.run=^TestAppendManyHelper$")
		command.Env = append(os.Environ(), appendWorkerEnv+"="+strconv.Itoa(worker))
		command.Stdout, command.Stderr = &outputs[worker], &outputs[worker]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		workers[worker] = command
	}
	for worker, command := range workers {
		if err := command.Wait(); err != nil {
			t.Fatalf("worker %d: %v\n%s", worker, err, outputs[worker].String())
		}
	}

	rows := mustEntries(t, "s", nil)
	if len(rows) != 400 {
		t.Fatalf("len(rows) = %d, want 400", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[dumps(t, []any{row.Value("worker"), row.Value("i")}, json.Options{})] = true
	}
	for worker := range 8 {
		for i := range 50 {
			if pair := dumps(t, []any{worker, i}, json.Options{}); !seen[pair] {
				t.Fatalf("missing row %s", pair)
			}
		}
	}
	if len(seen) != 400 {
		t.Fatalf("distinct rows = %d, want 400", len(seen))
	}
	key, err := keyFile()
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := policy.StateDir(); pathlib.Parent(key) != state {
		t.Fatalf("key parent = %q, want %q", pathlib.Parent(key), state)
	}
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("key mode %v size %d, want 0600 and 32", info.Mode().Perm(), info.Size())
	}
}

// py: tests/test_ledger.py::test_signed_entries_do_not_expose_internal_seal_fields
func TestSignedEntriesDoNotExposeInternalSealFields(t *testing.T) {
	useState(t)
	entry := object(t, `{"session_id": "s1", "gate": "decide", "action": "advisory", "tool_use_id": "toolu_1"}`)
	mustAppend(t, entry)
	rows := mustEntries(t, "s1", nil)
	assertRows(t, rows, entry)
	for _, key := range rows[0].Keys() {
		if strings.HasPrefix(key, "_ledger") {
			t.Fatalf("entry exposes %q", key)
		}
	}
}

// py: tests/test_ledger.py::test_tampered_signed_decision_entry_is_ignored
func TestTamperedSignedDecisionEntryIsIgnored(t *testing.T) {
	state := useState(t)
	mustAppend(t, object(t, `{"session_id": "s1", "gate": "decide", "action": "advisory", "pick": "lru",
		"tool_use_id": "toolu_1", "agent_id": "agent_a"}`))
	path := filepath.Join(state, "sessions", "s1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	row := object(t, strings.SplitN(string(data), "\n", 2)[0])
	target := row
	if entry, has := row.Get("entry"); has {
		target = entry.(*json.Object)
	}
	target.Set("action", "binding")
	target.Set("pick", "lfu")
	target.Set("tool_use_id", "toolu_2")
	target.Set("agent_id", "agent_b")
	if err := os.WriteFile(path, []byte(dumps(t, row, json.Options{SortKeys: true})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRows(t, mustEntries(t, "s1", nil))
	assertRows(t, mustEntries(t, "s1", limit(0)))
}

// py: tests/test_ledger.py::test_unsigned_entry_is_ignored
func TestUnsignedEntryIsIgnored(t *testing.T) {
	state := useState(t)
	mustAppend(t, object(t, `{"session_id": "s1", "gate": "decide", "action": "advisory"}`))
	appendText(t, filepath.Join(state, "sessions", "s1.jsonl"),
		dumps(t, object(t, `{"session_id": "s1", "gate": "decide", "action": "binding"}`), json.Options{})+"\n")
	var actions []any
	for _, row := range mustEntries(t, "s1", nil) {
		actions = append(actions, row.Value("action"))
	}
	if !equals(t, actions, []any{"advisory"}) {
		t.Fatalf("actions = %v, want [advisory]", actions)
	}
}

// py: tests/test_ledger.py::test_signed_entry_moved_to_another_session_is_ignored
func TestSignedEntryMovedToAnotherSessionIsIgnored(t *testing.T) {
	state := useState(t)
	mustAppend(t, object(t, `{"session_id": "s1", "gate": "decide", "action": "advisory", "tool_use_id": "toolu_1"}`))
	data, err := os.ReadFile(filepath.Join(state, "sessions", "s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "sessions", "s2.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	assertRows(t, mustEntries(t, "s2", nil))
	if rows := mustEntries(t, "s1", nil); len(rows) != 1 {
		t.Fatalf("len(s1 rows) = %d, want 1", len(rows))
	}
}

// py: tests/test_ledger.py::test_corrupt_key_does_not_authorize_existing_entries
func TestCorruptKeyDoesNotAuthorizeExistingEntries(t *testing.T) {
	useState(t)
	mustAppend(t, object(t, `{"session_id": "s1", "gate": "decide", "action": "advisory"}`))
	key, err := keyFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRows(t, mustEntries(t, "s1", nil))
}

// py: tests/test_ledger.py::test_verdict_file_stays_inside_the_state_directory
func TestVerdictFileStaysInsideTheStateDirectory(t *testing.T) {
	state := useState(t)
	path, err := WriteVerdict("../../etc/x", "verdict text")
	if err != nil {
		t.Fatal(err)
	}
	if pathlib.Parent(path) != filepath.Join(state, "verdicts") {
		t.Fatalf("verdict parent = %q, want %q", pathlib.Parent(path), filepath.Join(state, "verdicts"))
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "verdict text" {
		t.Fatalf("verdict = %q, %v", data, err)
	}
	if filepath.Base(path) != VerdictName("../../etc/x") {
		t.Fatalf("verdict name = %q, want %q", filepath.Base(path), VerdictName("../../etc/x"))
	}
}

// py: tests/test_ledger.py::test_prune_removes_only_old_files
func TestPruneRemovesOnlyOldFiles(t *testing.T) {
	state := useState(t)
	mustAppend(t, object(t, `{"session_id": "old"}`))
	mustAppend(t, object(t, `{"session_id": "new"}`))
	oldVerdict, err := WriteVerdict("old", "v")
	if err != nil {
		t.Fatal(err)
	}
	makeOld(t, filepath.Join(state, "sessions", "old.jsonl"), oldVerdict)
	if err := Prune(); err != nil {
		t.Fatal(err)
	}
	assertRows(t, mustEntries(t, "old", nil))
	if _, err := os.Stat(oldVerdict); !os.IsNotExist(err) {
		t.Fatalf("old verdict still exists: %v", err)
	}
	if rows := mustEntries(t, "new", nil); len(rows) != 1 {
		t.Fatalf("len(new rows) = %d, want 1", len(rows))
	}
}

// py: tests/test_ledger.py::test_prune_without_state_directory_is_a_no_op
func TestPruneWithoutStateDirectoryIsANoOp(t *testing.T) {
	useState(t)
	if err := Prune(); err != nil {
		t.Fatal(err)
	}
}

// py: tests/test_ledger.py::test_installation_recipe_survives_session_retention
func TestInstallationRecipeSurvivesSessionRetention(t *testing.T) {
	state := useState(t)
	entry := json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "codex", "state_root", state, "servers", &json.Object{}))
	mustAppend(t, entry)
	makeOld(t, filepath.Join(state, "sessions", "installation.jsonl"))
	if err := Prune(); err != nil {
		t.Fatal(err)
	}
	assertRows(t, mustEntries(t, "installation", nil), entry)
}

// The Python test builds the entry with install.installation_entry; only its host and
// session_review_exceptions fields matter to policy.session_review_exception.
//
// py: tests/test_session_exceptions.py::test_tampered_owner_policy_cannot_extend_exception
func TestTamperedOwnerPolicyCannotExtendException(t *testing.T) {
	state := useState(t)
	mustAppend(t, object(t, `{"session_id": "installation", "gate": "installation", "action": "configured",
		"spec": {"host": "codex", "session_review_exceptions": ["approved-sdk"]}}`))
	path := filepath.Join(state, "sessions", "installation.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	row := object(t, strings.SplitN(string(data), "\n", 2)[0])
	row.Value("entry").(*json.Object).Value("spec").(*json.Object).Set("session_review_exceptions", []any{"other-session"})
	appendText(t, path, dumps(t, row, json.Options{})+"\n")
	for session, want := range map[string]bool{"other-session": false, "approved-sdk": true} {
		if got, err := SessionReviewException("codex", session); err != nil || got != want {
			t.Fatalf("SessionReviewException(codex, %s) = %v, %v; want %v", session, got, err, want)
		}
	}
}

// FailOpen holds only for the host that a signed installation entry puts in availability mode.
func TestFailOpenReadsTheSignedHostPolicy(t *testing.T) {
	useState(t)
	mustAppend(t, object(t, `{"session_id": "installation", "gate": "installation", "action": "configured",
		"spec": {"host": "claude", "review_mode": "availability"}}`))
	for host, want := range map[string]bool{"claude": true, "codex": false} {
		if got, err := FailOpen(host); err != nil || got != want {
			t.Fatalf("FailOpen(%s) = %v, %v; want %v", host, got, err, want)
		}
	}
}

// Entries splits lines as a Python text file does: "\r", "\n" and "\r\n" all end a line, and the
// form feed and U+2028 that str.splitlines also splits on do not.
func TestEntriesSplitsUniversalNewlines(t *testing.T) {
	state := useState(t)
	for _, gate := range []string{"a", "b", "c"} {
		mustAppend(t, json.NewObject("session_id", "s", "gate", gate))
	}
	path := filepath.Join(state, "sessions", "s.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	text := lines[0] + "\r" + lines[1] + "\r\n" + lines[2] + "\f" + lines[0] + "\u2028" + lines[0] + "\n" + lines[2]
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var gates []string
	for _, row := range mustEntries(t, "s", nil) {
		gates = append(gates, row.Value("gate").(string))
	}
	if !slices.Equal(gates, []string{"a", "b", "c"}) {
		t.Fatalf("gates = %v, want [a b c]", gates)
	}
}

// dumps returns json.Dumps(v, o) and fails the test on an error.
func dumps(t *testing.T, v any, o json.Options) string {
	t.Helper()
	text, err := json.Dumps(v, o)
	if err != nil {
		t.Fatalf("json.Dumps: %v", err)
	}
	return text
}

// equals returns json.Equal(a, b) and fails the test on an error.
func equals(t *testing.T, a, b any) bool {
	t.Helper()
	same, err := json.Equal(a, b)
	if err != nil {
		t.Fatalf("json.Equal: %v", err)
	}
	return same
}
