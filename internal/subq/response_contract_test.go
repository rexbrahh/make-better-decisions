package subq

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

func TestAskBoundsAndClosesResponseBodies(t *testing.T) {
	valid := []byte(mustDump(t, json.NewObject("model", policy.SubQModel, "answers",
		json.NewObject("q00", json.NewObject("type", "noul", "noul", .8)))))
	for _, test := range []struct {
		name      string
		size      int
		readError error
		wantError string
	}{
		{"at_limit", maxResponseBytes, nil, ""},
		{"one_byte_over", maxResponseBytes + 1, nil, "response_too_large"},
		{"beyond_limit", maxResponseBytes + 1024, nil, "response_too_large"},
		{"unexpected_eof", len(valid), io.ErrUnexpectedEOF, "unreachable"},
		{"reader_error", len(valid), errors.New("fixture-key-only: response-only-diagnostic"), "unreachable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := append(bytes.Clone(valid), bytes.Repeat([]byte(" "), test.size-len(valid))...)
			measured := bytes.NewReader(raw)
			var reader io.Reader = measured
			if test.readError != nil {
				reader = io.MultiReader(reader, iotest.ErrReader(test.readError))
			}
			closed := make(chan struct{})
			ready := make(chan struct{})
			var requests, recoveryRequests atomic.Int32
			client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
				body, err := requestObject(request)
				if err != nil {
					return nil, err
				}
				if body.Value("state").(*json.Object).Value("recovery") == true {
					if recoveryRequests.Add(1) == 4 {
						close(ready)
					}
					select {
					case <-ready:
						return responseFor(request, policy.SubQModel, nil, nil)
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
				}
				if requests.Add(1) != 1 {
					return nil, errors.New("unexpected response-body retry")
				}
				reply := response(request, http.StatusOK, nil)
				reply.Body = &observedBody{ReadCloser: io.NopCloser(reader), closed: closed}
				return reply, nil
			}))
			client.timeout = 2 * time.Second
			metrics := json.NewObject("returned_model", "stale-model")
			answers, err := client.Ask(json.NewObject(), testQuestions(1), "fixture-key-only", metrics)
			if test.wantError == "" {
				if err != nil || answers == nil || answers.Len() != 1 {
					t.Fatalf("valid response at the limit: answers=%v error=%v", answers, err)
				}
				if metrics.Value("returned_model") != policy.SubQModel {
					t.Fatal("valid response did not record its model")
				}
			} else {
				if answers != nil {
					t.Fatal("failed response returned answers")
				}
				if got := errorReason(t, err, test.wantError).Error(); got != test.wantError {
					t.Fatalf("response failure exposed detail: %q", got)
				}
				if metrics.Value("returned_model") != nil {
					t.Fatal("failed response retained a validated model")
				}
			}
			if got, want := len(raw)-measured.Len(), min(len(raw), maxResponseBytes+1); got != want {
				t.Fatalf("read %d bytes, want %d", got, want)
			}
			select {
			case <-closed:
			default:
				t.Fatal("Ask returned before closing its response body")
			}
			if requests.Load() != 1 {
				t.Fatalf("transport requests=%d, want one", requests.Load())
			}
			recovered, err := client.Ask(json.NewObject("recovery", true), testQuestions(64), "fixture-key-only", nil)
			if err != nil || recovered == nil || recovered.Len() != 64 || recoveryRequests.Load() != 4 {
				t.Fatalf("shared client recovery: answers=%v requests=%d error=%v", recovered, recoveryRequests.Load(), err)
			}
		})
	}
}

func TestAskRejectsMissingAndNonStringResponseModels(t *testing.T) {
	for _, test := range []struct {
		name  string
		model any
	}{
		{"missing", nil},
		{"null", nil},
		{"boolean", true},
		{"number", json.Int("1")},
		{"array", []any{policy.SubQModel}},
		{"object", json.NewObject("name", policy.SubQModel)},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := json.NewObject("model", test.model, "answers",
				json.NewObject("q00", json.NewObject("type", "noul", "noul", .8)))
			if test.name == "missing" {
				reply.Delete("model")
			}
			raw := []byte(mustDump(t, reply))
			closed := make(chan struct{})
			var requests atomic.Int32
			client := testClient(context.Background(), roundTripper(func(request *http.Request) (*http.Response, error) {
				if requests.Add(1) != 1 {
					return nil, errors.New("unexpected invalid-model retry")
				}
				httpReply := response(request, http.StatusOK, raw)
				httpReply.Body = &observedBody{ReadCloser: httpReply.Body, closed: closed}
				return httpReply, nil
			}))
			metrics := json.NewObject("returned_model", "stale-model")
			answers, err := client.Ask(json.NewObject(), testQuestions(1), "fixture-key-only", metrics)
			if answers != nil {
				t.Fatal("invalid model returned answers")
			}
			if failure := errorReason(t, err, "malformed"); failure.Detail != "response model" {
				t.Fatalf("invalid model exposed unexpected detail: %q", failure.Detail)
			}
			if metrics.Value("returned_model") != nil {
				t.Fatal("invalid model retained a validated model")
			}
			if requests.Load() != 1 {
				t.Fatalf("transport requests=%d, want one", requests.Load())
			}
			select {
			case <-closed:
			default:
				t.Fatal("invalid model left its response body open")
			}
		})
	}
}
