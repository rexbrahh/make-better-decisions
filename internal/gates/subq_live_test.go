package gates

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
)

func TestLiveSubQGateQualification(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_LIVE") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_LIVE=1 for paid Snap gate qualification")
	}
	key, err := subq.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	setUp(t)
	provider, _ := policy.Provider("subq")
	checked := func(t *testing.T, want []string, run func(Ask) Outcome) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), provider.Timeout)
		defer cancel()
		client := subq.NewClient(ctx)
		started := time.Now()
		out := run(func(state, questions *json.Object) (*json.Object, error) {
			return client.Ask(state, questions, key, nil)
		})
		t.Logf("action=%s elapsed=%s", out.Action, time.Since(started).Round(time.Millisecond))
		if !oneOf(want...).met(out) {
			t.Fatalf("expected %v: %s; %s", want, out.Message, recordOf(t, out))
		}
	}
	contextFor := func(t *testing.T, messages, observations []string) Context {
		ctx := qaContext(t, messages, observations, nil, "", "")
		ctx.Reviewer = provider
		return ctx
	}
	t.Run("observed_material_choice", func(t *testing.T) {
		ctx := contextFor(t, []string{"Preserve names such as Zoë and 李 exactly."},
			[]string{"ASCII cannot encode 李. UTF-8 encodes both Zoë and 李."})
		checked(t, []string{"advisory"}, func(ask Ask) Outcome {
			out := decide(t, qaRequest(t, "Which encoding preserves the required names?",
				opts("ascii", "ASCII encoding", "utf8", "UTF-8 encoding")), ctx, ask)
			if out.Action == "advisory" && (out.Record.Value("pick") != "utf8" || !matchesReviewIdentity(out.Record, ctx)) {
				t.Fatal("material receipt lost its pick or provider identity")
			}
			return out
		})
	})
	t.Run("missing_prerequisite", func(t *testing.T) {
		ctx := contextFor(t, []string{"Use a native file watcher supported by the deployment platform."}, nil)
		checked(t, []string{"gather_evidence"}, func(ask Ask) Outcome {
			return decide(t, qaRequest(t, "Which native file watcher matches the deployment platform?",
				opts("inotify", "Linux inotify", "fsevents", "macOS FSEvents")), ctx, ask)
		})
	})
	t.Run("omitted_requirement", func(t *testing.T) {
		ctx := contextFor(t, []string{"Use SQLite for local storage."}, nil)
		checked(t, []string{"refused"}, func(ask Ask) Outcome {
			return decide(t, qaRequest(t, "Which local storage engine?", opts("postgres", "PostgreSQL", "mysql", "MySQL")), ctx, ask)
		})
	})
	t.Run("routine_verification", func(t *testing.T) {
		ctx := contextFor(t, []string{"Run the existing cache tests now."}, nil)
		checked(t, []string{"routine"}, func(ask Ask) Outcome {
			return decide(t, qaRequest(t, "Run the existing cache tests now?", opts("run", "Run the tests", "skip", "Skip the tests")), ctx, ask)
		})
	})
	t.Run("exact_edit", func(t *testing.T) {
		ctx := contextFor(t, []string{"Write config.py with STORAGE = 'sqlite' as its only line."}, nil)
		checked(t, []string{"allow"}, func(ask Ask) Outcome {
			return editGate(t, "Write", json.NewObject("file_path", "config.py", "content", "STORAGE = 'sqlite'\n"), ctx, ask)
		})
	})
	t.Run("unconsulted_material_edit", func(t *testing.T) {
		ctx := contextFor(t, []string{"Implement persistent storage for this application."}, nil)
		checked(t, []string{"deny"}, func(ask Ask) Outcome {
			return editGate(t, "Write", json.NewObject("file_path", "store.py",
				"content", "import sqlite3\nconnection = sqlite3.connect('data.db')\n"), ctx, ask)
		})
	})
}
