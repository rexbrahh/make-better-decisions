package jev

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// setUp ports tests/helpers.py StateTestCase.setUp: a private state directory and a test key.
func setUp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_DECISIONS_STATE", filepath.Join(dir, "state"))
	t.Setenv("TYPESAFE_API_KEY", "test-key-0123456789abcdef")
	return dir
}

func questions() *json.Object {
	return json.NewObject(
		"ok", json.NewObject("type", "noul", "instructions", "Is it ok?"),
		"pick", json.NewObject("type", "choice", "instructions", "Which?",
			"criteria", json.NewObject("a", "A", "b", "B")))
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

// deepCopy ports copy.deepcopy for decoded JSON values.
func deepCopy(t *testing.T, v any) any {
	t.Helper()
	return loads(t, dumps(t, v))
}

func assertEqual(t *testing.T, got, want any) {
	t.Helper()
	if equal, err := json.Equal(got, want); err != nil || !equal {
		t.Fatalf("got %s, want %s (%v)", dumps(t, got), dumps(t, want), err)
	}
}

func assertJevError(t *testing.T, err error, reason string) *JevError {
	t.Helper()
	var jevError *JevError
	if !errors.As(err, &jevError) {
		t.Fatalf("got %v, want a JevError", err)
	}
	if jevError.Reason != reason {
		t.Fatalf("got reason %q (%v), want %q", jevError.Reason, err, reason)
	}
	return jevError
}

// goodAnswers returns json.loads(FakeJev()(json.dumps({"questions": QUESTIONS}).encode(), "k", 1))["answers"].
func goodAnswers(t *testing.T) *json.Object {
	t.Helper()
	raw, err := new(jevtest.Fake).Post([]byte(dumps(t, json.NewObject("questions", questions()))), "k", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return loads(t, string(raw)).(*json.Object).Value("answers").(*json.Object)
}

// with returns a copy of object with key set to value: Python's {**object, key: value}.
func with(object *json.Object, key string, value any) *json.Object {
	result := object.Copy()
	result.Set(key, value)
	return result
}

// py: tests/test_jev.py::test_roundoff_at_existing_distribution_tolerance_is_accepted
func TestRoundoffAtExistingDistributionToleranceIsAccepted(t *testing.T) {
	setUp(t)
	for _, total := range []float64{0.99, 1.01} {
		chosen := .65
		other := total - chosen // float64 arithmetic, as in Python: Go constant arithmetic is exact
		raw := dumps(t, json.NewObject("answers", json.NewObject("pick", json.NewObject(
			"type", "choice", "choice", "a", "confidence", .65,
			"probabilities", json.NewObject("a", chosen, "b", other)))))
		fake := &jevtest.Fake{Raw: []byte(raw)}
		pick := json.NewObject("pick", questions().Value("pick"))
		if _, err := Ask(&json.Object{}, pick, "k", fake.Post, nil); err != nil {
			t.Errorf("total %v: %v", total, err)
		}
	}
}

// py: tests/test_jev.py::test_malformed_diagnostic_names_the_check_without_response_text
func TestMalformedDiagnosticNamesTheCheckWithoutResponseText(t *testing.T) {
	setUp(t)
	fake := &jevtest.Fake{Raw: []byte(`{"answers":{"ok":{"type":"noul","noul":"private-response-text"}}}`)}
	_, err := Ask(&json.Object{}, json.NewObject("ok", questions().Value("ok")), "k", fake.Post, nil)
	assertJevError(t, err, "malformed")
	if !strings.Contains(err.Error(), "ok: probability") || strings.Contains(err.Error(), "private-response-text") {
		t.Fatalf("got %q", err)
	}
}

// py: tests/test_jev.py::test_numeric_fields_and_choice_distribution_are_validated
func TestNumericFieldsAndChoiceDistributionAreValidated(t *testing.T) {
	setUp(t)
	good := goodAnswers(t)
	type answerCase struct {
		id     string
		answer *json.Object
	}
	var cases []answerCase
	for _, value := range []any{"0.9", true, nil, loads(t, "NaN")} {
		cases = append(cases, answerCase{"ok", json.NewObject("type", "noul", "noul", value)})
	}
	base := good.Value("pick").(*json.Object)
	for _, value := range []string{
		`{"a": "0.9", "b": 0.1}`, `{"a": true, "b": 0.0}`, `{"a": 1.0}`,
		`{"a": 1.0, "b": -0.1}`, `{"a": 0.8, "b": 0.8}`, `{"a": 0.1, "b": 0.9}`,
		`{"a": 0.65, "b": 0.3399}`, `{"a": 0.65, "b": 0.3601}`,
	} {
		cases = append(cases, answerCase{"pick", with(base, "probabilities", loads(t, value))})
	}
	for _, value := range []any{"0.9", true, 2.0} {
		cases = append(cases, answerCase{"pick", with(base, "confidence", value)})
	}
	for _, c := range cases {
		raw := dumps(t, json.NewObject("answers", with(good, c.id, c.answer)))
		_, err := Ask(&json.Object{}, questions(), "k", (&jevtest.Fake{Raw: []byte(raw)}).Post, nil)
		var jevError *JevError
		if !errors.As(err, &jevError) || jevError.Reason != "malformed" {
			t.Errorf("answer %s: got %v, want malformed", dumps(t, c.answer), err)
		}
	}
}

// py: tests/test_jev.py::test_sends_model_state_questions_and_returns_answers
func TestSendsModelStateQuestionsAndReturnsAnswers(t *testing.T) {
	setUp(t)
	fake := &jevtest.Fake{Answers: map[string]any{"ok": 0.9, "pick": "b"}}
	answers, err := Ask(json.NewObject("x", json.Int("1")), questions(), "k", fake.Post, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := json.NewObject("model", policy.Model, "state", json.NewObject("x", json.Int("1")), "questions", questions())
	if requests := fake.Requests(); len(requests) != 1 {
		t.Fatalf("got %d requests", len(requests))
	}
	assertEqual(t, fake.Requests()[0], want)
	assertEqual(t, answers.Value("ok").(*json.Object).Value("noul"), 0.9)
	assertEqual(t, answers.Value("pick").(*json.Object).Value("choice"), "b")
}

func TestValidateResponsePreservesJevValidationAndMetrics(t *testing.T) {
	setUp(t)
	requested := questions()
	wantAnswers := goodAnswers(t)
	wantUsage := json.NewObject("input_tokens", json.Int("120"), "output_tokens", json.Int("4"))
	raw := []byte(dumps(t, json.NewObject("model", policy.Model, "answers", wantAnswers, "usage", wantUsage)))
	metrics := &json.Object{}
	answers, err := ValidateResponse(raw, requested, metrics)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, answers, wantAnswers)
	assertEqual(t, metrics.Value("model"), policy.Model)
	assertEqual(t, metrics.Value("usage"), wantUsage)

	missing := dumps(t, json.NewObject("model", policy.Model,
		"answers", json.NewObject("ok", wantAnswers.Value("ok"))))
	_, err = ValidateResponse([]byte(missing), requested, nil)
	assertJevError(t, err, "malformed")
}

// py: tests/test_jev.py::test_malformed_answers_raise
func TestMalformedAnswersRaise(t *testing.T) {
	setUp(t)
	good := goodAnswers(t)
	bad := map[string]string{
		"not json":         "<html>",
		"no answers":       "{}",
		"missing question": dumps(t, json.NewObject("answers", json.NewObject("ok", good.Value("ok")))),
		"wrong type":       dumps(t, json.NewObject("answers", with(good, "ok", good.Value("pick")))),
		"out of range":     dumps(t, json.NewObject("answers", with(good, "ok", json.NewObject("type", "noul", "noul", 1.5)))),
		"choice outside criteria": dumps(t, json.NewObject("answers", with(good, "pick", json.NewObject(
			"type", "choice", "choice", "z", "probabilities", json.NewObject("z", 1.0))))),
	}
	for name, raw := range bad {
		_, err := Ask(&json.Object{}, questions(), "k", (&jevtest.Fake{Raw: []byte(raw)}).Post, nil)
		var jevError *JevError
		if !errors.As(err, &jevError) || jevError.Reason != "malformed" {
			t.Errorf("%s: got %v, want malformed", name, err)
		}
	}
}

// py: tests/test_jev.py::test_env_then_file_then_no_key
func TestEnvThenFileThenNoKey(t *testing.T) {
	dir := setUp(t)
	if key, err := LoadKey(); err != nil || key != "test-key-0123456789abcdef" {
		t.Fatalf("got %q, %v", key, err)
	}
	os.Unsetenv("TYPESAFE_API_KEY") // setUp's t.Setenv restores it
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patchKeyFile(t, path)
	if key, err := LoadKey(); err != nil || key != "file-key" {
		t.Fatalf("got %q, %v", key, err)
	}
	patchKeyFile(t, filepath.Join(dir, "missing"))
	_, err := LoadKey()
	assertJevError(t, err, "no_key")
}

func patchKeyFile(t *testing.T, path string) {
	original := keyFile
	keyFile = func() (string, error) { return path, nil }
	t.Cleanup(func() { keyFile = original })
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// patchTransport stands in for mock.patch("urllib.request.urlopen").
func patchTransport(t *testing.T, f roundTripper) {
	original := transport
	transport = f
	t.Cleanup(func() { transport = original })
}

func respond(request *http.Request, status int, body io.Reader) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(body), Request: request}
}

func postReason(t *testing.T, f roundTripper) string {
	t.Helper()
	patchTransport(t, f)
	_, err := Post([]byte("{}"), "k", time.Second)
	var jevError *JevError
	if !errors.As(err, &jevError) {
		t.Fatalf("got %v, want a JevError", err)
	}
	return jevError.Reason
}

func status(code int, body string) roundTripper {
	return func(request *http.Request) (*http.Response, error) {
		return respond(request, code, strings.NewReader(body)), nil
	}
}

func failure(err error) roundTripper {
	return func(*http.Request) (*http.Response, error) { return nil, err }
}

// py: tests/test_jev.py::test_reports_observed_context_limit_code_without_echoing_error_body
func TestReportsObservedContextLimitCodeWithoutEchoingErrorBody(t *testing.T) {
	setUp(t)
	// Exact detail shape returned by the live oversized Jev 1.13 probe.
	patchTransport(t, status(400, `{"detail":{"error_type":"max_tokens_exceeded","state":"private-text"}}`))
	_, err := Post([]byte("{}"), "k", time.Second)
	jevError := assertJevError(t, err, "http_400")
	if jevError.Detail != "max_tokens_exceeded" || strings.Contains(err.Error(), "private-text") {
		t.Fatalf("got %q", err)
	}
}

// py: tests/test_jev.py::test_unknown_validation_details_are_not_echoed
func TestUnknownValidationDetailsAreNotEchoed(t *testing.T) {
	setUp(t)
	if reason := postReason(t, status(400, `{"detail":{"error_type":"private-error-text"}}`)); reason != "http_400" {
		t.Fatalf("got %q", reason)
	}
}

type slowBody struct{ done chan struct{} }

func (body slowBody) Read([]byte) (int, error) {
	defer close(body.done)
	time.Sleep(250 * time.Millisecond)
	return 0, io.EOF
}

// py: tests/test_jev.py::test_deadline_includes_reading_the_response_body
func TestDeadlineIncludesReadingTheResponseBody(t *testing.T) {
	setUp(t)
	done := make(chan struct{})
	patchTransport(t, func(request *http.Request) (*http.Response, error) {
		return respond(request, 200, slowBody{done: done}), nil
	})
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("response body did not finish before transport cleanup")
		}
	})
	start := time.Now()
	_, err := Post([]byte("{}"), "k", 50*time.Millisecond)
	assertJevError(t, err, "timeout")
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("took %v", elapsed)
	}
}

// py: tests/test_jev.py::test_maps_transport_errors
func TestMapsTransportErrors(t *testing.T) {
	setUp(t)
	for _, c := range []struct {
		name      string
		transport roundTripper
		reason    string
	}{
		{"401", status(401, ""), "http_401"},
		{"429", status(429, ""), "http_429"},
		{"529", status(529, ""), "http_529"},
		{"TimeoutError", failure(os.ErrDeadlineExceeded), "timeout"},
		{"URLError(refused)", failure(errors.New("refused")), "unreachable"},
	} {
		if reason := postReason(t, c.transport); reason != c.reason {
			t.Errorf("%s: got %q, want %q", c.name, reason, c.reason)
		}
	}
}

// py: tests/test_jev.py::test_sends_bearer_key_and_never_puts_it_in_the_body
func TestSendsBearerKeyAndNeverPutsItInTheBody(t *testing.T) {
	setUp(t)
	var auth, body string
	var timeout time.Duration
	patchTransport(t, func(request *http.Request) (*http.Response, error) {
		auth = request.Header.Get("Authorization")
		data, _ := io.ReadAll(request.Body)
		body = string(data)
		deadline, _ := request.Context().Deadline()
		timeout = time.Until(deadline).Round(time.Second)
		return respond(request, 200, strings.NewReader(`{"answers": {}}`)), nil
	})
	raw, err := Post([]byte(`{"state": 1}`), "secret-key", 3*time.Second)
	if err != nil || string(raw) != `{"answers": {}}` {
		t.Fatalf("got %q, %v", raw, err)
	}
	if auth != "Bearer secret-key" || body != `{"state": 1}` || timeout != 3*time.Second {
		t.Fatalf("got auth %q, body %q, timeout %v", auth, body, timeout)
	}
}

// contextLimitEvidence ports ContextLimitTest.context(): make_ctx(host="codex").evidence with oversized history.
func contextLimitEvidence(t *testing.T) *json.Object {
	t.Helper()
	bundle, err := evidence.Build(transcript.Transcript{UserMessages: []string{"decide the cache policy"}}, nil,
		evidence.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bundle.Set("user_messages", []any{"Preserve the guard.", "fix it asap"})
	bundle.Set("brief", "Keep the required review policy.")
	bundle.Set("tool_calls_newest_first", []any{json.NewObject("id", "native-read", "tool", "exec_command",
		"input", "sed -n '1,400p' scripts/common.sh", "output", strings.Repeat("diagnostic evidence\n", 800),
		"trust", "unverified_tool_observation", "execution", json.NewObject("cwd", "/other/worktree"))})
	bundle.Set("source_files", []any{
		json.NewObject("path", "scripts/common.sh", "text", strings.Repeat("helper source\n", 1000),
			"sha256", "helper-hash", "trust", "verified_file_observation", "truncated", false),
		json.NewObject("path", "AGENTS.md", "text", strings.Repeat("Mandatory rules\n", 500), "sha256", "rules-hash"),
		json.NewObject("path", "contract.md", "text", strings.Repeat("Mandatory contract\n", 500),
			"sha256", "contract-hash")})
	bundle.Set("contract_observations", json.NewObject("contract.md", json.NewObject("sha256", "contract-hash")))
	return bundle
}

// py: tests/test_jev.py::test_peer_history_inputs_shrink_without_changing_current_dispatch_or_authority
func TestPeerHistoryInputsShrinkWithoutChangingCurrentDispatchOrAuthority(t *testing.T) {
	setUp(t)
	bundle := contextLimitEvidence(t)
	bundle.Set("user_messages", []any{strings.Repeat("Preserve every required instruction.\n", 1000)})
	bundle.Set("source_files", []any{json.NewObject("path", "AGENTS.md", "text", strings.Repeat("Required contract.\n", 1000),
		"sha256", "rules-hash", "trust", "verified_file_observation")})
	calls := []any{json.NewObject("id", "peer-verdict", "tool", "mcp__codex_app__send_message_to_thread",
		"input", strings.Repeat("Previously observed planning verdict.\n", 260), "output", "Delivered",
		"trust", "unverified_tool_observation", "execution", json.NewObject("cwd", "/other/worktree"))}
	for i := range 7 {
		calls = append(calls, json.NewObject("id", fmt.Sprintf("dispatch-%d", i), "tool", "followup_task",
			"input", strings.Repeat("Earlier factual dispatch.\n", 80), "output", "Completed",
			"trust", "unverified_tool_observation"))
	}
	bundle.Set("tool_calls_newest_first", calls)
	pending := json.NewObject("target", "v45_preserve_mac_evidence", "message", "Record the factual journal checkpoint.")
	state := json.NewObject("pending_tool", "agentsfollowup_task", "pending_input", pending,
		"decisions", []any{json.NewObject("selected_approach", strings.Repeat("Completed scoped decision.\n", 400))},
		"evidence", bundle)
	original := deepCopy(t, state).(*json.Object)
	var requests []*json.Object
	fake := new(jevtest.Fake)
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		requests = append(requests, loads(t, string(body)).(*json.Object))
		if len(body) > 76000 {
			return nil, &JevError{Reason: "http_400", Detail: "max_tokens_exceeded"}
		}
		return fake.Post(body, key, timeout)
	}
	if _, err := Ask(state, questions(), "k", post, nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests", len(requests))
	}
	second := requests[1].Value("state").(*json.Object)
	for _, field := range []string{"pending_tool", "pending_input", "decisions"} {
		assertEqual(t, second.Value(field), original.Value(field))
	}
	secondEvidence := second.Value("evidence").(*json.Object)
	originalEvidence := original.Value("evidence").(*json.Object)
	for _, field := range []string{"user_messages", "source_files", "brief", "contract_observations"} {
		assertEqual(t, secondEvidence.Value(field), originalEvidence.Value(field))
	}
	before := originalEvidence.Value("tool_calls_newest_first").([]any)
	after := secondEvidence.Value("tool_calls_newest_first").([]any)
	for i := range min(len(before), len(after)) {
		b, a := before[i].(*json.Object), after[i].(*json.Object)
		for _, field := range []string{"id", "trust", "execution"} {
			assertEqual(t, a.Value(field), b.Value(field))
		}
		assertEqual(t, a.Value("input_truncated"), true)
		assertEqual(t, a.Value("input_original_chars"), json.IntOf(int64(len(b.Value("input").(string)))))
		if !strings.Contains(a.Value("input").(string), "characters cut") {
			t.Errorf("call %d input was not cut", i)
		}
	}
	assertEqual(t, bundle, originalEvidence)
}

// py: tests/test_jev.py::test_retry_uses_remaining_deadline
func TestRetryUsesRemainingDeadline(t *testing.T) {
	setUp(t)
	var requests int
	var timeouts []time.Duration
	fake := new(jevtest.Fake)
	post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
		requests++
		timeouts = append(timeouts, timeout)
		if requests == 1 {
			time.Sleep(20 * time.Millisecond)
			return nil, &JevError{Reason: "http_400", Detail: "max_tokens_exceeded"}
		}
		return fake.Post(body, key, timeout)
	}
	metrics := &json.Object{}
	if _, err := Ask(json.NewObject("evidence", contextLimitEvidence(t)), questions(), "k", post, metrics); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || timeouts[1] >= timeouts[0]-10*time.Millisecond {
		t.Fatalf("got %d requests with timeouts %v", requests, timeouts)
	}
	assertEqual(t, metrics.Value("attempt_count"), json.Int("2"))
	initial, _ := metrics.Value("initial_request_bytes").(json.Int).Int64()
	final, _ := metrics.Value("request_bytes").(json.Int).Int64()
	if final >= initial {
		t.Fatalf("request_bytes %d not below initial_request_bytes %d", final, initial)
	}
}

// py: tests/test_jev.py::test_unrecognized_errors_and_unshortenable_state_are_not_retried
func TestUnrecognizedErrorsAndUnshortenableStateAreNotRetried(t *testing.T) {
	setUp(t)
	for _, raised := range []*JevError{{Reason: "http_400"}, {Reason: "unreachable"}, {Reason: "malformed"},
		{Reason: "http_400", Detail: "max_tokens_exceeded"}} {
		fake := &jevtest.Fake{Raises: raised}
		_, err := Ask(json.NewObject("policy", "Keep every instruction."), questions(), "k", fake.Post, nil)
		var jevError *JevError
		if !errors.As(err, &jevError) || len(fake.Requests()) != 1 {
			t.Errorf("%v: got %v after %d requests", raised, err, len(fake.Requests()))
		}
	}
}

// py: tests/test_metrics.py::test_client_retains_usage_without_changing_model_state
func TestClientRetainsUsageWithoutChangingModelState(t *testing.T) {
	setUp(t)
	ok := json.NewObject("ok", json.NewObject("type", "noul", "instructions", "Is this supported?"))
	usage := json.NewObject("input_tokens", json.Int("120"), "output_tokens", json.Int("4"))
	response := json.NewObject("model", policy.Model,
		"answers", json.NewObject("ok", json.NewObject("type", "noul", "noul", .8)), "usage", usage)
	fake := &jevtest.Fake{Raw: []byte(dumps(t, response))}
	metrics := &json.Object{}
	if _, err := Ask(json.NewObject("evidence", &json.Object{}), ok, "k", fake.Post, metrics); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, metrics.Value("usage"), usage)
	assertEqual(t, metrics.Value("model"), policy.Model)
	if requestBytes, _ := metrics.Value("request_bytes").(json.Int).Int64(); requestBytes <= 0 {
		t.Fatalf("request_bytes %d", requestBytes)
	}
	if apiMS, _ := metrics.Value("api_ms").(float64); apiMS < 0 {
		t.Fatalf("api_ms %v", apiMS)
	}
	if _, ok := fake.Requests()[0].Value("state").(*json.Object).Get("metrics"); ok {
		t.Fatal("metrics reached the request state")
	}
}

// The error texts below reach the hook only through their class name, but the oracle checks them too.
func TestLatin1EncodeErrorsMatchPython(t *testing.T) {
	for _, c := range []struct{ key, want string }{
		{"café", ""},
		{"k中x", "'latin-1' codec can't encode character '\\u4e2d' in position 8: ordinal not in range(256)"},
		{"\U0001f600中", "'latin-1' codec can't encode characters in position 7-8: ordinal not in range(256)"},
	} {
		_, err := latin1("Bearer " + c.key)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.key, got, c.want)
		}
	}
}

func TestFsumRoundsHalfEvenAcrossPartials(t *testing.T) {
	for _, c := range []struct {
		values []float64
		want   float64
	}{
		{[]float64{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1}, 1.0},
		{[]float64{1e100, 1.0, -1e100, 1e-100, 1e50, -1.0, -1e50}, 1e-100},
		{[]float64{1, 0x1p-53, 0x1p-106}, 1 + 0x1p-52}, // half-even correction
		{nil, 0.0},
	} {
		if got := fsum(c.values); got != c.want {
			t.Errorf("fsum(%v) = %v, want %v", c.values, got, c.want)
		}
	}
	if ulpOne != math.Nextafter(1, 2)-1 {
		t.Fatal(ulpOne)
	}
}
