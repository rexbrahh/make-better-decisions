package hook

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// --- tests/test_jev.py ContextLimitTest: the tests that review through hook._asker(post)

// ucLimitContext ports ContextLimitTest.context.
func ucLimitContext(t *testing.T) gates.Context {
	t.Helper()
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

// ucRecorder records the request bodies of concurrent posts.
type ucRecorder struct {
	mu       sync.Mutex
	requests []*json.Object
}

func (r *ucRecorder) record(t *testing.T, body []byte) (*json.Object, int) {
	request := obj(t, string(body))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	return request, len(r.requests)
}

func (r *ucRecorder) all() []*json.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*json.Object(nil), r.requests...)
}

var ucContextLimit = &jev.JevError{Reason: "http_400", Detail: "max_tokens_exceeded"}

// ucTransport ports ContextLimitTest.transport without the pause, which only the jev test uses.
func ucTransport(t *testing.T, alwaysFail bool) (jev.PostFunc, *ucRecorder) {
	r := &ucRecorder{}
	api := fake(nil)
	return func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		if _, n := r.record(t, body); n == 1 || alwaysFail {
			return nil, ucContextLimit
		}
		return api.Post(body, key, timeout)
	}, r
}

// ucEditGate runs gates.edit_gate(name, toolInput, ctx, hook._asker(post)).
func ucEditGate(t *testing.T, name string, toolInput *json.Object, ctx gates.Context, post jev.PostFunc) gates.Outcome {
	t.Helper()
	outcome, err := gates.EditGate(name, toolInput, ctx, asker(post, nil, editAsk), gateFrame)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func ucAssertAction(t *testing.T, outcome gates.Outcome, action string) {
	t.Helper()
	if outcome.Action != action {
		t.Fatalf("got %s (%q), want %s", outcome.Action, outcome.Message, action)
	}
}

func ucAssertRequests(t *testing.T, requests []*json.Object, n int) {
	t.Helper()
	if len(requests) != n {
		t.Fatalf("%d requests, want %d", len(requests), n)
	}
}

// ucWithout returns {k: v for k, v in object.items() if k != key}.
func ucWithout(object *json.Object, key string) *json.Object {
	result := object.Copy()
	result.Delete(key)
	return result
}

// ucDeepCopy returns copy.deepcopy(v).
func ucDeepCopy(t *testing.T, v any) any {
	return loads(t, dumps(t, v))
}

// py: tests/test_jev.py::test_retry_preserves_authority_and_records_actual_native_read_state
func TestRetryPreservesAuthorityAndRecordsActualNativeReadState(t *testing.T) {
	setUp(t)
	ctx := ucLimitContext(t)
	original := ucDeepCopy(t, ctx.Evidence).(*json.Object)
	post, r := ucTransport(t, false)
	outcome := ucEditGate(t, "mcp__codex_app__read_thread", json.NewObject("threadId", "peer"), ctx, post)
	ucAssertAction(t, outcome, "allow")
	requests := r.all()
	ucAssertRequests(t, requests, 2)
	assertEqual(t, outcome.Record.Value("state"), requests[1].Value("state"))
	assertEqual(t, ctx.Evidence, original)
	first, second := requests[0].Value("state").(*json.Object), requests[1].Value("state").(*json.Object)
	assertEqual(t, ucWithout(first, "evidence"), ucWithout(second, "evidence"))
	evidence := second.Value("evidence").(*json.Object)
	for _, name := range []string{"user_messages", "brief", "contract_observations"} {
		assertEqual(t, evidence.Value(name), original.Value(name))
	}
	assertEqual(t, evidence.Value("source_files").([]any)[1:], original.Value("source_files").([]any)[1:])
	call := at(t, evidence, "tool_calls_newest_first", 0).(*json.Object)
	assertEqual(t, ucWithout(call, "output"), ucWithout(at(t, original, "tool_calls_newest_first", 0).(*json.Object), "output"))
	assertContains(t, str(t, call, "output"), "characters cut")
	if at(t, evidence, "source_files", 0, "truncated") != true {
		t.Fatal("the helper source was not truncated")
	}
	if cut := at(t, evidence, "metadata", "context_limit_chars_cut"); !json.Truthy(cut) {
		t.Fatalf("context_limit_chars_cut %v", cut)
	}
}

// py: tests/test_jev.py::test_retry_failure_still_denies_with_exact_last_attempt_state
func TestRetryFailureStillDeniesWithExactLastAttemptState(t *testing.T) {
	setUp(t)
	post, r := ucTransport(t, true)
	outcome := ucEditGate(t, "mcp__codex_app__wait_threads", obj(t, `{"timeoutMs": 0}`), ucLimitContext(t), post)
	ucAssertAction(t, outcome, "deny")
	requests := r.all()
	ucAssertRequests(t, requests, 2)
	assertEqual(t, outcome.Record.Value("state"), requests[1].Value("state"))
	for _, part := range []string{"max_tokens_exceeded", "attempt_count=2", "user_messages="} {
		assertContains(t, outcome.Message, part)
	}
	assertNotContains(t, outcome.Message, "Mandatory rules")
}

// py: tests/test_jev.py::test_peer_dispatch_recovers_when_the_old_first_reduction_is_still_too_large
func TestPeerDispatchRecoversWhenTheOldFirstReductionIsStillTooLarge(t *testing.T) {
	setUp(t)
	ctx := ucLimitContext(t)
	original := ucDeepCopy(t, ctx.Evidence).(*json.Object)
	r := &ucRecorder{}
	api := fake(nil)
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		r.record(t, body)
		if len(body) > 27000 {
			return nil, ucContextLimit
		}
		return api.Post(body, key, timeout)
	}
	pending := obj(t, `{"task_name": "v45_trim_failure_trace", "model": "gpt-6-luna",
		"reasoning_effort": "max", "fork_turns": "none",
		"message": "Read-only source trace. Do not edit files or execute remote commands."}`)
	outcome := ucEditGate(t, "spawn_agent", pending, ctx, post)
	ucAssertAction(t, outcome, "allow")
	requests := r.all()
	ucAssertRequests(t, requests, 2)
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
	ctx := ucLimitContext(t)
	original := ucDeepCopy(t, ctx.Evidence).(*json.Object)
	r := &ucRecorder{}
	api := fake(nil)
	trimmed := func(request *json.Object) bool {
		_, ok := at(t, request, "state", "evidence", "metadata").(*json.Object).Get("context_limit_chars_cut")
		return ok
	}
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		if request, _ := r.record(t, body); !trimmed(request) {
			return nil, ucContextLimit
		}
		return api.Post(body, key, timeout)
	}
	pending := json.NewObject("file_path", "test.py", "old_string", strings.Repeat("x", 20000),
		"new_string", strings.Repeat("y", 20000))
	outcome := ucEditGate(t, "Edit", pending, ctx, post)
	ucAssertAction(t, outcome, "allow")
	assertEqual(t, ctx.Evidence, original)
	var reviewed []any
	for _, request := range r.all() {
		if trimmed(request) {
			reviewed = append(reviewed, request.Value("state"))
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
