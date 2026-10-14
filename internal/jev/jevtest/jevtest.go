// Package jevtest holds the fake Jev transport that the jev and gates tests share.
//
// py: tests/helpers.py FakeJev
package jevtest

import (
	"sync"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// Fake stands in for the HTTP transport. A noul answers its override or 0.0; a choice answers its override
// or its first criteria key, with probability 1. Gates ask in parallel, so Post is safe for concurrent use.
//
// py: tests/helpers.py FakeJev
type Fake struct {
	Answers map[string]any // question id to the noul value or the chosen criteria key
	Raises  error          // returned after the request is recorded
	Raw     []byte         // returned in place of generated answers

	mu       sync.Mutex
	requests []*json.Object
}

// Post records the request and answers it. Its signature is jev.PostFunc.
func (f *Fake) Post(body []byte, key string, timeout time.Duration) ([]byte, error) {
	value, err := json.Loads(string(body))
	if err != nil {
		return nil, err
	}
	request := value.(*json.Object)
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if f.Raises != nil {
		return nil, f.Raises
	}
	if f.Raw != nil {
		return f.Raw, nil
	}
	questions := request.Value("questions").(*json.Object)
	answers := &json.Object{}
	for _, id := range questions.Keys() {
		question := questions.Value(id).(*json.Object)
		want, ok := f.Answers[id]
		if question.Value("type") == "noul" {
			if !ok {
				want = 0.0
			}
			answers.Set(id, json.NewObject("type", "noul", "noul", want))
			continue
		}
		options := question.Value("criteria").(*json.Object).Keys()
		if !ok {
			want = options[0]
		}
		probabilities := &json.Object{}
		for _, option := range options {
			p := 0.0
			if option == want {
				p = 1.0
			}
			probabilities.Set(option, p)
		}
		answers.Set(id, json.NewObject("type", "choice", "choice", want, "confidence", 1.0,
			"probabilities", probabilities))
	}
	text, err := json.Dumps(json.NewObject("model", "jev-1.13.0", "answers", answers, "usage", &json.Object{}),
		json.Options{})
	return []byte(text), err
}

// Requests returns the decoded request bodies in the order they arrived.
func (f *Fake) Requests() []*json.Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*json.Object(nil), f.requests...)
}
