package hook

import (
	stdcontext "context"
	"fmt"
	"strings"
	"sync"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
)

type reviewClient interface {
	Ask(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error)
}

var newSnapClient = func(ctx stdcontext.Context) reviewClient { return subq.NewClient(ctx) }

func reviewLabel(provider policy.ReviewProvider) string {
	if provider.Name == "subq" {
		return "SubQ Snap"
	}
	return "Jev"
}

// One client and deadline cover every batch and edit fragment of the operation.
func reviewAsker(ctx gates.Context, post jev.PostFunc, frame int) (gates.Ask, stdcontext.CancelFunc) {
	if ctx.Reviewer.Name != "subq" {
		return asker(post, ctx.Metrics, frame), func() {}
	}
	bounded, cancel := stdcontext.WithTimeout(stdcontext.Background(), ctx.Reviewer.Timeout)
	client := newSnapClient(bounded)
	base := askerWithKey(subq.LoadKey, client.Ask, ctx.Metrics, frame)
	var firstErr error
	var failed sync.Once
	return func(state, questions *json.Object) (*json.Object, error) {
		answers, err := base(state, questions)
		if err != nil {
			failed.Do(func() { firstErr = err; cancel() })
			return nil, firstErr
		}
		return answers, nil
	}, cancel
}

// Values were already scrubbed. Unique markers also protect credential-bearing keys without merging them.
func redactKnownIdentifiers(value *json.Object, keys []string) (*json.Object, bool, error) {
	text, err := json.Dumps(value, json.Options{})
	if err != nil {
		return nil, false, err
	}
	clean, next := text, 1
	for _, key := range keys {
		if pystr.Len(key) < 16 {
			continue
		}
		quoted, err := json.Dumps(key, json.Options{})
		if err != nil {
			return nil, false, err
		}
		literal := quoted[1 : len(quoted)-1]
		if !strings.Contains(clean, literal) {
			continue
		}
		marker := fmt.Sprintf("<redacted identifier %d>", next)
		for strings.Contains(text, marker) || strings.Contains(clean, marker) {
			next++
			marker = fmt.Sprintf("<redacted identifier %d>", next)
		}
		clean = strings.ReplaceAll(clean, literal, marker)
		next++
	}
	if clean == text {
		return value, false, nil
	}
	decoded, err := json.Loads(clean)
	if err != nil {
		return nil, true, err
	}
	return decoded.(*json.Object), true, nil
}
