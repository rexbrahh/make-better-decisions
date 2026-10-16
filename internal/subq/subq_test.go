package subq

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testClient(ctx context.Context, transport http.RoundTripper) *Client {
	if ctx == nil {
		ctx = context.Background()
	}
	provider, _ := policy.Provider("subq")
	return &Client{ctx: ctx, http: &http.Client{Transport: transport, CheckRedirect: rejectRedirect},
		slots: make(chan struct{}, maxConcurrentRequests), timeout: provider.Timeout}
}

func response(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}
}

func testQuestions(count int) *json.Object {
	questions := &json.Object{}
	for i := range count {
		questions.Set(fmtQuestion(i), json.NewObject("type", "noul", "instructions", "Check this."))
	}
	return questions
}

func fmtQuestion(index int) string {
	const digits = "0123456789"
	return "q" + string([]byte{digits[index/10%10], digits[index%10]})
}

func requestObject(request *http.Request) (*json.Object, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	value, err := json.Loads(string(body))
	if err != nil {
		return nil, err
	}
	object, ok := value.(*json.Object)
	if !ok {
		return nil, errors.New("request is not an object")
	}
	return object, nil
}

func responseFor(request *http.Request, model string, usage *json.Object,
	mutate func(*json.Object, *json.Object)) (*http.Response, error) {
	requestBody, err := requestObject(request)
	if err != nil {
		return nil, err
	}
	questions, ok := requestBody.Value("questions").(*json.Object)
	if !ok {
		return nil, errors.New("questions is not an object")
	}
	answers := &json.Object{}
	for _, id := range questions.Keys() {
		question, ok := questions.Value(id).(*json.Object)
		if !ok {
			return nil, errors.New("question is not an object")
		}
		switch question.Value("type") {
		case "noul":
			answers.Set(id, json.NewObject("type", "noul", "noul", .8))
		case "choice":
			criteria, ok := question.Value("criteria").(*json.Object)
			if !ok || criteria.Len() == 0 {
				return nil, errors.New("choice question has no criteria")
			}
			probabilities := &json.Object{}
			for _, candidate := range criteria.Keys() {
				probabilities.Set(candidate, 0.0)
			}
			choice := criteria.Keys()[0]
			probabilities.Set(choice, 1.0)
			answers.Set(id, json.NewObject("type", "choice", "choice", choice,
				"confidence", 1.0, "probabilities", probabilities))
		default:
			return nil, errors.New("unknown question type")
		}
	}
	result := json.NewObject("model", model, "answers", answers)
	if usage != nil {
		result.Set("usage", usage)
	}
	if mutate != nil {
		mutate(questions, answers)
	}
	raw, err := json.Dumps(result, json.Options{})
	if err != nil {
		return nil, err
	}
	return response(request, http.StatusOK, []byte(raw)), nil
}

func errorReason(t *testing.T, err error, want string) *jev.JevError {
	t.Helper()
	var got *jev.JevError
	if !errors.As(err, &got) {
		t.Fatalf("got %v, want JevError %q", err, want)
	}
	if got.Reason != want {
		t.Fatalf("got reason %q (%v), want %q", got.Reason, err, want)
	}
	return got
}

func TestAskBatchesInOrderAndRetainsEveryAnswer(t *testing.T) {
	state := json.NewObject("immutable", "same state for each batch")
	questions := testQuestions(17)
	originalState, _ := json.Dumps(state, json.Options{})
	originalQuestions, _ := json.Dumps(questions, json.Options{})
	var mu sync.Mutex
	var requests []*json.Object
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		body, err := requestObject(request)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		return responseFor(request, policy.SubQModel, nil, nil)
	}))

	answers, err := client.Ask(state, questions, "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Len() != 17 {
		t.Fatalf("got %d answers, want 17", answers.Len())
	}
	if got, _ := json.Dumps(state, json.Options{}); got != originalState {
		t.Fatalf("state changed: %s", got)
	}
	if got, _ := json.Dumps(questions, json.Options{}); got != originalQuestions {
		t.Fatalf("questions changed: %s", got)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	byFirstQuestion := map[string]*json.Object{}
	for _, request := range requests {
		if keys := strings.Join(request.Keys(), ","); keys != "model,state,questions" {
			t.Fatalf("unexpected request fields %q", keys)
		}
		batch := request.Value("questions").(*json.Object)
		if batch.Len() > 16 {
			t.Fatalf("batch has %d questions", batch.Len())
		}
		byFirstQuestion[batch.Keys()[0]] = batch
		gotState, _ := json.Dumps(request.Value("state"), json.Options{})
		if gotState != originalState {
			t.Fatalf("batch state changed: %s", gotState)
		}
	}
	first, last := byFirstQuestion["q00"], byFirstQuestion["q16"]
	if first == nil || first.Len() != 16 || last == nil || last.Len() != 1 {
		t.Fatalf("unexpected batches: first=%v last=%v", first, last)
	}
	for i, id := range first.Keys() {
		if want := fmtQuestion(i); id != want {
			t.Fatalf("batch question %d = %q, want %q", i, id, want)
		}
	}
	if got := last.Keys()[0]; got != "q16" {
		t.Fatalf("last batch question = %q, want q16", got)
	}
}

func TestAskRejectsAnEmptyQuestionSet(t *testing.T) {
	client := testClient(context.Background(), roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("empty questions reached the API")
		return nil, nil
	}))
	answers, err := client.Ask(json.NewObject(), json.NewObject(), "test-key", nil)
	if answers != nil {
		t.Fatal("empty review returned answers")
	}
	errorReason(t, err, "malformed")
}

func TestAskSharesFourRequestLimitAcrossConcurrentCalls(t *testing.T) {
	var active, maximum atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return responseFor(request, policy.SubQModel, nil, nil)
	}))

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := client.Ask(json.NewObject(), testQuestions(64), "test-key", nil)
			results <- err
		}()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("client did not start four requests")
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Ask did not join all request workers")
		}
	}
	if got := maximum.Load(); got != 4 {
		t.Fatalf("maximum active requests = %d, want 4", got)
	}
}

func TestAskCancellationReturnsNoPartialAnswers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstAnswered := make(chan struct{})
	secondStarted := make(chan struct{}, 1)
	var requests atomic.Int32
	client := testClient(ctx, roundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		body, err := requestObject(request)
		if err != nil {
			return nil, err
		}
		batch := body.Value("questions").(*json.Object)
		if batch.Keys()[0] == "q00" {
			response, err := responseFor(request, policy.SubQModel, nil, nil)
			close(firstAnswered)
			return response, err
		}
		secondStarted <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))

	result := make(chan struct {
		answers *json.Object
		err     error
	}, 1)
	go func() {
		answers, err := client.Ask(json.NewObject(), testQuestions(17), "test-key", nil)
		result <- struct {
			answers *json.Object
			err     error
		}{answers, err}
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second batch did not start")
	}
	select {
	case <-firstAnswered:
	case <-time.After(time.Second):
		t.Fatal("first batch did not answer")
	}
	cancel()
	select {
	case got := <-result:
		if got.answers != nil {
			t.Fatalf("returned %d partial answers", got.answers.Len())
		}
		errorReason(t, got.err, "cancelled")
	case <-time.After(time.Second):
		t.Fatal("Ask did not join canceled workers")
	}
	if requests.Load() != 2 {
		t.Fatalf("got %d requests, want 2", requests.Load())
	}
}

func TestAskUsesParentDeadlineAndReturnsNoPartialAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	var deadlinesMu sync.Mutex
	var deadlines []time.Time
	client := testClient(ctx, roundTripper(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			return nil, errors.New("request has no deadline")
		}
		deadlinesMu.Lock()
		deadlines = append(deadlines, deadline)
		deadlinesMu.Unlock()
		body, err := requestObject(request)
		if err != nil {
			return nil, err
		}
		if body.Value("questions").(*json.Object).Keys()[0] == "q00" {
			time.Sleep(10 * time.Millisecond)
			return responseFor(request, policy.SubQModel, nil, nil)
		}
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	answers, err := client.Ask(json.NewObject(), testQuestions(17), "test-key", nil)
	if answers != nil {
		t.Fatalf("returned %d partial answers", answers.Len())
	}
	errorReason(t, err, "timeout")
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("batch deadlines differ: %v", deadlines)
	}
}

func TestAskCapsDeadlineAtTenSeconds(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var remaining time.Duration
	client := testClient(parent, roundTripper(func(request *http.Request) (*http.Response, error) {
		deadline, _ := request.Context().Deadline()
		remaining = deadline.Sub(time.Now())
		return responseFor(request, policy.SubQModel, nil, nil)
	}))
	if _, err := client.Ask(json.NewObject(), testQuestions(1), "key", nil); err != nil {
		t.Fatal(err)
	}
	if remaining < 9*time.Second || remaining > 10*time.Second {
		t.Fatalf("request deadline is %v from Ask start, want at most 10s", remaining)
	}
}

func TestAskRejectsMissingInvalidAndWrongModelResponses(t *testing.T) {
	choiceQuestion := json.NewObject("type", "choice", "instructions", "Choose.", "criteria", json.NewObject("a", "A", "b", "B"))
	for _, test := range []struct {
		name     string
		question *json.Object
		model    string
		mutate   func(*json.Object, *json.Object)
	}{
		{"missing answer", json.NewObject("type", "noul", "instructions", "Check."), policy.SubQModel,
			func(_ *json.Object, answers *json.Object) { answers.Delete("q00") }},
		{"invalid noul", json.NewObject("type", "noul", "instructions", "Check."), policy.SubQModel,
			func(_ *json.Object, answers *json.Object) {
				answers.Set("q00", json.NewObject("type", "noul", "noul", 1.5))
			}},
		{"invalid choice distribution", choiceQuestion, policy.SubQModel,
			func(_ *json.Object, answers *json.Object) {
				answers.Set("q00", json.NewObject("type", "choice", "choice", "a", "confidence", .8,
					"probabilities", json.NewObject("a", .8, "b", .8)))
			}},
		{"wrong model", json.NewObject("type", "noul", "instructions", "Check."), "other-model", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			questions := json.NewObject("q00", test.question)
			client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
				return responseFor(request, test.model, nil, test.mutate)
			}))
			answers, err := client.Ask(json.NewObject(), questions, "test-key", nil)
			if answers != nil {
				t.Fatalf("returned %d answers for invalid response", answers.Len())
			}
			je := errorReason(t, err, "malformed")
			if strings.Contains(je.Error(), "other-model") {
				t.Fatalf("diagnostic exposed model text: %q", je.Error())
			}
		})
	}
}

func TestAskRejectsRedirectWithoutForwardingKey(t *testing.T) {
	var seen int
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		seen++
		if got := request.Header.Get("Authorization"); got != "Bearer private-key" {
			return nil, errors.New("initial authorization missing")
		}
		redirect := response(request, http.StatusFound, []byte("private redirect body"))
		redirect.Header.Set("Location", "https://collector.invalid/steal")
		return redirect, nil
	}))
	answers, err := client.Ask(json.NewObject(), testQuestions(1), "private-key", nil)
	if answers != nil {
		t.Fatal("redirect returned answers")
	}
	je := errorReason(t, err, "http_302")
	if seen != 1 {
		t.Fatalf("transport saw %d requests, want only the original request", seen)
	}
	if strings.Contains(je.Error(), "private-key") || strings.Contains(je.Error(), "private redirect body") {
		t.Fatalf("diagnostic leaked private data: %q", je.Error())
	}
}

func TestAskSanitizesTransportErrors(t *testing.T) {
	client := testClient(context.Background(), roundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private-key transport diagnostic")
	}))
	answers, err := client.Ask(json.NewObject(), testQuestions(1), "private-key", nil)
	if answers != nil {
		t.Fatal("transport error returned answers")
	}
	je := errorReason(t, err, "unreachable")
	if strings.Contains(je.Error(), "private-key") || strings.Contains(je.Error(), "transport diagnostic") {
		t.Fatalf("diagnostic leaked transport detail: %q", je.Error())
	}
}

func TestAskEnforcesRequestAndResponseSizeLimits(t *testing.T) {
	var requests atomic.Int32
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return response(request, http.StatusOK, bytes.Repeat([]byte("x"), maxResponseBytes+1)), nil
	}))
	answers, err := client.Ask(json.NewObject("large", strings.Repeat("x", maxRequestBytes)), testQuestions(1), "k", nil)
	if answers != nil {
		t.Fatal("oversized request returned answers")
	}
	errorReason(t, err, "request_too_large")
	if requests.Load() != 0 {
		t.Fatalf("oversized request reached transport %d times", requests.Load())
	}

	answers, err = client.Ask(json.NewObject(), testQuestions(1), "k", nil)
	if answers != nil {
		t.Fatal("oversized response returned answers")
	}
	je := errorReason(t, err, "response_too_large")
	if strings.Contains(je.Error(), "xxxxxxxx") {
		t.Fatalf("diagnostic exposed response body: %q", je.Error())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}
}

func TestLoadKeyUsesEnvironmentThenInjectedFile(t *testing.T) {
	original := keyFile
	t.Cleanup(func() { keyFile = original })
	path := filepath.Join(t.TempDir(), "test-subq-key")
	if err := os.WriteFile(path, []byte(" file-key \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var fileLookups atomic.Int32
	keyFile = func() (string, error) {
		fileLookups.Add(1)
		return path, nil
	}
	t.Setenv("SUBQ_API_KEY", " env-key ")
	if key, err := LoadKey(); err != nil || key != "env-key" || fileLookups.Load() != 0 {
		t.Fatalf("environment key = %q, %v; lookups=%d", key, err, fileLookups.Load())
	}
	t.Setenv("SUBQ_API_KEY", "")
	if key, err := LoadKey(); err != nil || key != "file-key" || fileLookups.Load() != 1 {
		t.Fatalf("file key = %q, %v; lookups=%d", key, err, fileLookups.Load())
	}
	keyFile = func() (string, error) { return filepath.Join(t.TempDir(), "missing"), nil }
	_, err := LoadKey()
	errorReason(t, err, "no_key")
}

func TestNewClientKeepsFixedEndpointAndVerifiedTransport(t *testing.T) {
	t.Setenv("SUBQ_API_URL", "https://override.invalid")
	client := NewClient(context.Background())
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.http.Transport)
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS certificate verification is disabled")
	}
	var endpoint string
	client.http.Transport = roundTripper(func(request *http.Request) (*http.Response, error) {
		endpoint = request.URL.String()
		return responseFor(request, policy.SubQModel, nil, nil)
	})
	if _, err := client.Ask(json.NewObject(), testQuestions(1), "key", nil); err != nil {
		t.Fatal(err)
	}
	if endpoint != policy.SubQURL {
		t.Fatalf("request endpoint = %q, want fixed policy endpoint %q", endpoint, policy.SubQURL)
	}
}

func TestAskAggregatesUsageOutsideUnchangedState(t *testing.T) {
	state := json.NewObject("decision", "Preserve this value.")
	questions := testQuestions(17)
	originalState, _ := json.Dumps(state, json.Options{})
	originalQuestions, _ := json.Dumps(questions, json.Options{})
	var requestSizesMu sync.Mutex
	requestSizes := []int{}
	var batch atomic.Int32
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		requestSizesMu.Lock()
		requestSizes = append(requestSizes, int(request.ContentLength))
		requestSizesMu.Unlock()
		index := batch.Add(1)
		return responseFor(request, policy.SubQModel, json.NewObject(
			"input_tokens", json.IntOf(int64(index*10)),
			"output_tokens", json.IntOf(int64(index)),
			"total_tokens", json.IntOf(int64(index*11))), nil)
	}))
	metrics := &json.Object{}
	answers, err := client.Ask(state, questions, "test-key", metrics)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Len() != 17 {
		t.Fatalf("got %d answers", answers.Len())
	}
	if got, _ := json.Dumps(state, json.Options{}); got != originalState {
		t.Fatalf("state changed: %s", got)
	}
	if got, _ := json.Dumps(questions, json.Options{}); got != originalQuestions {
		t.Fatalf("questions changed: %s", got)
	}
	if metrics.Value("provider") != "subq" || metrics.Value("requested_model") != policy.SubQModel ||
		metrics.Value("returned_model") != policy.SubQModel || metrics.Value("batch_count") != json.Int("2") {
		t.Fatalf("unexpected provider metrics: %s", mustDump(t, metrics))
	}
	usage, ok := metrics.Value("usage").(*json.Object)
	if !ok || usage.Value("input_tokens") != json.Int("30") || usage.Value("output_tokens") != json.Int("3") ||
		usage.Value("total_tokens") != json.Int("33") {
		t.Fatalf("unexpected summed usage: %s", mustDump(t, metrics))
	}
	if sizes, ok := metrics.Value("request_sizes").([]any); !ok || len(sizes) != 2 {
		t.Fatalf("unexpected request sizes: %s", mustDump(t, metrics))
	}
	if elapsed, ok := metrics.Value("api_ms").(float64); !ok || elapsed < 0 {
		t.Fatalf("unexpected API time: %s", mustDump(t, metrics))
	}
	requestSizesMu.Lock()
	defer requestSizesMu.Unlock()
	if len(requestSizes) != 2 {
		t.Fatalf("got %d request sizes", len(requestSizes))
	}
	for _, size := range requestSizes {
		if size <= 0 || size > maxRequestBytes {
			t.Fatalf("request size %d outside bound", size)
		}
	}
}

func mustDump(t *testing.T, value any) string {
	t.Helper()
	text, err := json.Dumps(value, json.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return text
}
