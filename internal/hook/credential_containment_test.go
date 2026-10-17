package hook

import (
	"slices"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
)

func TestProviderRedactionRemovesContainingCredentials(t *testing.T) {
	for _, provider := range []string{"jev", "subq"} {
		t.Run(provider, func(t *testing.T) {
			setUp(t)
			short := "synthetic-key-common-0123456789"
			long := short + "-private-suffix"
			selected, other := "TYPESAFE_API_KEY", "SUBQ_API_KEY"
			load := jev.LoadKey
			if provider == "subq" {
				selected, other = other, selected
				load = subq.LoadKey
			}
			t.Setenv(selected, short)
			t.Setenv(other, long)
			if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
				"spec", json.NewObject("host", "claude", "review_provider", provider))); err != nil {
				t.Fatal(err)
			}
			ctx := makeCtx(t, ctxOptions{})
			ctx.Reviewer, _ = policy.Provider(provider)
			ctx.Metrics = json.NewObject()
			ask := askerWithKey(load, func(state, questions *json.Object, _ string, metrics *json.Object) (*json.Object, error) {
				if state.Value("observation") != evidence.Redacted || at(t, questions, "ok", "instructions") != evidence.Redacted {
					t.Error("review input retained part of a known credential")
				}
				metrics.Set("returned_metadata", long)
				return json.NewObject(), nil
			}, ctx.Metrics, decideAsk)
			state := json.NewObject("observation", long)
			if _, err := ask(state, json.NewObject("ok", json.NewObject("type", "noul", "instructions", long))); err != nil {
				t.Fatal(err)
			}
			if state.Value("observation") != evidence.Redacted || strings.Contains(dumps(t, ctx.Metrics), "-private-suffix") {
				t.Error("caller state or metrics retained part of a known credential")
			}
			if err := appendRecord(json.NewObject("session_id", ctx.SessionID, "tool_use_id", "check"), "claude", nil,
				gates.Outcome{Action: "blocked", Record: json.NewObject("gate", "test", "attributes", json.NewObject(long, "data"))}, &ctx, 4); err != nil {
				t.Fatal(err)
			}
			rows := entries(t, ctx.SessionID)
			if len(rows) != 1 {
				t.Fatalf("got %d signed test records, want one", len(rows))
			}
			if strings.Contains(dumps(t, rows[0].Value("attributes")), "-private-suffix") {
				t.Error("signed record identifiers retained part of a known credential")
			}
		})
	}
}

func TestContainedCredentialIdentifiersPreserveFieldsAndKeyOrder(t *testing.T) {
	for name, short := range map[string]string{"ascii": "synthetic-key-common-0123456789", "unicode": strings.Repeat("é", 16)} {
		long := short + "-private-suffix"
		for order, keys := range map[string][]string{"short_first": {short, long}, "long_first": {long, short}, "duplicates": {short, short, long, long}} {
			t.Run(name+"/"+order, func(t *testing.T) {
				original := slices.Clone(keys)
				value := json.NewObject(short, "short", long, "long", "<redacted identifier 1>", "original")
				before := dumps(t, value)
				clean, changed, err := redactKnownIdentifiers(value, keys)
				if err != nil || !changed {
					t.Fatalf("redaction changed=%t, error=%v", changed, err)
				}
				if clean.Len() != 3 || clean.Value("<redacted identifier 1>") != "original" {
					t.Fatal("redaction merged fields or replaced an existing marker")
				}
				if strings.Contains(dumps(t, clean), "-private-suffix") {
					t.Fatal("identifier retained part of a known credential")
				}
				if !slices.Equal(keys, original) || dumps(t, value) != before {
					t.Fatal("identifier redaction changed its caller's inputs")
				}
			})
		}
	}
}
