package subq

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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

func TestMalformedBatchCancelsItsSiblingWithoutPartialAnswers(t *testing.T) {
	completed := make(chan struct{})
	invalidClosed := make(chan struct{})
	peerStarted := make(chan struct{})
	peerCause := make(chan error, 1)
	client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
		body, err := requestObject(request)
		if err != nil {
			return nil, err
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
