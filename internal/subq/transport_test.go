package subq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

type observedBody struct {
	io.ReadCloser
	received chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func (body *observedBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if body.received != nil && count > 0 {
		body.once.Do(func() { close(body.received) })
	}
	return count, err
}

func (body *observedBody) Close() error {
	err := body.ReadCloser.Close()
	close(body.closed)
	return err
}

func TestAskTransportFailurePhases(t *testing.T) {
	for _, phase := range []string{"request", "response_body"} {
		t.Run(phase, func(t *testing.T) {
			wrappedNetworkReason := "timeout"
			if phase == "request" {
				// http.Client's url.Error checks only its immediate error for timeout status.
				wrappedNetworkReason = "unreachable"
			}
			for _, failure := range []struct {
				name, reason string
				err          error
			}{
				{"unreachable", "unreachable", errors.New("fixture-key-only private transport detail")},
				{"cancelled", "cancelled", context.Canceled},
				{"deadline", "timeout", context.DeadlineExceeded},
				{"network timeout", "timeout", &net.DNSError{Err: "private transport detail", IsTimeout: true}},
				{"wrapped cancelled", "cancelled", fmt.Errorf("private transport detail: %w", context.Canceled)},
				{"wrapped deadline", "timeout", fmt.Errorf("private transport detail: %w", context.DeadlineExceeded)},
				{"wrapped network timeout", wrappedNetworkReason, fmt.Errorf("private transport detail: %w", &net.DNSError{IsTimeout: true})},
				{"joined cancellation first", "cancelled", errors.Join(context.DeadlineExceeded, context.Canceled)},
			} {
				t.Run(failure.name, func(t *testing.T) {
					var requests atomic.Int32
					closed := make(chan struct{})
					client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
						if requests.Add(1) > 1 {
							return responseFor(request, policy.SubQModel, nil, nil)
						}
						if phase == "request" {
							return nil, failure.err
						}
						reply := response(request, http.StatusOK, nil)
						reply.Body = &observedBody{ReadCloser: io.NopCloser(iotest.ErrReader(failure.err)), closed: closed}
						return reply, nil
					}))
					metrics := json.NewObject("returned_model", "stale-model")
					answers, err := client.Ask(json.NewObject(), testQuestions(1), "fixture-key-only", metrics)
					je := errorReason(t, err, failure.reason)
					if je.Phase != phase {
						t.Errorf("failure phase = %q, want %q", je.Phase, phase)
					}
					if je.Detail != "" || je.Error() != failure.reason {
						t.Fatalf("failure exposed transport detail: %q", je.Error())
					}
					if answers != nil || metrics.Value("returned_model") != nil {
						t.Fatal("failed operation returned answers or a validated model")
					}
					if requests.Load() != 1 || len(client.slots) != 0 {
						t.Fatal("failed operation retried or retained a request slot")
					}
					if phase == "response_body" {
						select {
						case <-closed:
						default:
							t.Fatal("failed operation did not close its response body")
						}
					}
					answers, err = client.Ask(json.NewObject(), testQuestions(1), "fixture-key-only", metrics)
					if err != nil || answers == nil || answers.Len() != 1 || metrics.Value("returned_model") != policy.SubQModel {
						t.Fatalf("independent operation did not recover: %v", err)
					}
					if requests.Load() != 2 || len(client.slots) != 0 {
						t.Fatal("recovery retried or retained a request slot")
					}
				})
			}
		})
	}
}

func TestAskAdmissionFailuresHaveNoTransportPhase(t *testing.T) {
	for _, mode := range []string{"cancelled", "deadline", "slots"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests atomic.Int32
			client := testClient(ctx, roundTripper(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected transport call")
			}))
			want := "timeout"
			switch mode {
			case "cancelled":
				cancel()
				want = "cancelled"
			case "deadline":
				deadlineCtx, deadlineCancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer deadlineCancel()
				client.ctx = deadlineCtx
			case "slots":
				for range cap(client.slots) {
					client.slots <- struct{}{}
				}
				client.timeout = 20 * time.Millisecond
			}
			answers, err := client.Ask(json.NewObject(), testQuestions(1), "fixture-key-only", nil)
			if je := errorReason(t, err, want); je.Phase != "" {
				t.Fatalf("admission failure has transport phase %q", je.Phase)
			}
			if answers != nil || requests.Load() != 0 {
				t.Fatal("admission failure returned answers or reached transport")
			}
		})
	}
}

func TestAskRejectsHTTPFailuresBeforeReadingAnswers(t *testing.T) {
	for _, status := range []int{302, 400, 401, 402, 403, 422, 429, 500, 503} {
		for _, bodyKind := range []string{"valid_answer", "error_body"} {
			t.Run(strconv.Itoa(status)+"/"+bodyKind, func(t *testing.T) {
				received, closed := make(chan struct{}), make(chan struct{})
				var requests atomic.Int32
				client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
					if requests.Add(1) != 1 {
						return nil, errors.New("unexpected HTTP error retry")
					}
					var reply *http.Response
					if bodyKind == "valid_answer" {
						var err error
						reply, err = responseFor(request, policy.SubQModel, nil, nil)
						if err != nil {
							return nil, err
						}
						reply.StatusCode = status
					} else {
						reply = response(request, status, []byte(`{"detail":{"error_type":"fixture-key-only","message":"private fixture body"}}`))
					}
					if status == http.StatusFound {
						reply.Header.Set("Location", "https://collector.invalid/steal")
					}
					reply.Body = &observedBody{ReadCloser: reply.Body, received: received, closed: closed}
					return reply, nil
				}))
				metrics := json.NewObject("returned_model", "stale-model")
				answers, err := client.Ask(json.NewObject(), testQuestions(1), "fixture-key-only", metrics)
				if answers != nil {
					t.Fatal("HTTP failure returned answers")
				}
				want := "http_" + strconv.Itoa(status)
				if je := errorReason(t, err, want); je.Error() != want || je.Phase != "" {
					t.Fatalf("HTTP failure exposed response detail or a transport phase: %q, %q", je.Error(), je.Phase)
				}
				if requests.Load() != 1 {
					t.Fatalf("HTTP failure made %d requests, want one", requests.Load())
				}
				select {
				case <-received:
					t.Fatal("HTTP failure read its response body")
				default:
				}
				select {
				case <-closed:
				default:
					t.Fatal("HTTP failure did not close its response body")
				}
				if metrics.Value("returned_model") != nil {
					t.Fatal("HTTP failure recorded a validated returned model")
				}
			})
		}
	}
}

func TestMalformedBatchCancelsItsSiblingWithoutPartialAnswers(t *testing.T) {
	completed := make(chan struct{})
	invalidClosed := make(chan struct{})
	peerStarted := make(chan struct{})
	peerCause := make(chan error, 1)
	recoveryReady := make(chan struct{})
	var recoveryRequests atomic.Int32
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		body, err := requestObject(request)
		if err != nil {
			return nil, err
		}
		if body.Value("state").(*json.Object).Value("recovery") == true {
			if recoveryRequests.Add(1) == 4 {
				close(recoveryReady)
			}
			select {
			case <-recoveryReady:
				return responseFor(request, policy.SubQModel, nil, nil)
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		batch := body.Value("questions").(*json.Object)
		switch batch.Keys()[0] {
		case "q00":
			reply, err := responseFor(request, policy.SubQModel, nil, nil)
			if err == nil {
				reply.Body = &observedBody{ReadCloser: reply.Body, closed: completed}
			}
			return reply, err
		case "q16":
			for _, ready := range []<-chan struct{}{completed, peerStarted} {
				select {
				case <-ready:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
			}
			reply, err := responseFor(request, policy.SubQModel, nil, func(_ *json.Object, answers *json.Object) {
				answers.Delete("q16")
			})
			if err == nil {
				reply.Body = &observedBody{ReadCloser: reply.Body, closed: invalidClosed}
			}
			return reply, err
		default:
			close(peerStarted)
			<-request.Context().Done()
			peerCause <- request.Context().Err()
			return nil, request.Context().Err()
		}
	}))
	client.timeout = time.Second
	metrics := json.NewObject()
	answers, err := client.Ask(json.NewObject(), testQuestions(33), "test-key", metrics)
	if answers != nil {
		t.Fatalf("malformed batch returned %d partial answers", answers.Len())
	}
	errorReason(t, err, "malformed")
	select {
	case cause := <-peerCause:
		if !errors.Is(cause, context.Canceled) {
			t.Fatalf("sibling stopped because of %v, want cancellation after malformed response", cause)
		}
	default:
		t.Fatal("Ask returned before its sibling stopped")
	}
	for _, closed := range []<-chan struct{}{completed, invalidClosed} {
		select {
		case <-closed:
		default:
			t.Fatal("Ask returned before a response body closed")
		}
	}
	if metrics.Value("returned_model") != nil {
		t.Fatal("incomplete review recorded a validated returned model")
	}
	metrics = json.NewObject()
	answers, err = client.Ask(json.NewObject("recovery", true), testQuestions(64), "test-key", metrics)
	if err != nil {
		t.Fatalf("shared client did not recover all request slots: %v", err)
	}
	if answers == nil || answers.Len() != 64 {
		t.Fatal("shared client did not return every recovery answer")
	}
	if metrics.Value("returned_model") != policy.SubQModel {
		t.Fatal("successful recovery did not record its validated model")
	}
}

func TestAskFailureDoesNotCancelConcurrentCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	failedClosed := make(chan struct{})
	independentClosed := make(chan struct{})
	var requests atomic.Int32
	client := testClient(ctx, roundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		body, err := requestObject(request)
		if err != nil {
			return nil, err
		}
		closed := independentClosed
		var mutate func(*json.Object, *json.Object)
		if body.Value("state").(*json.Object).Value("failing") == true {
			closed = failedClosed
			mutate = func(_ *json.Object, answers *json.Object) { answers.Delete("q00") }
		} else {
			started <- request.Context()
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		reply, err := responseFor(request, policy.SubQModel, nil, mutate)
		if err == nil {
			reply.Body = &observedBody{ReadCloser: reply.Body, closed: closed}
		}
		return reply, err
	}))
	client.timeout = 2 * time.Second
	type result struct {
		answers *json.Object
		err     error
	}
	done := make(chan result, 1)
	independentMetrics := json.NewObject()
	var workers sync.WaitGroup
	workers.Add(1)
	defer func() {
		cancel()
		workers.Wait()
	}()
	go func() {
		defer workers.Done()
		answers, err := client.Ask(json.NewObject(), testQuestions(16), "test-key", independentMetrics)
		done <- result{answers, err}
	}()
	var independentContext context.Context
	select {
	case independentContext = <-started:
	case <-time.After(time.Second):
		t.Fatal("independent call did not start")
	}

	failedMetrics := json.NewObject()
	answers, err := client.Ask(json.NewObject("failing", true), testQuestions(1), "test-key", failedMetrics)
	if answers != nil {
		t.Fatal("failed call returned answers")
	}
	errorReason(t, err, "malformed")
	if failedMetrics.Value("returned_model") != nil {
		t.Fatal("failed call recorded a validated model")
	}
	if err := independentContext.Err(); err != nil {
		t.Fatalf("failed call canceled the independent call: %v", err)
	}
	select {
	case got := <-done:
		t.Fatalf("independent call finished before release: %v", got.err)
	default:
	}
	close(release)
	select {
	case got := <-done:
		if got.err != nil || got.answers == nil || got.answers.Len() != 16 {
			t.Fatalf("independent call lost its answers: answers=%v error=%v", got.answers, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent call did not finish after release")
	}
	if independentMetrics.Value("returned_model") != policy.SubQModel || requests.Load() != 2 {
		t.Fatal("concurrent calls did not retain independent outcomes without retries")
	}
	for _, closed := range []<-chan struct{}{failedClosed, independentClosed} {
		select {
		case <-closed:
		default:
			t.Fatal("Ask returned before its response body closed")
		}
	}
}

func TestAskCancelsResponseBodyWithoutPartialAnswers(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("local port binding is unavailable here: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			release := make(chan struct{})
			serverDone := make(chan struct{})
			firstClosed := make(chan struct{})
			secondReceived := make(chan struct{})
			secondClosed := make(chan struct{})
			transport := &http.Transport{}
			server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				body, err := requestObject(request)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				batch := body.Value("questions").(*json.Object)
				if batch.Keys()[0] == "q00" {
					reply, err := responseFor(request, policy.SubQModel, nil, nil)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					defer reply.Body.Close()
					io.Copy(w, reply.Body)
					return
				}
				defer close(serverDone)
				io.WriteString(w, `{"model":`)
				w.(http.Flusher).Flush()
				select {
				case <-request.Context().Done():
				case <-release:
				}
			})}}
			server.Start()
			askDone := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				close(release)
				server.CloseClientConnections()
				server.Close()
				transport.CloseIdleConnections()
				select {
				case <-askDone:
				case <-time.After(2 * time.Second):
					t.Error("Ask worker did not finish during cleanup")
				}
			})
			targetHost := listener.Addr().String()
			client := testClient(ctx, roundTripper(func(request *http.Request) (*http.Response, error) {
				body, err := requestObject(request)
				if err != nil {
					return nil, err
				}
				batch := body.Value("questions").(*json.Object)
				local := request.Clone(request.Context())
				local.URL.Scheme, local.URL.Host = "http", targetHost
				reply, err := transport.RoundTrip(local)
				if err != nil {
					return nil, err
				}
				if batch.Keys()[0] == "q00" {
					reply.Body = &observedBody{ReadCloser: reply.Body, closed: firstClosed}
				} else {
					reply.Body = &observedBody{ReadCloser: reply.Body, received: secondReceived, closed: secondClosed}
				}
				return reply, nil
			}))
			if mode == "deadline" {
				client.timeout = 3 * time.Second
			}
			result := make(chan error, 1)
			readyBy := time.Now().Add(2 * time.Second)
			readyTimer := time.NewTimer(time.Until(readyBy))
			defer readyTimer.Stop()
			go func() {
				defer close(askDone)
				answers, err := client.Ask(json.NewObject(), testQuestions(17), "test-key", nil)
				if answers != nil {
					err = errors.New("canceled operation returned partial answers")
				}
				result <- err
			}()
			for _, event := range []struct {
				name   string
				signal <-chan struct{}
			}{
				{"the first response body closing", firstClosed},
				{"data from the second response body", secondReceived},
			} {
				select {
				case <-event.signal:
				case <-readyTimer.C:
					t.Fatalf("Did not observe %s before the setup deadline", event.name)
				}
				if !time.Now().Before(readyBy) {
					t.Fatalf("Did not observe %s before the setup deadline", event.name)
				}
			}
			want := "timeout"
			if mode == "cancel" {
				want = "cancelled"
				cancel()
			}
			select {
			case err := <-result:
				errorReason(t, err, want)
			case <-time.After(4 * time.Second):
				t.Fatal("Ask did not stop reading the response body")
			}
			for _, done := range []<-chan struct{}{secondClosed, serverDone} {
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("response body or HTTP handler did not close")
				}
			}
		})
	}
}
